package web

import (
	"archive/zip"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/net/websocket"

	"github.com/sshivamanand/bit-flow/internal/secure"
)

// The room: every browser tab that opens this computer's page joins over a
// WebSocket, shows up in everyone's list, and gets its own pairing code.
// To send, you pick a device and type the code shown on that device's screen.
//
// The WebSocket carries only presence, requests and progress. File data goes
// through ordinary HTTP: the sender PUTs each file and the receiver GETs one
// download that this server stitches together (a zip when there are several
// files), copying bytes straight across with no temporary file. That keeps
// it working for 100 GB on a phone: plain-HTTP pages can't stream data
// received over a WebSocket to disk, but the browser's own downloader can.

const (
	roomMaxMembers   = 64
	roomPairFailures = 5
	roomIdle         = 75 * time.Second // clients ping every 25 s
	roomGrace        = 30 * time.Second // keep a member this long after its tab disconnects (phone screen lock)
	roomOfferTTL     = 5 * time.Minute
	roomPipeWait     = 2 * time.Minute
	roomKeepDone     = 2 * time.Minute
)

// roomPenalty slows down wrong pairing codes (a variable so tests can shorten it).
var roomPenalty = time.Second

type room struct {
	mu      sync.Mutex
	members map[string]*member // by id
	byKey   map[string]*member // by secret key (HTTP authentication)
	xfers   map[string]*rxfer
	onEvent func(string) // optional log of finished transfers
}

type member struct {
	id, key, name, kind string
	code                string
	fails               int
	paired              map[string]bool // ids that may send to this member without a code
	out                 chan any
	online              bool
	gone                *time.Timer
	ws                  *websocket.Conn
	host                bool              // the computer running bit-flow (joined in-process)
	check               func(string) bool // non-nil: codes are checked here instead (the host's pairing code)
}

type rfile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type rxfer struct {
	id, from, to string
	files        []rfile
	total        int64
	state        string // offered, accepted, done, declined, cancelled, failed
	started      bool   // the receiver's download is running (only one may)
	ctx          context.Context
	cancel       context.CancelCauseFunc
	pipes        chan pipe
	moved        atomic.Int64
	accepted     chan struct{}
	acceptOnce   sync.Once
}

type pipe struct {
	idx  int
	body io.Reader
	done chan error
}

// roomMsg is every message in both directions; unused fields are omitted.
type roomMsg struct {
	Type     string       `json:"type"`
	Name     string       `json:"name,omitempty"`
	Key      string       `json:"key,omitempty"`
	ID       string       `json:"id,omitempty"`
	Code     string       `json:"code,omitempty"`
	Kind     string       `json:"kind,omitempty"`
	To       string       `json:"to,omitempty"`
	From     string       `json:"from,omitempty"`
	FromName string       `json:"fromName,omitempty"`
	FromKind string       `json:"fromKind,omitempty"`
	Xfer     string       `json:"xfer,omitempty"`
	Files    []rfile      `json:"files,omitempty"`
	Total    int64        `json:"total,omitempty"`
	Bytes    int64        `json:"bytes,omitempty"`
	Error    string       `json:"error,omitempty"`
	Members  []rosterItem `json:"members,omitempty"`
}

type rosterItem struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Paired bool   `json:"paired"` // you can send to them without a code
	Online bool   `json:"online"`
	Host   bool   `json:"host,omitempty"` // the computer running bit-flow
}

func newRoom() *room {
	return &room{members: map[string]*member{}, byKey: map[string]*member{}, xfers: map[string]*rxfer{}}
}

// ---- WebSocket ----

// wsHandler only accepts connections from this page (same Origin host), so
// other websites can't join the room from a visitor's browser.
func (rm *room) wsHandler() http.Handler {
	return websocket.Server{
		Handshake: func(cfg *websocket.Config, r *http.Request) error {
			o, err := url.Parse(r.Header.Get("Origin"))
			if err != nil || o.Host != r.Host {
				return errors.New("cross-origin websocket refused")
			}
			return nil
		},
		Handler: rm.serveWS,
	}
}

func (rm *room) serveWS(ws *websocket.Conn) {
	defer ws.Close()
	ws.MaxPayloadBytes = 4 << 20 // an offer of 10,000 files fits; nothing else comes close
	_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))
	var hello roomMsg
	if websocket.JSON.Receive(ws, &hello) != nil || hello.Type != "hello" {
		return
	}
	m, out, err := rm.join(ws, hello, ws.Request())
	if err != nil {
		_ = websocket.JSON.Send(ws, roomMsg{Type: "error", Error: err.Error()})
		return
	}
	writerDone := make(chan struct{})
	go func() { // writer: one goroutine owns all sends to this socket
		defer close(writerDone)
		for v := range out { // this connection's channel; closed when it's replaced or leaves
			_ = ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
			if websocket.JSON.Send(ws, v) != nil {
				ws.Close()
				return
			}
		}
	}()
	rm.mu.Lock()
	rm.send(m, roomMsg{Type: "welcome", ID: m.id, Key: m.key, Code: m.code, Kind: m.kind, Name: m.name})
	rm.mu.Unlock()
	rm.broadcastRoster()

	for {
		_ = ws.SetReadDeadline(time.Now().Add(roomIdle))
		var msg roomMsg
		if err := websocket.JSON.Receive(ws, &msg); err != nil {
			break
		}
		rm.handle(m, msg)
	}
	rm.leave(m, ws)
	<-writerDone
}

// join adds a member, or reattaches one whose tab reconnected (same key).
func (rm *room) join(ws *websocket.Conn, hello roomMsg, r *http.Request) (*member, chan any, error) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if m := rm.byKey[hello.Key]; hello.Key != "" && m != nil {
		if m.gone != nil {
			m.gone.Stop()
			m.gone = nil
		}
		if m.online {
			// An older tab with the same key loses the seat: stop its writer
			// and close its socket so its handler returns.
			close(m.out)
			m.ws.Close()
		}
		m.online, m.ws = true, ws
		m.out = make(chan any, 64)
		if n := cleanName(hello.Name); n != "" {
			m.name = rm.uniqueName(m, n)
		}
		return m, m.out, nil
	}
	if len(rm.members) >= roomMaxMembers {
		return nil, nil, errors.New("the room is full")
	}
	m := &member{
		id: randomID(6), key: randomID(16), code: secure.NewCode(),
		name: cleanName(hello.Name), kind: deviceKind(r.UserAgent()),
		paired: map[string]bool{}, out: make(chan any, 64), online: true, ws: ws,
	}
	if m.kind == "Mac" && hello.Kind == "iPad" {
		m.kind = "iPad" // iPadOS presents itself as a Mac; the page can tell from touch support
	}
	if m.name == "" {
		m.name = m.kind
	}
	m.name = rm.uniqueName(m, m.name)
	rm.members[m.id], rm.byKey[m.key] = m, m
	return m, m.out, nil
}

// uniqueName returns name, or "name 2", "name 3"… if someone else has it:
// two phones both called "iPhone" can't be told apart otherwise. Caller holds rm.mu.
func (rm *room) uniqueName(m *member, name string) string {
	taken := func(n string) bool {
		for _, o := range rm.members {
			if o != m && strings.EqualFold(o.name, n) {
				return true
			}
		}
		return false
	}
	for i, n := 2, name; ; i++ {
		if !taken(n) {
			return n
		}
		n = fmt.Sprintf("%s %d", name, i)
	}
}

// leave marks a member offline; it's removed after a grace period unless it reconnects.
func (rm *room) leave(m *member, ws *websocket.Conn) {
	rm.mu.Lock()
	if m.ws == ws {
		m.online, m.ws = false, nil
		close(m.out)
		m.gone = time.AfterFunc(roomGrace, func() { rm.remove(m) })
	}
	rm.mu.Unlock()
	rm.broadcastRoster()
}

func (rm *room) remove(m *member) {
	rm.mu.Lock()
	if m.online {
		rm.mu.Unlock()
		return
	}
	delete(rm.members, m.id)
	delete(rm.byKey, m.key)
	var dead []*rxfer
	for _, x := range rm.xfers {
		if (x.from == m.id || x.to == m.id) && (x.state == "offered" || x.state == "accepted") {
			dead = append(dead, x)
		}
	}
	rm.mu.Unlock()
	for _, x := range dead {
		rm.finish(x, "failed", m.name+" left")
	}
	rm.broadcastRoster()
}

// send queues a message for m (dropped if its connection is gone or stuck).
// The caller holds rm.mu: m.out is replaced and closed only under it.
func (rm *room) send(m *member, v roomMsg) {
	if !m.online {
		return
	}
	select {
	case m.out <- v:
	default:
	}
}

func (rm *room) sendTo(id string, v roomMsg) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if m := rm.members[id]; m != nil {
		rm.send(m, v)
	}
}

func (rm *room) broadcastRoster() {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	for _, viewer := range rm.members {
		var list []rosterItem
		for _, other := range rm.members {
			if other.id == viewer.id {
				continue
			}
			list = append(list, rosterItem{ID: other.id, Name: other.name, Kind: other.kind,
				Paired: other.paired[viewer.id], Online: other.online, Host: other.host})
		}
		if list == nil {
			list = []rosterItem{}
		}
		rm.send(viewer, roomMsg{Type: "roster", Members: list})
	}
}

func (rm *room) handle(m *member, msg roomMsg) {
	switch msg.Type {
	case "ping":
		rm.sendTo(m.id, roomMsg{Type: "pong"})
	case "rename":
		if n := cleanName(msg.Name); n != "" {
			rm.mu.Lock()
			m.name = rm.uniqueName(m, n)
			rm.send(m, roomMsg{Type: "name", Name: m.name}) // it may have gained a number
			rm.mu.Unlock()
			rm.broadcastRoster()
		}
	case "offer":
		rm.offer(m, msg)
	case "decline", "cancel":
		rm.mu.Lock()
		x := rm.xfers[msg.Xfer]
		ok := x != nil && (x.to == m.id || x.from == m.id) && (x.state == "offered" || x.state == "accepted")
		rm.mu.Unlock()
		if ok {
			state, why := "cancelled", m.name+" cancelled"
			if msg.Type == "decline" && x.to == m.id {
				state, why = "declined", m.name+" declined"
			}
			rm.finish(x, state, why)
		}
	}
}

// offer handles an offer from a browser tab.
func (rm *room) offer(from *member, msg roomMsg) {
	if _, err := rm.makeOffer(from, msg.To, msg.Code, msg.Files); err != nil {
		rm.sendTo(from.id, roomMsg{Type: "offer-error", To: msg.To, Error: err.Error()})
	}
}

// makeOffer checks the pairing code (unless already paired) and asks the receiver.
func (rm *room) makeOffer(from *member, toID, rawCode string, in []rfile) (*rxfer, error) {
	var total int64
	files := make([]rfile, 0, len(in))
	for _, f := range in {
		name := cleanPath(f.Name)
		if name == "" || f.Size < 0 {
			return nil, errors.New("bad file name")
		}
		files = append(files, rfile{Name: name, Size: f.Size})
		total += f.Size
	}
	if len(files) == 0 || len(files) > 10000 {
		return nil, errors.New("pick between 1 and 10,000 files")
	}

	rm.mu.Lock()
	to := rm.members[toID]
	switch {
	case to == nil || !to.online:
		rm.mu.Unlock()
		return nil, errors.New("that device isn't here any more")
	case to.id == from.id:
		rm.mu.Unlock()
		return nil, errors.New("that's you")
	}
	if !to.paired[from.id] {
		code := secure.NormalizeCode(rawCode)
		if to.check != nil {
			// The host's own pairing code: its checker rate-limits and rotates
			// exactly like computer pairing, and may sleep, so don't hold the lock.
			check, name := to.check, to.name
			rm.mu.Unlock()
			if !check(code) {
				return nil, errors.New("wrong code — use the pairing code shown on " + name)
			}
			rm.mu.Lock()
			if to = rm.members[toID]; to == nil || !to.online {
				rm.mu.Unlock()
				return nil, errors.New("that device isn't here any more")
			}
		} else if subtle.ConstantTimeCompare([]byte(code), []byte(to.code)) != 1 {
			to.fails++
			if to.fails >= roomPairFailures {
				to.code, to.fails = secure.NewCode(), 0
				for id := range to.paired {
					delete(to.paired, id) // a new code means everyone pairs again
				}
				rm.send(to, roomMsg{Type: "code", Code: to.code})
			}
			name := to.name
			rm.mu.Unlock()
			time.Sleep(roomPenalty)
			return nil, errors.New("wrong code — use the code shown on " + name)
		} else {
			to.fails = 0
		}
		to.paired[from.id] = true
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	x := &rxfer{id: randomID(9), from: from.id, to: to.id, files: files, total: total, state: "offered",
		ctx: ctx, cancel: cancel, pipes: make(chan pipe), accepted: make(chan struct{})}
	rm.xfers[x.id] = x
	rm.send(to, roomMsg{Type: "incoming", Xfer: x.id, From: from.id, FromName: from.name, FromKind: from.kind, Files: files, Total: total})
	rm.send(from, roomMsg{Type: "offered", Xfer: x.id, To: to.id, Files: files, Total: total})
	rm.mu.Unlock()
	rm.broadcastRoster()

	time.AfterFunc(roomOfferTTL, func() {
		rm.mu.Lock()
		stale := x.state == "offered"
		rm.mu.Unlock()
		if stale {
			rm.finish(x, "failed", "no answer")
		}
	})
	return x, nil
}

// finish ends a transfer once and tells both sides.
func (rm *room) finish(x *rxfer, state, why string) {
	rm.mu.Lock()
	if x.state != "offered" && x.state != "accepted" {
		rm.mu.Unlock()
		return
	}
	x.state = state
	rm.mu.Unlock()
	x.cancel(errors.New(why))
	msg := roomMsg{Type: state, Xfer: x.id, Error: why, Bytes: x.moved.Load(), Total: x.total}
	rm.sendTo(x.from, msg)
	rm.sendTo(x.to, msg)
	if state == "done" && rm.onEvent != nil {
		rm.onEvent(fmt.Sprintf("%s → %s: %d files, %d bytes", rm.nameOf(x.from), rm.nameOf(x.to), len(x.files), x.total))
	}
	time.AfterFunc(roomKeepDone, func() {
		rm.mu.Lock()
		delete(rm.xfers, x.id)
		rm.mu.Unlock()
	})
}

func (rm *room) nameOf(id string) string {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if m := rm.members[id]; m != nil {
		return m.name
	}
	return "?"
}

func (rm *room) lookup(r *http.Request, id string) (*member, *rxfer) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return rm.byKey[r.URL.Query().Get("k")], rm.xfers[id]
}

// ---- data: receiver ----

// download is the receiver's side, and opening it is what accepts the offer:
// the accept button starts the download inside the click, which browsers
// (iOS Safari included) treat as user-initiated.
func (rm *room) download(w http.ResponseWriter, r *http.Request) {
	m, x := rm.lookup(r, r.PathValue("id"))
	if m == nil || x == nil || x.to != m.id {
		http.NotFound(w, r)
		return
	}
	if !rm.accept(m, x) {
		http.Error(w, "this transfer is no longer waiting", http.StatusConflict)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	var zw *zip.Writer
	if len(x.files) == 1 {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", contentDisposition(path.Base(x.files[0].Name)))
		w.Header().Set("Content-Length", fmt.Sprint(x.files[0].Size))
	} else {
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", contentDisposition(cleanFile(rm.nameOf(x.from))+" files.zip"))
		zw = zip.NewWriter(w)
	}
	w.WriteHeader(http.StatusOK)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush() // show the download right away; data follows as the sender uploads
	}

	// A cancel from either side must stop the relay even while it's blocked
	// writing to a stalled download: a past write deadline makes Write return.
	rc := http.NewResponseController(w)
	stop := context.AfterFunc(x.ctx, func() { _ = rc.SetWriteDeadline(time.Now()) })
	err := rm.relay(r.Context(), x, func(f rfile) (io.Writer, func(error) error, error) {
		if zw == nil {
			return w, nil, nil
		}
		hdr := &zip.FileHeader{Name: f.Name, Method: zip.Store, Modified: time.Now()}
		hdr.UncompressedSize64 = uint64(f.Size)
		zf, err := zw.CreateHeader(hdr)
		return zf, nil, err
	})
	if err == nil && zw != nil {
		err = zw.Close()
	}
	stop()
	if err != nil {
		rm.finish(x, "failed", err.Error())
		return
	}
	rm.finish(x, "done", "")
}

// accept marks x accepted by m and starts it (once). It reports false if x
// isn't waiting for m any more, or its data is already flowing.
func (rm *room) accept(m *member, x *rxfer) bool {
	rm.mu.Lock()
	if x.state == "offered" {
		x.state = "accepted"
		if from := rm.members[x.from]; from != nil {
			from.paired[m.id] = true // accepting someone means you can send back without a code
		}
	}
	ok := x.to == m.id && x.state == "accepted" && !x.started
	x.started = x.started || ok
	rm.mu.Unlock()
	if ok {
		x.acceptOnce.Do(func() {
			close(x.accepted)
			rm.sendTo(x.from, roomMsg{Type: "accepted", Xfer: x.id})
			go rm.progress(x)
			rm.broadcastRoster()
		})
	}
	return ok
}

// relay copies each file the sender uploads, in order, to where open says.
// done, if non-nil, is told how that file went and may fail it.
func (rm *room) relay(ctx context.Context, x *rxfer, open func(rfile) (dst io.Writer, done func(error) error, err error)) error {
	for i, f := range x.files {
		var p pipe
		select {
		case p = <-x.pipes:
		case <-x.ctx.Done():
			return context.Cause(x.ctx)
		case <-ctx.Done():
			return errors.New("the download was stopped")
		case <-time.After(roomPipeWait):
			return errors.New("the sender stopped sending")
		}
		if p.idx != i {
			p.done <- errors.New("files out of order")
			return errors.New("files arrived out of order")
		}
		dst, done, err := open(f)
		if err != nil {
			p.done <- err
			return err
		}
		// Never pass on more than was offered: extra bytes would corrupt the zip.
		n, err := io.Copy(dst, countingReader{io.LimitReader(p.body, f.Size), &x.moved})
		if err == nil && n != f.Size {
			err = fmt.Errorf("%s: got %d of %d bytes", f.Name, n, f.Size)
		}
		if done != nil {
			err = done(err)
		}
		p.done <- err
		if err != nil {
			return err
		}
	}
	return nil
}

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// progress reports bytes relayed to both sides until the transfer ends.
func (rm *room) progress(x *rxfer) {
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-x.ctx.Done():
			return
		case <-t.C:
			msg := roomMsg{Type: "progress", Xfer: x.id, Bytes: x.moved.Load(), Total: x.total}
			rm.sendTo(x.from, msg)
			rm.sendTo(x.to, msg)
		}
	}
}

// ---- data: sender ----

// upload is the sender's side: PUT /room/up/{id}/{i} with file i as the body.
func (rm *room) upload(w http.ResponseWriter, r *http.Request) {
	// Every refusal drops the rest of the body: otherwise the server tries to
	// drain it after we return and waits on a sender that's still streaming.
	rc := http.NewResponseController(w)
	refuse := func(msg string, code int) {
		_ = rc.SetReadDeadline(time.Now())
		http.Error(w, msg, code)
	}
	m, x := rm.lookup(r, r.PathValue("id"))
	if m == nil || x == nil || x.from != m.id || r.Header.Get("X-Bitflow") != "1" {
		refuse("404 page not found", http.StatusNotFound)
		return
	}
	var i int
	if _, err := fmt.Sscan(r.PathValue("i"), &i); err != nil || i < 0 || i >= len(x.files) {
		refuse("bad file index", http.StatusBadRequest)
		return
	}
	select {
	case <-x.accepted:
	case <-x.ctx.Done():
		refuse(context.Cause(x.ctx).Error(), http.StatusConflict)
		return
	}
	p := pipe{idx: i, body: r.Body, done: make(chan error, 1)}
	select {
	case x.pipes <- p:
	case <-x.ctx.Done():
		refuse(context.Cause(x.ctx).Error(), http.StatusConflict)
		return
	case <-r.Context().Done():
		return
	case <-time.After(roomPipeWait):
		refuse("the receiver isn't downloading", http.StatusGatewayTimeout)
		return
	}
	// The relay reads r.Body, so this handler can't return before it's done
	// with it. On cancel, a past read deadline unblocks that read.
	var err error
	select {
	case err = <-p.done:
	case <-x.ctx.Done():
		_ = rc.SetReadDeadline(time.Now())
		err = <-p.done // nil if the file got through before the cancel landed
	}
	if err != nil {
		refuse(err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// ---- helpers ----

func cleanName(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, s))
	for utf8.RuneCountInString(s) > 40 {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// cleanPath keeps a sender's relative path (folders from desktop browsers)
// but drops anything that could climb out of a zip.
func cleanPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	var parts []string
	for _, part := range strings.Split(p, "/") {
		part = cleanFile(part)
		if part != "" && part != "." && part != ".." {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "/")
}

func cleanFile(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"|?*`, r) {
			return '_'
		}
		return r
	}, s))
}

// deviceKind names a device from its browser's User-Agent.
func deviceKind(ua string) string {
	switch {
	case strings.Contains(ua, "iPhone"):
		return "iPhone"
	case strings.Contains(ua, "iPad"):
		return "iPad"
	case strings.Contains(ua, "Android") && strings.Contains(ua, "Mobile"):
		return "Android phone"
	case strings.Contains(ua, "Android"):
		return "Android tablet"
	case strings.Contains(ua, "CrOS"):
		return "Chromebook"
	case strings.Contains(ua, "Macintosh"):
		return "Mac" // iPadOS also says Macintosh; the page corrects that from touch support
	case strings.Contains(ua, "Windows"):
		return "Windows PC"
	case strings.Contains(ua, "Linux"):
		return "Linux"
	}
	return "Browser"
}

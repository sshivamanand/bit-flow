// Package ui is bit-flow's desktop app: a long-running agent (receiver,
// discovery responder, sender) with a web interface served on 127.0.0.1 and
// opened in the user's browser. The browser is only the screen; all transfer
// work, file access and native dialogs happen in this process.
package ui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sshivamanand/bit-flow/internal/advisor"
	"github.com/sshivamanand/bit-flow/internal/awake"
	"github.com/sshivamanand/bit-flow/internal/config"
	"github.com/sshivamanand/bit-flow/internal/discovery"
	"github.com/sshivamanand/bit-flow/internal/history"
	"github.com/sshivamanand/bit-flow/internal/netinfo"
	"github.com/sshivamanand/bit-flow/internal/planner"
	"github.com/sshivamanand/bit-flow/internal/proto"
	"github.com/sshivamanand/bit-flow/internal/secure"
	"github.com/sshivamanand/bit-flow/internal/steam"
	"github.com/sshivamanand/bit-flow/internal/store"
	"github.com/sshivamanand/bit-flow/internal/transfer"
	"github.com/sshivamanand/bit-flow/internal/web"
)

// offerTimeout declines offers nobody answers.
const offerTimeout = 10 * time.Minute

// App is the agent behind the UI.
type App struct {
	cfg      *config.Config
	recv     *transfer.Receiver
	port     int
	version  string
	stateDir string
	dir      string // config dir: sessions, history
	sessions *sessionStore
	ctx      context.Context

	mu        sync.Mutex
	transfers []*xfer // newest first
	offers    []*offer
	incoming  *xfer
	peers     []discovery.Peer
	scanning  bool
	lastScan  time.Time
	addrs     []Addr // cached: listing interfaces runs a command on macOS
	addrsAt   time.Time
	phoneURL  string // browser address on the LAN
	phoneQR   string // SVG

	room      *web.Local       // this computer in the browser room
	uploads   history.Batches  // browser uploads, grouped per folder or run of files
	batchRows map[string]*xfer // batch id → its row
}

// xfer is one transfer as the UI shows it.
type xfer struct {
	ID       string    `json:"id"`
	Dir      string    `json:"dir"` // "in" or "out"
	Peer     string    `json:"peer"`
	Name     string    `json:"name"`
	Files    int       `json:"files"`
	Total    int64     `json:"total"`
	Done     int64     `json:"done"`
	Already  int64     `json:"already"`
	Rate     float64   `json:"rate"`
	State    string    `json:"state"` // waiting, running, done, failed, declined, cancelled
	Error    string    `json:"error,omitempty"`
	Dest     string    `json:"dest,omitempty"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`
	Retrying string    `json:"retrying,omitempty"`
	Streams  int       `json:"streams,omitempty"`
	Saved    int64     `json:"saved,omitempty"` // bytes compression kept off the wire
	Link     string    `json:"link,omitempty"`  // e.g. "Wi-Fi → Ethernet" or "Wi-Fi + Thunderbolt Bridge"
	Tips     []string  `json:"tips,omitempty"`
	Items    []item    `json:"items,omitempty"` // browser uploads: the files in this row
	More     int       `json:"more,omitempty"`  // files beyond the itemsLimit kept in Items

	progress func() (done int64, accepted bool)
	refresh  func(x *xfer) // optional: live details, called with a.mu held
	cancel   func()
	release  func() // keep-awake hold
	lastDone int64
	lastT    time.Time
}

// item is one file of a grouped browser upload, relative to the row's folder.
type item struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// itemsLimit bounds the file list kept per row (it's saved with the history).
const itemsLimit = 1000

// offer is an incoming transfer waiting for the user's decision.
type offer struct {
	ID      string            `json:"id"`
	From    string            `json:"from"`
	Name    string            `json:"name"`
	Top     []string          `json:"top"`
	Files   int               `json:"files"`
	Total   int64             `json:"total"`
	Dest    string            `json:"dest"`
	Already int64             `json:"already"`
	Skipped int               `json:"skipped"`
	Resumed bool              `json:"resumed"`
	Renames map[string]string `json:"renames,omitempty"`
	Raw     bool              `json:"raw"`
	Steam   string            `json:"steam,omitempty"`    // game name, for Steam offers
	Library string            `json:"library,omitempty"`  // Steam library it installs into
	OtherOS string            `json:"other_os,omitempty"` // set when the game was built for another OS
	Via     string            `json:"via,omitempty"`      // "room": a phone or browser, through the room

	decide chan transfer.Decision
}

func newID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// NewApp loads the config and prepares the receiver. Start begins listening.
func NewApp(version string) (*App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	a := &App{cfg: cfg, version: version}
	if dir, err := config.Dir(); err == nil {
		a.dir = dir
		a.sessions = loadSessions(dir)
		a.transfers = loadHistory(dir)
	} else {
		a.sessions = &sessionStore{m: map[string]*Session{}}
	}
	if dir, err := os.UserCacheDir(); err == nil {
		a.stateDir = dir + string(os.PathSeparator) + "bitflow"
	}
	policy, _ := store.ParsePolicy(cfg.Conflict)
	a.recv = &transfer.Receiver{Dest: cfg.ReceiveDir(), Policy: policy, StateDir: a.stateDir}
	a.recv.Confirm = a.confirm
	a.recv.OnStart = a.onStart
	a.recv.OnDone = a.onDone
	return a, nil
}

// Start listens for transfers on port (or any free port if it's taken) and
// answers discovery until ctx is cancelled.
func (a *App) Start(ctx context.Context, port int) error {
	a.ctx = ctx
	if err := os.MkdirAll(a.cfg.ReceiveDir(), 0o755); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		// Another bit-flow (e.g. `bitflow recv`) has the usual port. Discovery
		// tells peers the real one, so any port works.
		if ln, err = net.Listen("tcp", ":0"); err != nil {
			return err
		}
	}
	a.port = ln.Addr().(*net.TCPAddr).Port

	// One port for bit-flow peers and browsers (phones, /get downloads).
	peers, browsers := web.Split(ln)
	webSrv := web.NewServer(web.Options{
		Name: a.DeviceName, CheckCode: a.recv.CheckCode, Binaries: web.FindBinaries,
		UploadDir: func() string {
			a.mu.Lock()
			defer a.mu.Unlock()
			return a.cfg.ReceiveDir()
		},
		OnUpload: a.onBrowserUpload,
	})
	a.room = webSrv.JoinRoom(a.cfg.DeviceName(), roomKind(runtime.GOOS), a.recv.CheckCode)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: webSrv}
	go srv.Serve(browsers)
	go func() {
		<-ctx.Done()
		_ = srv.Close()
		a.uploads.Flush()
	}()
	for _, ad := range a.localAddrs() {
		if ip := net.ParseIP(ad.IP); ip != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			a.phoneURL = fmt.Sprintf("http://%s:%d", ad.IP, a.port)
			a.phoneQR = web.SVGQR(a.phoneURL)
			break
		}
	}

	info := func() discovery.Info {
		return discovery.Info{Proto: proto.Version, ID: a.cfg.ID, Name: a.cfg.DeviceName(), OS: runtime.GOOS,
			Port: a.port, Role: discovery.RoleReceive}
	}
	a.recv.Probe = func() any { return info() }
	go discovery.Respond(ctx, proto.DefaultPort, info)
	go a.recv.Serve(ctx, peers, false)
	go a.tick(ctx)
	return nil
}

// tick refreshes live progress and rates.
func (a *App) tick(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			a.mu.Lock()
			for _, x := range a.transfers {
				if x.progress == nil || (x.State != "running" && x.State != "waiting") {
					continue
				}
				done, accepted := x.progress()
				if accepted && x.State == "waiting" {
					x.State, x.lastDone, x.lastT = "running", done, now
				}
				if x.refresh != nil {
					x.refresh(x)
				}
				if x.State == "running" {
					if dt := now.Sub(x.lastT).Seconds(); dt > 0 && !x.lastT.IsZero() {
						inst := float64(done-x.lastDone) / dt
						if x.Rate == 0 {
							x.Rate = inst
						} else {
							x.Rate = 0.3*inst + 0.7*x.Rate
						}
					}
					x.Done, x.lastDone, x.lastT = done, done, now
				}
			}
			a.mu.Unlock()
		}
	}
}

// ---- receiving ----

func (a *App) confirm(o *proto.Offer, info transfer.OfferInfo) transfer.Decision {
	if o.Bench {
		return transfer.Decision{Accept: true}
	}
	of := &offer{
		ID: newID(), From: o.From, Total: o.Total, Dest: info.Dest, Already: info.Already,
		Skipped: info.Skipped, Resumed: info.Resumed, Renames: info.Renames, Raw: info.Raw,
		decide: make(chan transfer.Decision, 1),
	}
	of.Top, of.Files = topLevel(o.Files)
	of.Name = summarize(of.Top)
	if o.Steam != nil {
		of.Name, of.Steam, of.Library = o.Steam.Name, o.Steam.Name, info.SteamLibrary
		if o.Steam.OS != runtime.GOOS {
			of.OtherOS = o.Steam.OS
		}
	}
	a.mu.Lock()
	a.offers = append(a.offers, of)
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		for i, x := range a.offers {
			if x == of {
				a.offers = append(a.offers[:i], a.offers[i+1:]...)
				break
			}
		}
		a.mu.Unlock()
	}()
	select {
	case d := <-of.decide:
		return d
	case <-time.After(offerTimeout):
		return transfer.Decision{}
	case <-a.ctx.Done():
		return transfer.Decision{}
	}
}

// Decide answers a pending offer.
func (a *App) Decide(id string, accept bool, dest string) error {
	if a.decideRoom(id, accept, dest) {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, of := range a.offers {
		if of.ID == id {
			select {
			case of.decide <- transfer.Decision{Accept: accept, Dest: dest}:
			default:
			}
			return nil
		}
	}
	return errors.New("that offer is no longer waiting")
}

func (a *App) onStart(o *proto.Offer, info transfer.OfferInfo) {
	if o.Bench {
		return
	}
	top, files := topLevel(o.Files)
	name := summarize(top)
	if o.Steam != nil {
		name = o.Steam.Name + " (steam)"
	}
	x := &xfer{
		ID: newID(), Dir: "in", Peer: o.From, Name: name, Files: files, Total: o.Total,
		Done: info.Already, Already: info.Already, State: "running", Dest: info.Dest, Started: time.Now(),
		lastDone: info.Already, lastT: time.Now(),
		cancel: a.recv.CancelActive, release: awake.Hold(),
	}
	// Called by tick with a.mu held.
	x.progress = func() (int64, bool) {
		if st := a.recv.Stats(); st.Active {
			return st.Received, true
		}
		return x.Done, true
	}
	a.mu.Lock()
	a.incoming = x
	a.transfers = append([]*xfer{x}, a.transfers...)
	a.mu.Unlock()
}

func (a *App) onDone(o *proto.Offer, res transfer.Result, err error) {
	if o.Bench {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	x := a.incoming
	a.incoming = nil
	if x == nil {
		// Declined or refused before it started: keep a record anyway.
		top, files := topLevel(o.Files)
		name := summarize(top)
		if o.Steam != nil {
			name = o.Steam.Name + " (steam)"
		}
		x = &xfer{ID: newID(), Dir: "in", Peer: o.From, Name: name, Files: files, Total: o.Total, Started: time.Now()}
		a.transfers = append([]*xfer{x}, a.transfers...)
	}
	if x.release != nil {
		x.release()
		x.release = nil
	}
	finish(x, res, err)
	a.saveHistoryLocked()
}

// logHistory appends a finished transfer to the shared history log.
func logHistory(x *xfer, res transfer.Result) {
	dir := "sent"
	if x.Dir == "in" {
		dir = "received"
	}
	_ = history.Append(history.Record{Time: time.Now(), Dir: dir, Peer: x.Peer, Name: x.Name, Files: x.Files,
		Bytes: res.Bytes, Already: res.Already, Seconds: x.Finished.Sub(x.Started).Seconds(), State: x.State, Error: x.Error})
}

// onBrowserUpload records a file a phone or browser sent. Browsers upload
// file by file, so files from one sender are grouped into one row per
// top-level folder (or per run of loose files) instead of a row each.
func (a *App) onBrowserUpload(rel string, size int64, from string) {
	now := time.Now()
	id, isNew := a.uploads.Add(rel, size, from, now)
	folder := history.TopFolder(rel)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.batchRows == nil {
		a.batchRows = map[string]*xfer{}
	}
	x := a.batchRows[id]
	if isNew || x == nil {
		x = &xfer{ID: newID(), Dir: "in", Peer: from, State: "done", Started: now}
		a.transfers = append([]*xfer{x}, a.transfers...)
		for bid := range a.batchRows { // forget rows whose batch has closed
			if !a.uploads.Open(bid) {
				delete(a.batchRows, bid)
			}
		}
		a.batchRows[id] = x
	}
	x.Files++
	x.Total += size
	x.Done = x.Total
	x.Finished = now
	if secs := now.Sub(x.Started).Seconds(); secs > 0 {
		x.Rate = float64(x.Total) / secs
	}
	x.Name = history.BatchName(folder, path.Base(rel), x.Files)
	dest := a.cfg.ReceiveDir()
	switch {
	case folder != "":
		x.Dest = filepath.Join(dest, filepath.FromSlash(folder))
	case x.Files == 1:
		x.Dest = filepath.Join(dest, filepath.FromSlash(rel))
	default:
		x.Dest = dest
	}
	if len(x.Items) < itemsLimit {
		x.Items = append(x.Items, item{Name: strings.TrimPrefix(rel, folder+"/"), Size: size})
	} else {
		x.More++
	}
	a.saveHistoryLocked()
}

// Remove deletes one finished transfer from the list.
func (a *App) Remove(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, x := range a.transfers {
		if x.ID != id {
			continue
		}
		if x.State == "running" || x.State == "waiting" {
			return errors.New("stop that transfer first")
		}
		a.transfers = append(a.transfers[:i], a.transfers[i+1:]...)
		for bid, row := range a.batchRows {
			if row == x { // more files from that upload start a fresh row
				delete(a.batchRows, bid)
			}
		}
		a.saveHistoryLocked()
		return nil
	}
	return errors.New("that transfer isn't in the list")
}

// ClearHistory removes finished transfers from the list and deletes the
// transfer log the CLI shares (bitflow history).
func (a *App) ClearHistory() error {
	a.ClearFinished()
	return history.Clear()
}

// finish records a transfer's outcome. Caller holds a.mu.
func finish(x *xfer, res transfer.Result, err error) {
	defer logHistory(x, res)
	x.Finished = time.Now()
	x.Rate = 0
	x.cancel = nil
	switch {
	case err == nil:
		x.State, x.Done, x.Files = "done", x.Total, max(x.Files, res.Files)
		if secs := x.Finished.Sub(x.Started).Seconds(); secs > 0 {
			x.Rate = float64(res.Bytes) / secs // average, shown on finished rows
		}
	case transfer.IsDeclined(err):
		x.State = "declined"
	case errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "cancelled"):
		x.State, x.Error = "cancelled", err.Error()
	default:
		x.State, x.Error = "failed", err.Error()
	}
}

// ---- sending ----

// Games lists installed Steam games.
func (a *App) Games() []steam.Game { return steam.Games() }

// SendSteam sends an installed Steam game into the receiver's Steam library.
func (a *App) SendSteam(to, peerName, appID, code string) (string, error) {
	games, err := steam.Find(appID)
	if err != nil {
		return "", err
	}
	g := games[0]
	m, err := planner.BuildSteam(proto.SteamGame{AppID: g.AppID, Name: g.Name, InstallDir: g.InstallDir, OS: runtime.GOOS}, g.Dir, g.Manifest)
	if err != nil {
		return "", err
	}
	return a.send(to, peerName, m, g.Name+" (steam)", code, false)
}

// Send starts sending paths to addr (host:port) with the receiver's code.
func (a *App) Send(to, peerName string, paths []string, code string, raw bool) (string, error) {
	if len(paths) == 0 {
		return "", errors.New("pick something to send")
	}
	if id, ok := strings.CutPrefix(to, roomPrefix); ok {
		return a.sendRoom(id, peerName, paths, code)
	}
	m, err := planner.Build(paths)
	if err != nil {
		return "", err
	}
	top, _ := topLevel(m.Files)
	return a.send(to, peerName, m, summarize(top), code, raw)
}

func (a *App) send(to, peerName string, m *planner.Manifest, name, code string, raw bool) (string, error) {
	code = secure.NormalizeCode(code)
	if len(code) < 6 {
		return "", errors.New("enter the 6-digit pairing code shown on the other computer")
	}
	addr := to
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, fmt.Sprint(proto.DefaultPort))
	}
	if peerName == "" {
		peerName = to
	}
	ctx, cancel := context.WithCancel(a.ctx)
	x := &xfer{
		ID: newID(), Dir: "out", Peer: peerName, Name: name, Files: m.FileCount(),
		Total: m.Total, State: "waiting", Started: time.Now(), cancel: cancel,
	}
	s := transfer.NewSender(transfer.Options{
		Name: a.cfg.DeviceName(), Code: code, Raw: raw,
		OnRetry: func(attempt int, err error, wait time.Duration) {
			a.mu.Lock()
			x.Retrying = fmt.Sprintf("%v; reconnecting (attempt %d)…", err, attempt)
			a.mu.Unlock()
		},
	})
	x.progress = func() (int64, bool) {
		st := s.Stats()
		return st.Committed, st.Accepted
	}
	x.refresh = func(x *xfer) {
		st := s.Stats()
		if !st.Accepted {
			return
		}
		x.Streams, x.Saved = st.Streams, st.Saved
		r := st.Route
		links := []string{orIP(r.LocalLabel, r.LocalIP)}
		if len(st.Paths) > 1 {
			links = links[:0]
			for _, p := range st.Paths {
				links = append(links, p.Label)
			}
			x.Link = strings.Join(links, " + ")
		} else {
			x.Link = orIP(r.LocalLabel, r.LocalIP) + " → " + orIP(r.PeerLabel, r.PeerIP)
		}
		x.Tips = advisor.Tips(advisor.Link{Label: r.LocalLabel, IP: r.LocalIP}, advisor.Link{Label: r.PeerLabel, IP: r.PeerIP}, links)
	}
	a.mu.Lock()
	a.transfers = append([]*xfer{x}, a.transfers...)
	a.mu.Unlock()

	go func() {
		release := awake.Hold()
		defer release()
		res, err := s.Send(ctx, addr, m)
		cancel()
		a.mu.Lock()
		defer a.mu.Unlock()
		x.Retrying = ""
		x.refresh(x)
		if res.Already > 0 {
			x.Already = res.Already
		}
		finish(x, res, err)
		a.saveHistoryLocked()
	}()
	return x.ID, nil
}

// Cancel stops a running transfer.
func (a *App) Cancel(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, x := range a.transfers {
		if x.ID == id && x.cancel != nil {
			go x.cancel()
			return nil
		}
	}
	return errors.New("that transfer isn't running")
}

// ClearFinished removes finished transfers from the list.
func (a *App) ClearFinished() {
	a.mu.Lock()
	defer a.mu.Unlock()
	keep := a.transfers[:0]
	for _, x := range a.transfers {
		if x.State == "running" || x.State == "waiting" {
			keep = append(keep, x)
		}
	}
	a.transfers = keep
	a.batchRows = nil // more files from an open upload start a fresh row
	a.saveHistoryLocked()
}

// ---- discovery ----

// Scan refreshes the nearby-devices list in the background.
func (a *App) Scan() {
	a.mu.Lock()
	if a.scanning || time.Since(a.lastScan) < 3*time.Second {
		a.mu.Unlock()
		return
	}
	a.scanning = true
	a.mu.Unlock()
	go func() {
		peers, _ := discovery.Scan(a.ctx, discovery.ScanOptions{SelfID: a.cfg.ID, Wait: 1200 * time.Millisecond})
		var recv []discovery.Peer
		for _, p := range peers {
			if p.Role == discovery.RoleReceive {
				recv = append(recv, p)
			}
		}
		a.mu.Lock()
		a.peers, a.scanning, a.lastScan = recv, false, time.Now()
		a.mu.Unlock()
	}()
}

// SetDeviceName changes the name other devices see.
func (a *App) SetDeviceName(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.Name = strings.TrimSpace(name)
	if a.room != nil {
		a.room.SetName(a.cfg.DeviceName())
	}
	return a.cfg.Save()
}

// ---- settings ----

// Settings are the user-editable preferences.
type Settings struct {
	Name     string `json:"name"`
	Dest     string `json:"dest"`
	Conflict string `json:"conflict"`
}

// UpdateSettings applies and saves new preferences.
func (a *App) UpdateSettings(s Settings) error {
	policy, err := store.ParsePolicy(s.Conflict)
	if err != nil {
		return err
	}
	if s.Dest != "" {
		if st, err := os.Stat(s.Dest); err != nil || !st.IsDir() {
			return fmt.Errorf("%s is not a folder", s.Dest)
		}
	}
	a.mu.Lock()
	a.cfg.Name = strings.TrimSpace(s.Name)
	if s.Dest != "" {
		a.cfg.Dest = s.Dest
	}
	a.cfg.Conflict = policy.String()
	err = a.cfg.Save()
	dest := a.cfg.ReceiveDir()
	if a.room != nil {
		a.room.SetName(a.cfg.DeviceName())
	}
	a.mu.Unlock()
	a.recv.SetDefaults(dest, policy)
	return err
}

// ---- state ----

// State is everything the UI renders.
type State struct {
	Version   string           `json:"version"`
	Home      string           `json:"home"` // for shortening paths to ~/…
	Name      string           `json:"name"`
	OS        string           `json:"os"`
	Code      string           `json:"code"`
	Port      int              `json:"port"`
	Addrs     []Addr           `json:"addrs"`
	Settings  Settings         `json:"settings"`
	Offers    []*offer         `json:"offers"`
	Transfers []*xfer          `json:"transfers"`
	Peers     []discovery.Peer `json:"peers"`
	Scanning  bool             `json:"scanning"`
	PhoneURL  string           `json:"phone_url,omitempty"`
	PhoneQR   string           `json:"phone_qr,omitempty"`
	RoomPeers []RoomPeer       `json:"room_peers"`
}

// Addr is one of this device's network addresses.
type Addr struct {
	Label string `json:"label"`
	IP    string `json:"ip"`
}

// DeviceName is the name other devices see.
func (a *App) DeviceName() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.DeviceName()
}

func (a *App) localAddrs() []Addr {
	a.mu.Lock()
	fresh := time.Since(a.addrsAt) < 10*time.Second
	cached := a.addrs
	a.mu.Unlock()
	if fresh {
		return cached
	}
	ifs, _ := netinfo.List()
	var addrs []Addr
	for _, ifc := range ifs {
		for _, ip := range ifc.Addrs {
			addrs = append(addrs, Addr{Label: ifc.Label, IP: ip.String()})
		}
	}
	a.mu.Lock()
	a.addrs, a.addrsAt = addrs, time.Now()
	a.mu.Unlock()
	return addrs
}

func (a *App) State() State {
	addrs := a.localAddrs()
	home, _ := os.UserHomeDir()
	a.mu.Lock()
	defer a.mu.Unlock()
	st := State{
		Version: a.version, Home: home, Name: a.cfg.DeviceName(), OS: runtime.GOOS, Code: a.recv.CurrentCode(),
		Port: a.port, Addrs: addrs,
		Settings: Settings{Name: a.cfg.Name, Dest: a.cfg.ReceiveDir(), Conflict: orDefault(a.cfg.Conflict, "error")},
		Offers:   append([]*offer{}, a.offers...),
		Peers:    append([]discovery.Peer{}, a.peers...),
		Scanning: a.scanning,
		PhoneURL: a.phoneURL,
		PhoneQR:  a.phoneQR,
	}
	st.Offers = append(st.Offers, a.roomOffers()...)
	st.RoomPeers = a.roomPeers()
	for _, x := range a.transfers {
		c := *x
		st.Transfers = append(st.Transfers, &c)
	}
	sort.SliceStable(st.Transfers, func(i, j int) bool { return st.Transfers[i].Started.After(st.Transfers[j].Started) })
	return st
}

func orIP(label, ip string) string {
	if label != "" {
		return label
	}
	return ip
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// topLevel returns the names the user picked and the number of files.
func topLevel(entries []proto.FileEntry) (top []string, files int) {
	for _, f := range entries {
		if !f.Dir {
			files++
		}
		if !strings.Contains(f.Path, "/") {
			top = append(top, f.Path)
		}
	}
	return top, files
}

func summarize(top []string) string {
	switch len(top) {
	case 0:
		return "(nothing)"
	case 1:
		return top[0]
	case 2:
		return top[0] + " and " + top[1]
	}
	return fmt.Sprintf("%s and %d more", top[0], len(top)-1)
}

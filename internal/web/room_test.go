package web

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

type roomClient struct {
	t             *testing.T
	srv           *httptest.Server
	ws            *websocket.Conn
	id, key, code string
	name          string
	lastRoster    []rosterItem
}

func newRoomServer(t *testing.T) *httptest.Server {
	old := roomPenalty
	roomPenalty = time.Millisecond
	t.Cleanup(func() { roomPenalty = old })
	h := NewServer(Options{Name: func() string { return "desk" }, CheckCode: func(string) bool { return false }})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Cleanup(h.Close)
	return srv
}

func joinRoom(t *testing.T, srv *httptest.Server, name string) *roomClient {
	t.Helper()
	ws, err := websocket.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/room/ws", "", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	c := &roomClient{t: t, srv: srv, ws: ws, name: name}
	c.send(roomMsg{Type: "hello", Name: name})
	w := c.wait("welcome")
	c.id, c.key, c.code = w.ID, w.Key, w.Code
	if c.id == "" || c.key == "" || len(c.code) != 6 {
		t.Fatalf("welcome: %+v", w)
	}
	return c
}

func (c *roomClient) send(m roomMsg) {
	c.t.Helper()
	if err := websocket.JSON.Send(c.ws, m); err != nil {
		c.t.Fatal(err)
	}
}

// wait reads messages until one of the given type, remembering rosters on the way.
func (c *roomClient) wait(typ string) roomMsg {
	c.t.Helper()
	_ = c.ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var m roomMsg
		if err := websocket.JSON.Receive(c.ws, &m); err != nil {
			c.t.Fatalf("%s waiting for %q: %v", c.name, typ, err)
		}
		if m.Type == "roster" {
			c.lastRoster = m.Members
		}
		if m.Type == typ {
			return m
		}
	}
}

// waitRoster waits for a roster that satisfies ok.
func (c *roomClient) waitRoster(ok func([]rosterItem) bool) []rosterItem {
	c.t.Helper()
	if c.lastRoster != nil && ok(c.lastRoster) {
		return c.lastRoster
	}
	for {
		if l := c.wait("roster").Members; ok(l) {
			return l
		}
	}
}

func (c *roomClient) put(xfer string, i int, body io.Reader) (*http.Response, error) {
	req, _ := http.NewRequest("PUT", fmt.Sprintf("%s/room/up/%s/%d?k=%s", c.srv.URL, xfer, i, c.key), body)
	req.Header.Set("X-Bitflow", "1")
	return http.DefaultClient.Do(req)
}

func (c *roomClient) get(xfer string) (*http.Response, error) {
	return http.Get(fmt.Sprintf("%s/room/dl/%s?k=%s", c.srv.URL, xfer, c.key))
}

func has(id string) func([]rosterItem) bool {
	return func(l []rosterItem) bool {
		for _, m := range l {
			if m.ID == id {
				return true
			}
		}
		return false
	}
}

// relay runs one accepted transfer: the receiver downloads while the sender
// uploads each file. It returns the downloaded bytes.
func relayFiles(t *testing.T, from, to *roomClient, xfer string, bodies ...[]byte) []byte {
	t.Helper()
	type result struct {
		b   []byte
		err error
	}
	got := make(chan result, 1)
	go func() {
		res, err := to.get(xfer)
		if err != nil {
			got <- result{err: err}
			return
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			got <- result{err: fmt.Errorf("download: %s", res.Status)}
			return
		}
		b, err := io.ReadAll(res.Body)
		got <- result{b, err}
	}()
	from.wait("accepted")
	for i, b := range bodies {
		res, err := from.put(xfer, i, bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("upload %d: %s", i, res.Status)
		}
	}
	r := <-got
	if r.err != nil {
		t.Fatal(r.err)
	}
	from.wait("done")
	to.wait("done")
	return r.b
}

func TestRoomRosterAndLeave(t *testing.T) {
	srv := newRoomServer(t)
	a := joinRoom(t, srv, "alice's phone")
	b := joinRoom(t, srv, "bob\x00's ipad") // control characters are dropped
	l := a.waitRoster(has(b.id))
	if len(l) != 1 || l[0].Name != "bob's ipad" || l[0].Paired || !l[0].Online {
		t.Fatalf("alice's roster: %+v", l)
	}
	if l := b.waitRoster(has(a.id)); len(l) != 1 || l[0].Name != "alice's phone" {
		t.Fatalf("bob's roster: %+v", l)
	}
	b.ws.Close()
	a.waitRoster(func(l []rosterItem) bool { return len(l) == 1 && !l[0].Online }) // kept for the grace period
}

func TestRoomRejectsOtherOrigins(t *testing.T) {
	srv := newRoomServer(t)
	if _, err := websocket.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/room/ws", "", "http://evil.example"); err == nil {
		t.Fatal("a cross-origin page joined the room")
	}
}

func TestRoomWrongCodeRotates(t *testing.T) {
	srv := newRoomServer(t)
	a, b := joinRoom(t, srv, "a"), joinRoom(t, srv, "b")
	files := []rfile{{Name: "x.txt", Size: 1}}
	for i := 0; i < roomPairFailures; i++ {
		a.send(roomMsg{Type: "offer", To: b.id, Code: "000000", Files: files})
		if e := a.wait("offer-error"); !strings.Contains(e.Error, "wrong code") {
			t.Fatalf("wrong code answer: %+v", e)
		}
	}
	rotated := b.wait("code").Code
	if rotated == b.code || len(rotated) != 6 {
		t.Fatalf("code didn't rotate: %q", rotated)
	}
	a.send(roomMsg{Type: "offer", To: b.id, Code: b.code, Files: files}) // the old code is dead
	a.wait("offer-error")
	a.send(roomMsg{Type: "offer", To: b.id, Code: rotated[:3] + " " + rotated[3:], Files: files})
	a.wait("offered")
	b.wait("incoming")
}

func TestRoomSingleFileAndPairing(t *testing.T) {
	srv := newRoomServer(t)
	a, b := joinRoom(t, srv, "a"), joinRoom(t, srv, "b")
	data := bytes.Repeat([]byte("room!"), 200_000)
	a.send(roomMsg{Type: "offer", To: b.id, Code: b.code, Files: []rfile{{Name: "../../clip.mov", Size: int64(len(data))}}})
	x := a.wait("offered").Xfer
	in := b.wait("incoming")
	if in.FromName != "a" || in.Total != int64(len(data)) || in.Files[0].Name != "clip.mov" {
		t.Fatalf("incoming: %+v", in)
	}
	if got := relayFiles(t, a, b, x, data); !bytes.Equal(got, data) {
		t.Fatalf("received %d bytes, want %d", len(got), len(data))
	}

	// The download can't be replayed.
	if res, err := b.get(x); err != nil || res.StatusCode != http.StatusConflict {
		t.Fatalf("second download: %v %v", res.Status, err)
	}

	// Both ways are now paired: b sends back with no code.
	b.waitRoster(func(l []rosterItem) bool { return len(l) == 1 && l[0].Paired })
	b.send(roomMsg{Type: "offer", To: a.id, Files: []rfile{{Name: "reply.txt", Size: 2}}})
	x2 := b.wait("offered").Xfer
	a.wait("incoming")
	if got := relayFiles(t, b, a, x2, []byte("hi")); string(got) != "hi" {
		t.Fatalf("reply: %q", got)
	}
}

func TestRoomZipAndKeyChecks(t *testing.T) {
	srv := newRoomServer(t)
	a, b, eve := joinRoom(t, srv, "a"), joinRoom(t, srv, "b"), joinRoom(t, srv, "eve")
	files := []rfile{{Name: "Photos/2026/a.jpg", Size: 6}, {Name: "Photos/b.jpg", Size: 0}, {Name: "notes.txt", Size: 5}}
	a.send(roomMsg{Type: "offer", To: b.id, Code: b.code, Files: files})
	x := a.wait("offered").Xfer
	b.wait("incoming")

	// Only the receiver may download, and only the sender may upload.
	if res, _ := eve.get(x); res.StatusCode != http.StatusNotFound {
		t.Fatalf("eve downloaded: %s", res.Status)
	}
	if res, _ := eve.put(x, 0, strings.NewReader("evil!!")); res.StatusCode != http.StatusNotFound {
		t.Fatalf("eve uploaded: %s", res.Status)
	}

	got := relayFiles(t, a, b, x, []byte("jpeg-a"), nil, []byte("notes"))
	zr, err := zip.NewReader(bytes.NewReader(got), int64(len(got)))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"Photos/2026/a.jpg": "jpeg-a", "Photos/b.jpg": "", "notes.txt": "notes"}
	for _, zf := range zr.File {
		rc, _ := zf.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		if w, ok := want[zf.Name]; !ok || w != string(b) {
			t.Errorf("zip entry %s = %q", zf.Name, b)
		}
		delete(want, zf.Name)
	}
	if len(want) != 0 {
		t.Errorf("missing from zip: %v", want)
	}
}

func TestRoomDecline(t *testing.T) {
	srv := newRoomServer(t)
	a, b := joinRoom(t, srv, "a"), joinRoom(t, srv, "b")
	a.send(roomMsg{Type: "offer", To: b.id, Code: b.code, Files: []rfile{{Name: "x", Size: 1}}})
	x := a.wait("offered").Xfer
	b.wait("incoming")
	b.send(roomMsg{Type: "decline", Xfer: x})
	if m := a.wait("declined"); !strings.Contains(m.Error, "b declined") {
		t.Fatalf("declined: %+v", m)
	}
	if res, _ := b.get(x); res.StatusCode != http.StatusConflict {
		t.Fatalf("download after decline: %s", res.Status)
	}
}

// A cancel mid-file must end both HTTP requests, even though the sender's
// body is still open and the relay is blocked reading it.
func TestRoomCancelMidTransfer(t *testing.T) {
	srv := newRoomServer(t)
	a, b := joinRoom(t, srv, "a"), joinRoom(t, srv, "b")
	a.send(roomMsg{Type: "offer", To: b.id, Code: b.code, Files: []rfile{{Name: "big.iso", Size: 1 << 30}}})
	x := a.wait("offered").Xfer
	b.wait("incoming")

	dl := make(chan error, 1)
	go func() {
		res, err := b.get(x)
		if err == nil {
			_, err = io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
		dl <- err
	}()
	a.wait("accepted")
	pr, pw := io.Pipe()
	defer pw.Close()
	up := make(chan int, 1)
	go func() {
		res, err := a.put(x, 0, pr)
		if err != nil {
			up <- -1
			return
		}
		res.Body.Close()
		up <- res.StatusCode
	}()
	if _, err := pw.Write(make([]byte, 64<<10)); err != nil { // the relay has started reading
		t.Fatal(err)
	}
	b.send(roomMsg{Type: "cancel", Xfer: x})
	if m := a.wait("cancelled"); !strings.Contains(m.Error, "b cancelled") {
		t.Fatalf("cancelled: %+v", m)
	}
	select {
	case code := <-up:
		if code == http.StatusOK {
			t.Fatal("upload reported success after cancel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upload request still hanging after cancel")
	}
	select {
	case err := <-dl:
		if err == nil {
			t.Fatal("cut-short download read as complete")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("download still hanging after cancel")
	}
}

func TestRoomReconnectKeepsSeat(t *testing.T) {
	srv := newRoomServer(t)
	a, b := joinRoom(t, srv, "a"), joinRoom(t, srv, "b")
	a.waitRoster(has(b.id))
	b.ws.Close()

	ws, err := websocket.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/room/ws", "", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	b2 := &roomClient{t: t, srv: srv, ws: ws, name: "b2"}
	b2.send(roomMsg{Type: "hello", Name: "b", Key: b.key})
	w := b2.wait("welcome")
	if w.ID != b.id || w.Code != b.code {
		t.Fatalf("rejoined as %+v, want id %s code %s", w, b.id, b.code)
	}
	a.waitRoster(func(l []rosterItem) bool { return len(l) == 1 && l[0].ID == b.id && l[0].Online })
}

func TestRoomNamesAreUnique(t *testing.T) {
	srv := newRoomServer(t)
	a := joinRoom(t, srv, "iPhone")
	b := joinRoom(t, srv, "iphone")
	if l := a.waitRoster(has(b.id)); l[0].Name != "iphone 2" {
		t.Fatalf("second iPhone is called %q", l[0].Name)
	}
	b.send(roomMsg{Type: "rename", Name: "IPHONE"})
	if n := b.wait("name").Name; n != "IPHONE 2" {
		t.Fatalf("rename to a taken name gave %q", n)
	}
}

package ui

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/sshivamanand/bit-flow/internal/web"
)

type phone struct {
	t   *testing.T
	ws  *websocket.Conn
	url string
	key string
	id  string
}

func (p *phone) wait(typ string) map[string]any {
	p.t.Helper()
	_ = p.ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var m map[string]any
		if err := websocket.JSON.Receive(p.ws, &m); err != nil {
			p.t.Fatalf("waiting for %s: %v", typ, err)
		}
		if m["type"] == typ {
			return m
		}
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out: %s", what)
}

// A phone in the room shows up under nearby, sends to the app through the
// usual accept prompt into the receive folder, and gets files back.
func TestAppInRoom(t *testing.T) {
	s := newTestServer(t)
	a := s.app
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.ctx = ctx
	dest := t.TempDir()
	a.cfg.Dest = dest
	h := web.NewServer(web.Options{Name: a.DeviceName, CheckCode: func(c string) bool { return c == "111222" }})
	srv := httptest.NewServer(h)
	defer srv.Close()
	a.room = h.JoinRoom("studio", "Mac", func(c string) bool { return c == "111222" })

	ws, err := websocket.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/room/ws", "", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	p := &phone{t: t, ws: ws, url: srv.URL}
	_ = websocket.JSON.Send(ws, map[string]string{"type": "hello", "name": "pixel"})
	w := p.wait("welcome")
	p.key, p.id = w["key"].(string), w["id"].(string)
	var hostID string
	for hostID == "" {
		for _, m := range p.wait("roster")["members"].([]any) {
			if mm := m.(map[string]any); mm["host"] == true {
				hostID = mm["id"].(string)
			}
		}
	}
	eventually(t, "the phone under nearby", func() bool { return len(a.State().RoomPeers) == 1 })

	// phone → app: an offer like any other, accepted into the receive folder.
	_ = websocket.JSON.Send(ws, map[string]any{"type": "offer", "to": hostID, "code": "111 222",
		"files": []map[string]any{{"name": "Trip/a.jpg", "size": 5}}})
	x := p.wait("offered")["xfer"].(string)
	var of *offer
	eventually(t, "the accept prompt", func() bool {
		for _, o := range a.State().Offers {
			if o.Via == "room" {
				of = o
				return true
			}
		}
		return false
	})
	if of.From != "pixel" || of.Name != "Trip" {
		t.Fatalf("offer: %+v", of)
	}
	if err := a.Decide(of.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	p.wait("accepted")
	req, _ := http.NewRequest("PUT", srv.URL+"/room/up/"+x+"/0?k="+p.key, strings.NewReader("jpeg!"))
	req.Header.Set("X-Bitflow", "1")
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("upload: %v %v", res, err)
	}
	res.Body.Close()
	eventually(t, "the file in the receive folder", func() bool {
		b, err := os.ReadFile(filepath.Join(dest, "Trip", "a.jpg"))
		return err == nil && string(b) == "jpeg!"
	})
	eventually(t, "the row to finish", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return len(a.transfers) > 0 && a.transfers[0].State == "done" && a.transfers[0].Dir == "in"
	})

	// app → phone: paired by the accept, so no code needed.
	src := filepath.Join(t.TempDir(), "notes.txt")
	must(t, os.WriteFile(src, []byte("hello phone"), 0o644))
	if _, err := a.Send("room:"+p.id, "pixel", []string{src}, "", false); err != nil {
		t.Fatal(err)
	}
	in := p.wait("incoming")["xfer"].(string)
	res, err = http.Get(srv.URL + "/room/dl/" + in + "?k=" + p.key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(got) != "hello phone" {
		t.Fatalf("phone got %q", got)
	}
	eventually(t, "the send row to finish", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.transfers[0].Dir == "out" && a.transfers[0].State == "done"
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

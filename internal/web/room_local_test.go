package web

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newLocalRoom(t *testing.T) (*httptest.Server, *Local) {
	old := roomPenalty
	roomPenalty = time.Millisecond
	t.Cleanup(func() { roomPenalty = old })
	h := NewServer(Options{Name: func() string { return "desk" }, CheckCode: func(string) bool { return false }})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Cleanup(h.Close)
	return srv, h.JoinRoom("desk", "Mac", func(c string) bool { return c == "123456" })
}

// waitFor polls cond: Local's state is updated by its own goroutine.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestLocalReceivesIntoFolder(t *testing.T) {
	srv, local := newLocalRoom(t)
	phone := joinRoom(t, srv, "phone")
	host := phone.waitRoster(func(l []rosterItem) bool { return len(l) == 1 && l[0].Host })[0]
	if host.Name != "desk" || host.Kind != "Mac" {
		t.Fatalf("host in the phone's roster: %+v", host)
	}
	waitFor(t, "the phone in the host's peers", func() bool { return len(local.Peers()) == 1 })

	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "a.jpg"), []byte("keep me"), 0o644)) // never overwritten

	// The host's code is its pairing code, checked by its own checker.
	phone.send(roomMsg{Type: "offer", To: host.ID, Code: "000000", Files: []rfile{{Name: "a.jpg", Size: 1}}})
	if e := phone.wait("offer-error"); !strings.Contains(e.Error, "pairing code shown on desk") {
		t.Fatalf("wrong code: %+v", e)
	}
	files := []rfile{{Name: "a.jpg", Size: 6}, {Name: "Trip/day1/b.mov", Size: 3}, {Name: "../../escape.txt", Size: 2}}
	phone.send(roomMsg{Type: "offer", To: host.ID, Code: "123 456", Files: files})
	x := phone.wait("offered").Xfer
	waitFor(t, "the offer", func() bool { return len(local.Incoming()) == 1 })
	if in := local.Incoming()[0]; in.PeerName != "phone" || in.Total != 11 {
		t.Fatalf("incoming: %+v", in)
	}

	done := make(chan error, 1)
	go func() { done <- local.Accept(x, dir) }()
	phone.wait("accepted")
	for i, b := range []string{"jpeg-a", "mov", "hi"} {
		res, err := phone.put(x, i, strings.NewReader(b))
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("upload %d: %v %v", i, res.Status, err)
		}
		res.Body.Close()
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	phone.wait("done")
	for name, want := range map[string]string{"a.jpg": "keep me", "a (1).jpg": "jpeg-a", "Trip/day1/b.mov": "mov", "escape.txt": "hi"} {
		if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name))); err != nil || string(b) != want {
			t.Errorf("%s = %q, %v", name, b, err)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.bitflow-partial")); len(m) != 0 {
		t.Errorf("partial files left: %v", m)
	}
	waitFor(t, "the local transfer to finish", func() bool { lx, _ := local.Xfer(x); return lx.State == "done" })
}

func TestLocalSendsToBrowser(t *testing.T) {
	srv, local := newLocalRoom(t)
	phone := joinRoom(t, srv, "phone")
	waitFor(t, "the phone", func() bool { return len(local.Peers()) == 1 })
	data := bytes.Repeat([]byte("x"), 300_000)
	open := func(i int) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	if _, err := local.Offer(phone.id, "000000", []RoomFile{{Name: "big.bin", Size: int64(len(data))}}, open); err == nil ||
		!strings.Contains(err.Error(), "wrong code") {
		t.Fatalf("wrong code: %v", err)
	}
	id, err := local.Offer(phone.id, phone.code, []RoomFile{{Name: "big.bin", Size: int64(len(data))}}, open)
	if err != nil {
		t.Fatal(err)
	}
	phone.wait("incoming")
	res, err := phone.get(id)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !bytes.Equal(got, data) {
		t.Fatalf("phone got %d bytes", len(got))
	}
	waitFor(t, "done", func() bool { lx, _ := local.Xfer(id); return lx.State == "done" && lx.Bytes == int64(len(data)) })

	// Accepting paired them both ways: the phone sends back with no code.
	phone.send(roomMsg{Type: "offer", To: local.m.id, Files: []rfile{{Name: "r.txt", Size: 1}}})
	phone.wait("offered")
	waitFor(t, "the reply offer", func() bool { return len(local.Incoming()) == 1 })
	local.Decline(local.Incoming()[0].ID)
	phone.wait("declined")
}

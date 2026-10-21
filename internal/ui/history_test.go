package ui

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/sshivamanand/bit-flow/internal/history"
)

var tokenHdr = map[string]string{"X-Bitflow-Token": "secret-token", "X-Bitflow": "1"}

func TestBrowserUploadsAreGrouped(t *testing.T) {
	s := newTestServer(t)
	a := s.app
	a.onBrowserUpload("Photos/2026/a.jpg", 10, "iPhone (192.168.1.35)")
	a.onBrowserUpload("DSC_0432.JPG", 4, "iPhone (192.168.1.35)")
	a.onBrowserUpload("Photos/b.jpg", 20, "iPhone (192.168.1.35)")
	a.onBrowserUpload("DSC_0433.JPG", 5, "iPhone (192.168.1.35)")
	a.onBrowserUpload("notes.txt", 1, "browser (192.168.1.50)")

	a.mu.Lock()
	rows := map[string]*xfer{}
	for _, x := range a.transfers {
		rows[x.Name] = x
	}
	n := len(a.transfers)
	a.mu.Unlock()
	if n != 3 {
		t.Fatalf("%d rows, want 3 (folder, loose files, other sender)", n)
	}
	if p := rows["Photos"]; p == nil || p.Files != 2 || p.Total != 30 || len(p.Items) != 2 ||
		p.Items[0].Name != "2026/a.jpg" || filepath.Base(p.Dest) != "Photos" {
		t.Fatalf("folder row: %+v", p)
	}
	if l := rows["2 files"]; l == nil || l.Total != 9 || l.Items[1].Name != "DSC_0433.JPG" {
		t.Fatalf("loose row: %+v", l)
	}
	if o := rows["notes.txt"]; o == nil || filepath.Base(o.Dest) != "notes.txt" {
		t.Fatalf("single-file row: %+v", o)
	}

	// The saved list reloads the same way.
	if got := loadHistory(a.dir); len(got) != 3 {
		t.Fatalf("reloaded %d rows", len(got))
	}

	// Remove one row; more files from that upload start a new row.
	if w := do(t, s, "POST", "/api/transfers/"+rows["Photos"].ID+"/remove", "{}", tokenHdr); w.Code != http.StatusOK {
		t.Fatalf("remove: %d %s", w.Code, w.Body)
	}
	a.onBrowserUpload("Photos/c.jpg", 1, "iPhone (192.168.1.35)")
	a.mu.Lock()
	if a.transfers[0].Name != "Photos" || a.transfers[0].Files != 1 || len(a.transfers) != 3 {
		t.Errorf("after remove: first row %+v of %d", a.transfers[0], len(a.transfers))
	}
	a.mu.Unlock()

	// Clear history empties the list and the shared log.
	a.uploads.Flush()
	if rs, _ := history.Load(0); len(rs) == 0 {
		t.Fatal("nothing logged")
	}
	if w := do(t, s, "POST", "/api/transfers/clear", "{}", tokenHdr); w.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", w.Code, w.Body)
	}
	a.mu.Lock()
	left := len(a.transfers)
	a.mu.Unlock()
	if rs, _ := history.Load(0); left != 0 || len(rs) != 0 {
		t.Fatalf("after clear: %d rows, %d log records", left, len(rs))
	}
}

func TestOldPerFileRowsMerge(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 18, 0, 0, 0, time.UTC)
	row := func(name string, at time.Time, peer string) *xfer {
		return &xfer{ID: name, Dir: "in", Peer: peer, Name: name, Files: 1, Total: 4, Done: 4, State: "done", Dest: "/dl", Started: at, Finished: at}
	}
	peerXfer := &xfer{ID: "game", Dir: "in", Peer: "laptop", Name: "Game", Files: 900, State: "done", Started: t0.Add(-time.Hour), Finished: t0.Add(-50 * time.Minute)}
	got := mergeOldUploads([]*xfer{ // newest first, as saved
		row("DSC_0435.JPG", t0.Add(3*time.Second), "browser (192.168.1.35)"),
		row("DSC_0434.JPG", t0.Add(2*time.Second), "browser (192.168.1.35)"),
		row("DSC_0433.JPG", t0.Add(time.Second), "browser (192.168.1.35)"),
		row("x.txt", t0.Add(-10*time.Minute), "browser (192.168.1.35)"), // long gap: its own row
		peerXfer,
	})
	if len(got) != 3 || got[0].Name != "3 files" || got[0].Total != 12 || got[0].Items[0].Name != "DSC_0433.JPG" ||
		got[1].Name != "x.txt" || got[2] != peerXfer {
		for _, g := range got {
			t.Logf("%+v", g)
		}
		t.Fatal("old rows didn't merge as expected")
	}
}

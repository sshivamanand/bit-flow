package ui

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/sshivamanand/bit-flow/internal/history"
)

// historyLimit bounds the saved transfer history.
const historyLimit = 200

// loadHistory reads finished transfers saved by earlier runs.
func loadHistory(dir string) []*xfer {
	b, err := os.ReadFile(filepath.Join(dir, "history.json"))
	if err != nil {
		return nil
	}
	var xs []*xfer
	if json.Unmarshal(b, &xs) != nil {
		return nil
	}
	return mergeOldUploads(xs)
}

// mergeOldUploads folds history saved before browser uploads were grouped
// (one row per file) into one row per run of files from the same sender.
// Those rows are recognizable: one file, received, and started == finished.
func mergeOldUploads(xs []*xfer) []*xfer {
	old := func(x *xfer) bool {
		return x.Dir == "in" && x.State == "done" && x.Files == 1 && x.Items == nil && x.Started.Equal(x.Finished)
	}
	var out []*xfer
	for _, x := range xs { // newest first
		if !old(x) {
			out = append(out, x)
			continue
		}
		it := item{Name: x.Name, Size: x.Total}
		if n := len(out); n > 0 {
			if g := out[n-1]; g.Dir == "in" && g.State == "done" && g.Peer == x.Peer && g.Items != nil &&
				g.Started.Sub(x.Finished) < history.BatchGap && g.Started.Sub(x.Finished) >= 0 {
				g.Items = append(g.Items, it)
				g.Files++
				g.Total += x.Total
				g.Done = g.Total
				g.Started = x.Started
				g.Name = history.BatchName("", x.Name, g.Files)
				if filepath.Base(g.Dest) == g.Items[0].Name { // a single file's row pointed at the file
					g.Dest = filepath.Dir(g.Dest)
				}
				continue
			}
		}
		g := *x
		g.Items = []item{it}
		out = append(out, &g)
	}
	for _, g := range out { // the list reads newest first; files read oldest first
		if g.Items != nil {
			for i, j := 0, len(g.Items)-1; i < j; i, j = i+1, j-1 {
				g.Items[i], g.Items[j] = g.Items[j], g.Items[i]
			}
		}
	}
	return out
}

// saveHistoryLocked writes finished transfers. Caller holds a.mu.
func (a *App) saveHistoryLocked() {
	if a.dir == "" {
		return
	}
	var done []*xfer
	for _, x := range a.transfers {
		if x.State != "running" && x.State != "waiting" {
			done = append(done, x)
		}
		if len(done) == historyLimit {
			break
		}
	}
	b, err := json.Marshal(done)
	if err != nil {
		return
	}
	path := filepath.Join(a.dir, "history.json")
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	return xs
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

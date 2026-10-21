// Package history keeps a log of finished transfers, shared by the CLI and
// the app: one JSON object per line in history.jsonl in the config dir.
// Appending a single short line is effectively atomic, so several bit-flow
// processes can log at once without locking.
package history

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/sshivamanand/bit-flow/internal/config"
)

// Record is one finished (or failed) transfer.
type Record struct {
	Time    time.Time `json:"time"`
	Dir     string    `json:"dir"` // "sent" or "received"
	Peer    string    `json:"peer"`
	Name    string    `json:"name"`
	Files   int       `json:"files,omitempty"`
	Bytes   int64     `json:"bytes"`             // moved over the network
	Already int64     `json:"already,omitempty"` // already at the receiver
	Seconds float64   `json:"seconds"`
	State   string    `json:"state"` // done, failed, declined, cancelled
	Error   string    `json:"error,omitempty"`
}

func path() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "history.jsonl"), nil
}

// Append logs r. Failures to log never fail a transfer, so errors are ignored by callers.
func Append(r Record) error {
	p, err := path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// Load returns the most recent records, newest first (at most limit; 0 = all).
func Load(limit int) ([]Record, error) {
	p, err := path()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var all []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			all = append(all, r)
		}
	}
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, sc.Err()
}

// Rate is the average speed in bytes per second.
func (r Record) Rate() float64 {
	if r.Seconds <= 0 {
		return 0
	}
	return float64(r.Bytes) / r.Seconds
}

// Clear deletes the whole log.
func Clear() error {
	p, err := path()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

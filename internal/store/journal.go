package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/sshivamanand/bit-flow/internal/proto"
)

// journal is the on-disk resume state for one offer at one destination.
type journal struct {
	Key     string                `json:"key"`
	Dest    string                `json:"dest"`
	Renames map[string]string     `json:"renames,omitempty"`
	Files   map[int][]proto.Range `json:"files"`
	Saved   time.Time             `json:"saved"`
}

// JournalKey identifies an offer (paths, sizes, mtimes) landing at a destination.
// The same files sent again to the same folder get the same key, which is how
// a reconnecting sender finds its earlier progress.
func JournalKey(root string, entries []proto.FileEntry) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00", root)
	for _, e := range entries {
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00%t\n", e.Path, e.Size, e.MTime, e.Dir)
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func loadJournal(path, key, root string) *journal {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var j journal
	if json.Unmarshal(b, &j) != nil || j.Key != key || j.Dest != root {
		return nil
	}
	return &j
}

// checkpoint records which ranges are durable. Order matters: snapshot the
// ranges, fsync the files they belong to, and only then publish the journal,
// so it never claims bytes that a crash could lose.
func (p *Plan) checkpoint() error {
	p.ckMu.Lock()
	defer p.ckMu.Unlock()

	j := journal{Key: p.key, Dest: p.root, Renames: p.renames, Files: map[int][]proto.Range{}, Saved: time.Now()}
	var open []*os.File
	for i := range p.files {
		st := &p.files[i]
		st.mu.Lock()
		if !st.done && !st.skip && st.have.total() > 0 {
			j.Files[i] = st.have.clone()
			if st.f != nil {
				open = append(open, st.f)
			}
		}
		st.mu.Unlock()
	}
	for _, f := range open {
		_ = f.Sync() // a file finished meanwhile is already closed and complete
	}
	if len(j.Files) == 0 && len(j.Renames) == 0 {
		_ = os.Remove(p.journal)
		return nil
	}

	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.journal), 0o700); err != nil {
		return err
	}
	tmp := p.journal + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.journal)
}

func (p *Plan) checkpointLoop() {
	defer close(p.ckDone)
	t := time.NewTicker(checkpointEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			_ = p.checkpoint()
		case <-p.stopCk:
			return
		}
	}
}

func (p *Plan) stopCheckpoints() {
	if p.stopCk == nil {
		return
	}
	select {
	case <-p.stopCk:
	default:
		close(p.stopCk)
	}
	<-p.ckDone
}

// rangeSet is a sorted list of non-overlapping, non-adjacent byte ranges.
type rangeSet []proto.Range

func (rs *rangeSet) add(off, n int64) {
	if n <= 0 {
		return
	}
	s := *rs
	end := off + n
	// First range that ends at or after off (could merge), and first that starts after end.
	i := sort.Search(len(s), func(k int) bool { return s[k].Off+s[k].Len >= off })
	j := sort.Search(len(s), func(k int) bool { return s[k].Off > end })
	if i < j {
		off = min(off, s[i].Off)
		end = max(end, s[j-1].Off+s[j-1].Len)
	}
	merged := proto.Range{Off: off, Len: end - off}
	s = append(s[:i], append([]proto.Range{merged}, s[j:]...)...)
	*rs = s
}

func (rs rangeSet) total() int64 {
	var n int64
	for _, r := range rs {
		n += r.Len
	}
	return n
}

func (rs rangeSet) clone() []proto.Range { return append([]proto.Range(nil), rs...) }

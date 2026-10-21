package history

import (
	"fmt"
	pathpkg "path"
	"strings"
	"sync"
	"time"
)

// BatchGap is how long a browser may pause between files and still be part
// of the same upload. Browsers send one file at a time, so a 300-photo upload
// arrives as 300 separate files; without grouping it fills the history.
const BatchGap = 2 * time.Minute

// Batches groups files uploaded one by one into one entry per top-level
// folder, or per run of loose files, from the same sender. Each batch is
// logged once, after it has gone quiet for BatchGap (or on Flush).
type Batches struct {
	Gap time.Duration      // 0 means BatchGap
	Log func(Record) error // nil means Append
	mu  sync.Mutex
	on  map[string]*openBatch // by sender + folder
	seq int
}

type openBatch struct {
	id    string
	rec   Record
	start time.Time
	timer *time.Timer
}

// Add records one uploaded file (rel is its path inside the receive folder,
// with forward slashes) and returns the batch it joined. isNew reports
// whether that batch was started by this file.
func (b *Batches) Add(rel string, size int64, from string, now time.Time) (id string, isNew bool) {
	gap := b.Gap
	if gap == 0 {
		gap = BatchGap
	}
	folder := TopFolder(rel)
	key := from + "\x00" + folder
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.on == nil {
		b.on = map[string]*openBatch{}
	}
	ob := b.on[key]
	if ob == nil {
		b.seq++
		ob = &openBatch{id: fmt.Sprintf("b%d", b.seq), start: now,
			rec: Record{Dir: "received", Peer: from, State: "done"}}
		b.on[key] = ob
		isNew = true
	}
	ob.rec.Files++
	ob.rec.Bytes += size
	ob.rec.Time = now
	ob.rec.Seconds = now.Sub(ob.start).Seconds()
	ob.rec.Name = BatchName(folder, pathpkg.Base(rel), ob.rec.Files)
	if ob.timer != nil {
		ob.timer.Stop()
	}
	ob.timer = time.AfterFunc(gap, func() { b.close(key, ob) })
	return ob.id, isNew
}

// Open reports whether batch id is still collecting files.
func (b *Batches) Open(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ob := range b.on {
		if ob.id == id {
			return true
		}
	}
	return false
}

func (b *Batches) close(key string, ob *openBatch) {
	b.mu.Lock()
	if b.on[key] != ob {
		b.mu.Unlock()
		return
	}
	delete(b.on, key)
	rec := ob.rec
	b.mu.Unlock()
	b.log(rec)
}

// Flush logs every open batch now (call it on shutdown).
func (b *Batches) Flush() {
	b.mu.Lock()
	var recs []Record
	for key, ob := range b.on {
		ob.timer.Stop()
		recs = append(recs, ob.rec)
		delete(b.on, key)
	}
	b.mu.Unlock()
	for _, r := range recs {
		b.log(r)
	}
}

func (b *Batches) log(r Record) {
	if b.Log != nil {
		_ = b.Log(r)
		return
	}
	_ = Append(r)
}

// TopFolder is the first folder of rel, or "" for a file at the top.
func TopFolder(rel string) string {
	if i := strings.IndexByte(rel, '/'); i > 0 {
		return rel[:i]
	}
	return ""
}

// BatchName is what a batch is called: its folder, its only file, or "N files".
func BatchName(folder, first string, files int) string {
	switch {
	case folder != "":
		return folder
	case files == 1:
		return first
	}
	return fmt.Sprintf("%d files", files)
}

package history

import (
	"sync"
	"testing"
	"time"
)

func TestAppendAndLoadNewestFirst(t *testing.T) {
	t.Setenv("BITFLOW_CONFIG_DIR", t.TempDir())
	for i, name := range []string{"a", "b", "c"} {
		if err := Append(Record{Time: time.Unix(int64(i), 0), Dir: "sent", Name: name, Bytes: 100, Seconds: 2, State: "done"}); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := Load(2)
	if err != nil || len(rs) != 2 || rs[0].Name != "c" || rs[1].Name != "b" {
		t.Fatalf("got %+v, %v", rs, err)
	}
	if rs[0].Rate() != 50 {
		t.Fatalf("rate %v", rs[0].Rate())
	}
}

func TestLoadMissing(t *testing.T) {
	t.Setenv("BITFLOW_CONFIG_DIR", t.TempDir())
	if rs, err := Load(0); err != nil || len(rs) != 0 {
		t.Fatalf("got %+v, %v", rs, err)
	}
}

func TestClear(t *testing.T) {
	t.Setenv("BITFLOW_CONFIG_DIR", t.TempDir())
	if err := Clear(); err != nil { // nothing there yet is fine
		t.Fatal(err)
	}
	_ = Append(Record{Time: time.Now(), Dir: "sent", Name: "a", State: "done"})
	if err := Clear(); err != nil {
		t.Fatal(err)
	}
	if rs, _ := Load(0); len(rs) != 0 {
		t.Fatalf("still %d records", len(rs))
	}
}

func TestBatchesGroupUploads(t *testing.T) {
	var logged []Record
	var mu sync.Mutex
	b := &Batches{Gap: time.Hour, Log: func(r Record) error { mu.Lock(); logged = append(logged, r); mu.Unlock(); return nil }}
	now := time.Now()
	add := func(rel, from string) (string, bool) { now = now.Add(time.Second); return b.Add(rel, 10, from, now) }

	p1, n1 := add("Photos/2026/a.jpg", "phone")
	p2, n2 := add("Photos/b.jpg", "phone")
	l1, n3 := add("DSC_0432.JPG", "phone")
	l2, _ := add("DSC_0433.JPG", "phone")
	o, n4 := add("DSC_0434.JPG", "laptop") // another sender: its own batch
	if !n1 || n2 || p1 != p2 || !n3 || l1 != l2 || l1 == p1 || !n4 || o == l1 {
		t.Fatalf("grouping: %s %v, %s %v, %s %v, %s, %s %v", p1, n1, p2, n2, l1, n3, l2, o, n4)
	}
	if !b.Open(p1) {
		t.Fatal("batch closed early")
	}
	b.Flush()
	if b.Open(p1) {
		t.Fatal("batch still open after Flush")
	}
	names := map[string]Record{}
	for _, r := range logged {
		names[r.Peer+"/"+r.Name] = r
	}
	if r := names["phone/Photos"]; r.Files != 2 || r.Bytes != 20 || r.Seconds != 1 {
		t.Errorf("folder batch: %+v", r)
	}
	if r := names["phone/2 files"]; r.Files != 2 {
		t.Errorf("loose batch: %+v", r)
	}
	if r := names["laptop/DSC_0434.JPG"]; r.Files != 1 {
		t.Errorf("single-file batch: %+v", r)
	}
	if len(logged) != 3 {
		t.Errorf("logged %d records: %+v", len(logged), logged)
	}

	// A gap starts a new batch.
	b.Gap = time.Millisecond
	id1, _ := add("x.txt", "phone")
	time.Sleep(20 * time.Millisecond)
	if b.Open(id1) {
		t.Fatal("quiet batch still open")
	}
	if id2, isNew := add("y.txt", "phone"); !isNew || id2 == id1 {
		t.Fatal("upload after a gap joined the old batch")
	}
	b.Flush()
}

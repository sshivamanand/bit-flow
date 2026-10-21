package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sshivamanand/bit-flow/internal/proto"
)

func TestSafeJoin(t *testing.T) {
	root := t.TempDir()
	ok := []string{"Game", "Game/bin/x.exe", "a b/c.d", "..hidden/file", "x..y"}
	for _, p := range ok {
		got, err := SafeJoin(root, p)
		if err != nil {
			t.Errorf("SafeJoin(%q) unexpected error: %v", p, err)
			continue
		}
		if want := filepath.Join(root, filepath.FromSlash(p)); got != want {
			t.Errorf("SafeJoin(%q) = %q, want %q", p, got, want)
		}
	}
	bad := []string{"", "/etc/passwd", "..", "../x", "a/../../x", "a//b", "a/./b", "a/", "a\x00b"}
	for _, p := range bad {
		if _, err := SafeJoin(root, p); err == nil {
			t.Errorf("SafeJoin(%q) should fail", p)
		}
	}
}

func TestWindowsNames(t *testing.T) {
	for _, n := range []string{"CON", "con.txt", "COM1", "a:b", "x?", "trail.", "trail "} {
		if windowsNameOK(n) == nil {
			t.Errorf("windowsNameOK(%q) should fail", n)
		}
	}
	for _, n := range []string{"console", "COM10", "file.txt", "Game"} {
		if err := windowsNameOK(n); err != nil {
			t.Errorf("windowsNameOK(%q) unexpected error: %v", n, err)
		}
	}
}

func TestRangeSet(t *testing.T) {
	var rs rangeSet
	rs.add(10, 10) // [10,20)
	rs.add(30, 5)  // [30,35)
	rs.add(20, 10) // bridges into [10,35)
	rs.add(0, 5)   // [0,5)
	rs.add(3, 4)   // overlaps → [0,7)
	rs.add(50, 0)  // ignored
	want := rangeSet{{Off: 0, Len: 7}, {Off: 10, Len: 25}}
	if len(rs) != len(want) || rs[0] != want[0] || rs[1] != want[1] {
		t.Fatalf("got %+v want %+v", rs, want)
	}
	if rs.total() != 32 {
		t.Fatalf("total %d", rs.total())
	}
	rs.add(5, 5) // fills the gap exactly
	if len(rs) != 1 || rs[0] != (proto.Range{Off: 0, Len: 35}) {
		t.Fatalf("got %+v", rs)
	}
}

func TestPolicyParse(t *testing.T) {
	for _, s := range []string{"error", "skip", "rename", "overwrite", "update"} {
		p, err := ParsePolicy(s)
		if err != nil || p.String() != s {
			t.Fatalf("%s → %v %v", s, p, err)
		}
	}
	if _, err := ParsePolicy("nope"); err == nil {
		t.Fatal("expected error")
	}
}

func TestCheckLinks(t *testing.T) {
	ok := []proto.FileEntry{{Path: "G", Dir: true}, {Path: "G/lib", Dir: true}, {Path: "G/lib/a.so.1"}, {Path: "G/lib/a.so", Link: "a.so.1"}}
	if err := checkLinks(ok); err != nil {
		t.Fatal(err)
	}
	escape := []proto.FileEntry{{Path: "G", Dir: true}, {Path: "G/etc", Link: "../../etc"}}
	if checkLinks(escape) == nil {
		t.Fatal("link out of the transfer accepted")
	}
	through := []proto.FileEntry{{Path: "G", Dir: true}, {Path: "G/d", Link: "sub"}, {Path: "G/sub", Dir: true}, {Path: "G/d/x"}}
	if checkLinks(through) == nil {
		t.Fatal("file written through a link accepted")
	}
}

func TestResumeJournalRoundTrip(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	entries := []proto.FileEntry{{Path: "G", Dir: true}, {Path: "G/big", Size: 1 << 20, MTime: 1_700_000_000_000_000_000}}
	p, err := Prepare(root, entries, Options{StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Materialize(); err != nil {
		t.Fatal(err)
	}
	if err := p.WriteAt(1, 0, make([]byte, 256<<10)); err != nil {
		t.Fatal(err)
	}
	p.Suspend()

	p2, err := Prepare(root, entries, Options{StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	if got := p2.Already(); got != 256<<10 {
		journals, _ := filepath.Glob(filepath.Join(state, "resume", "*"))
		var detail []string
		for _, j := range journals {
			b, _ := os.ReadFile(j)
			detail = append(detail, j+": "+string(b))
		}
		partial, perr := os.Stat(filepath.Join(root, "G", "big"+PartialSuffix))
		size := int64(-1)
		if perr == nil {
			size = partial.Size()
		}
		t.Fatalf("resumed %d bytes, want %d\njournals: %v\npartial size %d (err %v)\nroot %q key %q", got, 256<<10, detail, size, perr, root, JournalKey(root, entries))
	}
}

func TestJournalKeyIgnoresDirectoryMtimes(t *testing.T) {
	a := []proto.FileEntry{{Path: "G", Dir: true, MTime: 1}, {Path: "G/f", Size: 5, MTime: 7}}
	b := []proto.FileEntry{{Path: "G", Dir: true, MTime: 2}, {Path: "G/f", Size: 5, MTime: 7}}
	if JournalKey("/d", a) != JournalKey("/d", b) {
		t.Fatal("a directory mtime change must not change the resume key")
	}
	c := []proto.FileEntry{{Path: "G", Dir: true, MTime: 1}, {Path: "G/f", Size: 5, MTime: 8}}
	if JournalKey("/d", a) == JournalKey("/d", c) {
		t.Fatal("a file mtime change must change the resume key")
	}
}

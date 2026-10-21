package store

import (
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
	for _, s := range []string{"error", "skip", "rename", "overwrite"} {
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

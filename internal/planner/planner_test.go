package planner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sshivamanand/bit-flow/internal/proto"
)

func TestUnitsChunksAndPacks(t *testing.T) {
	m := &Manifest{Files: []proto.FileEntry{
		{Path: "d", Dir: true},
		{Path: "d/small", Size: 5},
		{Path: "d/empty"},
		{Path: "d/big", Size: 25 << 20},
		{Path: "d/small2", Size: 7},
		{Path: "d/mid", Size: 3 << 20},
	}}
	units := m.Units(10<<20, 4<<20, nil, nil)
	// big: 3 chunks, mid: 1 chunk, then one pack with both small files.
	if len(units) != 5 {
		t.Fatalf("got %d units: %+v", len(units), units)
	}
	if units[0].File != 3 || units[3].File != 5 {
		t.Fatalf("largest file should come first: %+v", units)
	}
	covered := map[uint32]int64{}
	for _, u := range units[:4] {
		if u.Offset != covered[u.File] {
			t.Fatalf("gap or overlap at %+v", u)
		}
		covered[u.File] += u.Len
	}
	if covered[3] != 25<<20 || covered[5] != 3<<20 {
		t.Fatalf("coverage wrong: %v", covered)
	}
	if p := units[4]; len(p.Pack) != 2 || p.Len != 12 {
		t.Fatalf("pack wrong: %+v", p)
	}
}

func TestPacksRespectBlockSize(t *testing.T) {
	m := &Manifest{}
	for range 10 {
		m.Files = append(m.Files, proto.FileEntry{Path: "f", Size: 1000})
	}
	units := m.Units(1<<20, 4096, nil, nil)
	for _, u := range units {
		if payload := int(u.Len) + len(u.Pack)*proto.PackEntryHeader; payload > 4096 {
			t.Fatalf("pack payload %d exceeds block", payload)
		}
	}
	if len(units) != 3 { // 4 entries of 1008 bytes fit in 4096: packs of 4, 4, 2
		t.Fatalf("got %d packs", len(units))
	}
}

func TestUnitsResume(t *testing.T) {
	m := &Manifest{Files: []proto.FileEntry{{Path: "a", Size: 100}, {Path: "b", Size: 5}, {Path: "c", Size: 2 << 20}}}
	have := map[uint32][]proto.Range{2: {{Off: 0, Len: 1 << 20}}}
	units := m.Units(512<<10, 4<<20, map[uint32]bool{0: true}, have)
	var sent int64
	for _, u := range units {
		if u.File == 0 && u.Pack == nil {
			t.Fatal("skipped file was planned")
		}
		for _, f := range u.Pack {
			if f == 0 {
				t.Fatal("skipped file was packed")
			}
		}
		if u.Pack == nil && u.File == 2 && u.Offset < 1<<20 {
			t.Fatalf("resent a range the receiver has: %+v", u)
		}
		sent += u.Len
	}
	if sent != 5+(1<<20) {
		t.Fatalf("planned %d bytes", sent)
	}
}

func TestMissing(t *testing.T) {
	got := Missing(100, []proto.Range{{Off: 50, Len: 10}, {Off: 0, Len: 20}, {Off: 15, Len: 10}})
	want := []proto.Range{{Off: 25, Len: 25}, {Off: 60, Len: 40}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v want %+v", got, want)
	}
	if len(Missing(10, []proto.Range{{Off: 0, Len: 10}})) != 0 {
		t.Fatal("fully covered file should have nothing missing")
	}
}

func TestBuild(t *testing.T) {
	dir := t.TempDir()
	game := filepath.Join(dir, "Game")
	must(t, os.MkdirAll(filepath.Join(game, "data", "empty"), 0o755))
	must(t, os.WriteFile(filepath.Join(game, "data", "a.pak"), make([]byte, 100), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "solo.iso"), make([]byte, 7), 0o644))
	must(t, os.WriteFile(filepath.Join(game, "data", ".DS_Store"), make([]byte, 9), 0o644)) // never sent
	must(t, os.Symlink("a.pak", filepath.Join(game, "data", "link")))                       // inside: sent
	must(t, os.Symlink("../../solo.iso", filepath.Join(game, "data", "escape")))            // outside: skipped

	m, err := Build([]string{game, filepath.Join(dir, "solo.iso")})
	must(t, err)
	if m.Total != 107 || m.FileCount() != 3 || len(m.Skipped) != 1 {
		t.Fatalf("total=%d files=%d skipped=%v", m.Total, m.FileCount(), m.Skipped)
	}
	want := map[string]bool{"Game": true, "Game/data": true, "Game/data/a.pak": true, "Game/data/empty": true, "Game/data/link": true, "solo.iso": true}
	for _, f := range m.Files {
		if !want[f.Path] {
			t.Errorf("unexpected entry %q", f.Path)
		}
		delete(want, f.Path)
	}
	if len(want) != 0 {
		t.Errorf("missing entries %v", want)
	}

	if _, err := Build([]string{game, game}); err == nil {
		t.Error("duplicate top-level names should fail")
	}
}

func TestLinkInside(t *testing.T) {
	ok := [][2]string{{"Game/lib/libx.so", "libx.so.1"}, {"Game/F.framework/Current", "Versions/A"}, {"Game/a/b", "../c"}, {"Game/a/b", ".."}}
	for _, c := range ok {
		if !LinkInside(c[0], c[1]) {
			t.Errorf("%s -> %s should be inside", c[0], c[1])
		}
	}
	bad := [][2]string{{"Game/a", "/etc/passwd"}, {"Game/a", "../../x"}, {"Game/a/b", "../../../Other"}, {"Game/a", ""}, {"Game", "."}}
	for _, c := range bad {
		if LinkInside(c[0], c[1]) {
			t.Errorf("%s -> %s should be rejected", c[0], c[1])
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

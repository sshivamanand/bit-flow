package planner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sshivamanand/bit-flow/internal/proto"
)

func TestUnitsCoverFilesLargestFirst(t *testing.T) {
	m := &Manifest{Files: []proto.FileEntry{
		{Path: "d", Dir: true},
		{Path: "d/small", Size: 5},
		{Path: "d/empty"},
		{Path: "d/big", Size: 25},
	}}
	units := m.Units(10)
	if len(units) != 4 {
		t.Fatalf("got %d units, want 4: %+v", len(units), units)
	}
	if units[0].File != 3 {
		t.Fatalf("largest file should come first, got %+v", units[0])
	}
	covered := map[uint32]int64{}
	for _, u := range units {
		if u.Offset != covered[u.File] {
			t.Fatalf("gap or overlap at %+v", u)
		}
		covered[u.File] += u.Len
	}
	if covered[3] != 25 || covered[1] != 5 {
		t.Fatalf("coverage wrong: %v", covered)
	}
}

func TestBuild(t *testing.T) {
	dir := t.TempDir()
	game := filepath.Join(dir, "Game")
	must(t, os.MkdirAll(filepath.Join(game, "data", "empty"), 0o755))
	must(t, os.WriteFile(filepath.Join(game, "data", "a.pak"), make([]byte, 100), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "solo.iso"), make([]byte, 7), 0o644))
	must(t, os.Symlink("a.pak", filepath.Join(game, "data", "link")))

	m, err := Build([]string{game, filepath.Join(dir, "solo.iso")})
	must(t, err)
	if m.Total != 107 || m.FileCount() != 2 || len(m.Skipped) != 1 {
		t.Fatalf("total=%d files=%d skipped=%v", m.Total, m.FileCount(), m.Skipped)
	}
	want := map[string]bool{"Game": true, "Game/data": true, "Game/data/a.pak": true, "Game/data/empty": true, "solo.iso": true}
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

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

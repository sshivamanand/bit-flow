package steam

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSteam builds a Steam install with a second library and three games.
func fakeSteam(t *testing.T) (root, lib2 string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "Steam")
	lib2 = filepath.Join(base, "Games Drive", "SteamLibrary")
	for _, d := range []string{root, lib2} {
		if err := os.MkdirAll(filepath.Join(d, "steamapps", "common"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Steam escapes backslashes in Windows paths ("C:\\Steam").
	vdfPath := func(p string) string { return strings.ReplaceAll(p, `\`, `\\`) }
	write(filepath.Join(root, "steamapps", "libraryfolders.vdf"), `"libraryfolders"
{
	// a comment
	"0"
	{
		"path"		"`+vdfPath(root)+`"
		"apps" { "620" "123" }
	}
	"1"
	{
		"path"		"`+vdfPath(lib2)+`"
	}
}`)
	manifest := func(lib, id, name, dir, flags string) {
		write(filepath.Join(lib, "steamapps", "appmanifest_"+id+".acf"), `"AppState"
{
	"appid"		"`+id+`"
	"name"		"`+name+`"
	"StateFlags"		"`+flags+`"
	"installdir"		"`+dir+`"
	"SizeOnDisk"		"1234"
}`)
		write(filepath.Join(lib, "steamapps", "common", dir, "game.bin"), "data")
	}
	manifest(root, "620", "Portal 2", "Portal 2", "4")
	manifest(lib2, "1145360", "Hades", "Hades", "4")
	manifest(lib2, "999", "Half Downloaded", "HalfDL", "1026") // not fully installed
	return root, lib2
}

func TestGamesAcrossLibraries(t *testing.T) {
	root, lib2 := fakeSteam(t)
	t.Setenv("BITFLOW_STEAM_ROOT", root)
	libs := Libraries()
	if len(libs) != 2 {
		t.Fatalf("libraries %v", libs)
	}
	games := Games()
	if len(games) != 2 || games[0].Name != "Hades" || games[1].Name != "Portal 2" {
		t.Fatalf("games %+v", games)
	}
	if games[0].Library != lib2 && games[0].Library != mustEval(lib2) {
		t.Fatalf("Hades library %q", games[0].Library)
	}
	if games[1].Size != 1234 || filepath.Base(games[1].Dir) != "Portal 2" {
		t.Fatalf("Portal 2 %+v", games[1])
	}
}

func TestFind(t *testing.T) {
	root, _ := fakeSteam(t)
	t.Setenv("BITFLOW_STEAM_ROOT", root)
	for _, q := range []string{"portal", "620", "Portal 2"} {
		g, err := Find(q)
		if err != nil || len(g) != 1 || g[0].AppID != "620" {
			t.Fatalf("Find(%q) = %+v, %v", q, g, err)
		}
	}
	if _, err := Find("doom"); err == nil {
		t.Fatal("expected no match")
	}
}

func TestParseVDFEscapesAndOldFormat(t *testing.T) {
	v, err := ParseVDF(`"LibraryFolders" { "TimeNextStatsReport" "1" "1" "D:\\SteamLibrary" "name" "say \"hi\"" }`)
	if err != nil {
		t.Fatal(err)
	}
	lf := v.Get("libraryfolders").(map[string]any)
	if lf["1"] != `D:\SteamLibrary` || lf["name"] != `say "hi"` {
		t.Fatalf("parsed %v", lf)
	}
	if _, err := ParseVDF(`"a" { "b" "c" `); err == nil {
		t.Fatal("unterminated block accepted")
	}
}

func mustEval(p string) string {
	r, _ := filepath.EvalSymlinks(p)
	return r
}

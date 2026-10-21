package store

import (
	"path/filepath"
	"testing"
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

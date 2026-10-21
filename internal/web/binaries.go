package web

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// Binary is a bit-flow executable this computer can hand out.
type Binary struct {
	OS, Arch string
	Path     string
	Size     int64
}

// Platform is "darwin-arm64", "windows-amd64", …
func (b Binary) Platform() string { return b.OS + "-" + b.Arch }

var binName = regexp.MustCompile(`^bitflow-(darwin|linux|windows)-(amd64|arm64)(\.exe)?$`)

// FindBinaries returns the running executable plus any cross-built copies
// (`make cross` output) found next to it or in ./dist, so one laptop can
// give bit-flow to any other, with no internet.
func FindBinaries() []Binary {
	found := map[string]Binary{}
	if exe, err := os.Executable(); err == nil {
		if st, err := os.Stat(exe); err == nil {
			b := Binary{OS: runtime.GOOS, Arch: runtime.GOARCH, Path: exe, Size: st.Size()}
			found[b.Platform()] = b
		}
		dirs := []string{filepath.Dir(exe), filepath.Join(filepath.Dir(exe), "dist"), filepath.Join(filepath.Dir(exe), "..", "dist"), "dist"}
		for _, dir := range dirs {
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				m := binName.FindStringSubmatch(e.Name())
				if m == nil {
					continue
				}
				b := Binary{OS: m[1], Arch: m[2], Path: filepath.Join(dir, e.Name())}
				if _, ok := found[b.Platform()]; ok {
					continue
				}
				if info, err := e.Info(); err == nil {
					b.Size = info.Size()
					found[b.Platform()] = b
				}
			}
		}
	}
	out := make([]Binary, 0, len(found))
	for _, b := range found {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Platform() < out[j].Platform() })
	return out
}

func (s *server) binary(w http.ResponseWriter, r *http.Request) {
	want := strings.TrimSuffix(r.PathValue("platform"), ".exe")
	if s.o.Binaries == nil {
		http.NotFound(w, r)
		return
	}
	for _, b := range s.o.Binaries() {
		if b.Platform() == want {
			name := "bitflow"
			if b.OS == "windows" {
				name += ".exe"
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", contentDisposition(name))
			http.ServeFile(w, r, b.Path)
			return
		}
	}
	http.Error(w, "bit-flow for "+want+" isn't available on this computer", http.StatusNotFound)
}

// installer is a POSIX shell script: `curl -fsSL http://HOST/get/sh | sh`.
// curl doesn't mark downloads as quarantined, so macOS runs the binary without
// a Gatekeeper prompt.
func (s *server) installer(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	host := r.Host
	if !hostOK(host) {
		http.Error(w, "bad host", http.StatusBadRequest)
		return
	}
	_, _ = w.Write([]byte(`#!/bin/sh
# bit-flow installer, served by ` + s.o.Name() + `
set -e
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) echo "unsupported CPU: $(uname -m)"; exit 1 ;; esac
dir="${BITFLOW_DIR:-$HOME/.local/bin}"
mkdir -p "$dir"
echo "downloading bit-flow for $os-$arch from ` + host + ` ..."
curl -fsSL "http://` + host + `/get/$os-$arch" -o "$dir/bitflow.tmp"
chmod +x "$dir/bitflow.tmp"
mv "$dir/bitflow.tmp" "$dir/bitflow"
echo "installed $dir/bitflow"
case ":$PATH:" in *":$dir:"*) ;; *) echo "add it to your PATH, or run it as $dir/bitflow" ;; esac
"$dir/bitflow" version
`))
}

// hostOK keeps the Host header from injecting anything into the script.
func hostOK(h string) bool {
	if h == "" || len(h) > 255 {
		return false
	}
	for _, r := range h {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(".:-[]", r)) {
			return false
		}
	}
	return true
}

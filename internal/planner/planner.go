// Package planner turns the paths a sender picked into a manifest and a list of
// work units (chunks) that the data streams pull from.
package planner

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/sshivamanand/bit-flow/internal/proto"
)

// Manifest is what the sender offers. Sources[i] is the local path of Files[i].
type Manifest struct {
	Files   []proto.FileEntry
	Sources []string
	Total   int64
	Skipped []string // symlinks, devices, sockets: not sent
}

// Build walks each path. A directory "…/Game" becomes entries "Game", "Game/…";
// a file "…/disk.iso" becomes "disk.iso".
func Build(paths []string) (*Manifest, error) {
	m := &Manifest{}
	seen := map[string]bool{}
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(abs)
		if err != nil {
			return nil, err
		}
		base := filepath.Base(abs)
		if base == string(filepath.Separator) || base == "." || filepath.VolumeName(abs)+string(filepath.Separator) == abs {
			return nil, fmt.Errorf("cannot send a filesystem root: %s", p)
		}
		if seen[base] {
			return nil, fmt.Errorf("two items named %q; rename one or send them separately", base)
		}
		seen[base] = true

		if !info.IsDir() {
			m.add(abs, base, info)
			continue
		}
		err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(abs, path)
			if err != nil {
				return err
			}
			name := base
			if rel != "." {
				name = base + "/" + filepath.ToSlash(rel)
			}
			m.add(path, name, info)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *Manifest) add(src, name string, info fs.FileInfo) {
	e := proto.FileEntry{Path: name, Mode: uint32(info.Mode().Perm()), MTime: info.ModTime().UnixNano()}
	switch {
	case info.IsDir():
		e.Dir = true
	case info.Mode().IsRegular():
		e.Size = info.Size()
		m.Total += e.Size
	default:
		m.Skipped = append(m.Skipped, src)
		return
	}
	m.Files = append(m.Files, e)
	m.Sources = append(m.Sources, src)
}

// FileCount is the number of regular files (not directories).
func (m *Manifest) FileCount() int {
	n := 0
	for _, f := range m.Files {
		if !f.Dir {
			n++
		}
	}
	return n
}

// Unit is one chunk of one file: the unit of scheduling (and, from M2, of resume).
type Unit struct {
	File   uint32
	Offset int64
	Len    int64
}

// Units splits every non-empty file into chunks of at most chunk bytes, largest
// files first so the transfer never ends waiting on one huge file.
func (m *Manifest) Units(chunk int64) []Unit {
	idx := make([]int, 0, len(m.Files))
	n := 0
	for i, f := range m.Files {
		if !f.Dir && f.Size > 0 {
			idx = append(idx, i)
			n += int((f.Size + chunk - 1) / chunk)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool { return m.Files[idx[a]].Size > m.Files[idx[b]].Size })
	units := make([]Unit, 0, n)
	for _, i := range idx {
		size := m.Files[i].Size
		for off := int64(0); off < size; off += chunk {
			units = append(units, Unit{File: uint32(i), Offset: off, Len: min(chunk, size-off)})
		}
	}
	return units
}

// Package planner turns the paths a sender picked into a manifest and a list of
// work units (chunks) that the data streams pull from.
package planner

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sshivamanand/bit-flow/internal/proto"
)

// Manifest is what the sender offers. Sources[i] is the local path of Files[i].
type Manifest struct {
	Files   []proto.FileEntry
	Sources []string
	Total   int64
	Skipped []string // devices, sockets, and symlinks pointing outside what's sent
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
	case info.Mode()&fs.ModeSymlink != 0:
		// App and game bundles rely on relative symlinks (macOS frameworks,
		// Linux .so versions). Links that leave what's being sent can't work
		// on the other computer, so they're skipped.
		target, err := os.Readlink(src)
		if err != nil || !LinkInside(name, filepath.ToSlash(target)) {
			m.Skipped = append(m.Skipped, src)
			return
		}
		e.Link = filepath.ToSlash(target)
	default:
		m.Skipped = append(m.Skipped, src)
		return
	}
	m.Files = append(m.Files, e)
	m.Sources = append(m.Sources, src)
}

// LinkInside reports whether a symlink at name (slash-separated, relative to
// the transfer root) with the given target stays inside its top-level item.
func LinkInside(name, target string) bool {
	if target == "" || path.IsAbs(target) || strings.Contains(target, "\\") {
		return false
	}
	top, _, _ := strings.Cut(name, "/")
	resolved := path.Clean(path.Join(path.Dir(name), target))
	return resolved == top || strings.HasPrefix(resolved, top+"/")
}

// FileCount is the number of regular files and links (not directories).
func (m *Manifest) FileCount() int {
	n := 0
	for _, f := range m.Files {
		if !f.Dir {
			n++
		}
	}
	return n
}

// SmallFile is the largest file sent inside a pack instead of as its own chunk.
// Games ship tens of thousands of small files; packing them avoids a frame,
// checksum and scheduling round per file.
const SmallFile = 1 << 20

// Unit is the unit of scheduling: one chunk of one file, or a pack of whole small files.
type Unit struct {
	File   uint32
	Offset int64
	Len    int64    // file bytes carried (for packs: sum of member sizes)
	Pack   []uint32 // non-nil: whole small files sent together in one frame
}

// Units plans what to send. Files in skip are already complete at the receiver;
// have lists ranges the receiver already holds (resume). Big files are split
// into chunks of at most chunk bytes, largest files first so the transfer never
// ends waiting on one huge file; small files are packed into frames of at most
// block bytes and sent last, where their small size evens out the tail.
func (m *Manifest) Units(chunk int64, block int, skip map[uint32]bool, have map[uint32][]proto.Range) []Unit {
	var big, small []int
	for i, f := range m.Files {
		id := uint32(i)
		if f.Dir || f.Size == 0 || skip[id] {
			continue
		}
		if f.Size <= SmallFile && f.Size+proto.PackEntryHeader <= int64(block) && len(have[id]) == 0 {
			small = append(small, i)
		} else {
			big = append(big, i)
		}
	}
	sort.SliceStable(big, func(a, b int) bool { return m.Files[big[a]].Size > m.Files[big[b]].Size })

	var units []Unit
	for _, i := range big {
		id := uint32(i)
		for _, r := range Missing(m.Files[i].Size, have[id]) {
			end := r.Off + r.Len
			for off := r.Off; off < end; off += chunk {
				units = append(units, Unit{File: id, Offset: off, Len: min(chunk, end-off)})
			}
		}
	}

	// Packs keep manifest (directory) order, which is friendly to both disks.
	var cur Unit
	payload := 0
	for _, i := range small {
		size := int(m.Files[i].Size)
		if len(cur.Pack) > 0 && payload+proto.PackEntryHeader+size > block {
			units = append(units, cur)
			cur, payload = Unit{}, 0
		}
		cur.Pack = append(cur.Pack, uint32(i))
		cur.Len += int64(size)
		payload += proto.PackEntryHeader + size
	}
	if len(cur.Pack) > 0 {
		units = append(units, cur)
	}
	return units
}

// Missing returns the parts of [0, size) not covered by have.
func Missing(size int64, have []proto.Range) []proto.Range {
	hs := append([]proto.Range(nil), have...)
	sort.Slice(hs, func(a, b int) bool { return hs[a].Off < hs[b].Off })
	var out []proto.Range
	pos := int64(0)
	for _, h := range hs {
		if h.Off > pos {
			out = append(out, proto.Range{Off: pos, Len: min(h.Off, size) - pos})
		}
		pos = max(pos, h.Off+h.Len)
		if pos >= size {
			break
		}
	}
	if pos < size {
		out = append(out, proto.Range{Off: pos, Len: size - pos})
	}
	return out
}

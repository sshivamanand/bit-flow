// Package store writes incoming files on the receiver: it validates paths,
// creates directories, writes blocks at their exact offsets (in any order, from
// many goroutines), and renames each file into place once all its bytes arrive.
package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/sshivamanand/bit-flow/internal/proto"
)

// PartialSuffix marks files that are still being received.
const PartialSuffix = ".bitflow-partial"

// ConflictError lists destination paths that already exist.
type ConflictError struct{ Paths []string }

func (e *ConflictError) Error() string {
	shown := e.Paths
	if len(shown) > 5 {
		shown = shown[:5]
	}
	msg := fmt.Sprintf("%d item(s) already exist at the destination: %s", len(e.Paths), strings.Join(shown, ", "))
	if len(e.Paths) > len(shown) {
		msg += ", …"
	}
	return msg + " (receiver can use --overwrite)"
}

type fileState struct {
	mu        sync.Mutex
	entry     proto.FileEntry
	final     string
	partial   string
	f         *os.File
	remaining int64
	done      bool
}

// Plan is the receiver's view of one incoming transfer.
type Plan struct {
	root  string
	files []fileState
}

// Prepare validates every path and checks for conflicts. It has no side effects,
// so the user can still decline afterwards.
func Prepare(root string, entries []proto.FileEntry, overwrite bool) (*Plan, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	p := &Plan{root: root, files: make([]fileState, len(entries))}
	seen := make(map[string]bool, len(entries))
	var conflicts []string
	for i, e := range entries {
		dst, err := SafeJoin(root, e.Path)
		if err != nil {
			return nil, err
		}
		if seen[dst] {
			return nil, fmt.Errorf("duplicate path in offer: %q", e.Path)
		}
		seen[dst] = true
		if e.Size < 0 || (e.Dir && e.Size != 0) {
			return nil, fmt.Errorf("invalid size for %q", e.Path)
		}
		st := &p.files[i]
		st.entry, st.final, st.partial, st.remaining = e, dst, dst+PartialSuffix, e.Size
		if !e.Dir && !overwrite {
			if _, err := os.Lstat(dst); err == nil {
				conflicts = append(conflicts, e.Path)
			}
		}
	}
	if len(conflicts) > 0 {
		return nil, &ConflictError{Paths: conflicts}
	}
	return p, nil
}

// Materialize creates all directories and empty files.
func (p *Plan) Materialize() error {
	if err := os.MkdirAll(p.root, 0o755); err != nil {
		return err
	}
	for i := range p.files {
		st := &p.files[i]
		if st.entry.Dir {
			if err := os.MkdirAll(st.final, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(st.final), 0o755); err != nil {
			return err
		}
		if st.entry.Size == 0 {
			f, err := os.OpenFile(st.partial, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			st.f = f
			if err := st.finish(); err != nil {
				return err
			}
		}
	}
	return nil
}

// WriteAt writes one verified block. Safe for concurrent use; blocks of the same
// file may arrive in any order on any stream.
func (p *Plan) WriteAt(idx uint32, off int64, data []byte) error {
	if int(idx) >= len(p.files) {
		return fmt.Errorf("block for unknown file #%d", idx)
	}
	st := &p.files[idx]
	if st.entry.Dir || off < 0 || off+int64(len(data)) > st.entry.Size {
		return fmt.Errorf("block out of range for %q (offset %d, len %d)", st.entry.Path, off, len(data))
	}

	st.mu.Lock()
	if st.done {
		st.mu.Unlock()
		return fmt.Errorf("unexpected extra block for %q", st.entry.Path)
	}
	if st.f == nil {
		f, err := os.OpenFile(st.partial, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			st.mu.Unlock()
			return err
		}
		// Size the file up front so out-of-order writes never extend it.
		if err := f.Truncate(st.entry.Size); err != nil {
			f.Close()
			st.mu.Unlock()
			return err
		}
		st.f = f
	}
	f := st.f
	st.mu.Unlock()

	if _, err := f.WriteAt(data, off); err != nil {
		return fmt.Errorf("write %q: %w", st.entry.Path, err)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	st.remaining -= int64(len(data))
	switch {
	case st.remaining < 0:
		return fmt.Errorf("received too many bytes for %q", st.entry.Path)
	case st.remaining == 0:
		return st.finish()
	}
	return nil
}

// finish closes the partial file, restores its mode and mtime, and renames it into place.
// Caller holds st.mu (or has exclusive access).
func (st *fileState) finish() error {
	if err := st.f.Close(); err != nil {
		return err
	}
	st.f = nil
	if err := os.Chmod(st.partial, perm(st.entry.Mode, 0o644)); err != nil {
		return err
	}
	if st.entry.MTime != 0 {
		t := time.Unix(0, st.entry.MTime)
		_ = os.Chtimes(st.partial, t, t)
	}
	if err := os.Rename(st.partial, st.final); err != nil {
		return err
	}
	st.done = true
	return nil
}

// Finish checks every file arrived and restores directory modes and mtimes
// (deepest first, since writing into a directory bumps its mtime).
func (p *Plan) Finish() error {
	var missing []string
	for i := range p.files {
		st := &p.files[i]
		if !st.entry.Dir && !st.done {
			missing = append(missing, st.entry.Path)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%d file(s) incomplete, e.g. %q", len(missing), missing[0])
	}
	for i := len(p.files) - 1; i >= 0; i-- {
		st := &p.files[i]
		if !st.entry.Dir {
			continue
		}
		_ = os.Chmod(st.final, perm(st.entry.Mode, 0o755))
		if st.entry.MTime != 0 {
			t := time.Unix(0, st.entry.MTime)
			_ = os.Chtimes(st.final, t, t)
		}
	}
	return nil
}

// Abort closes open files and removes partial files. (M2 keeps them for resume.)
func (p *Plan) Abort() {
	for i := range p.files {
		st := &p.files[i]
		st.mu.Lock()
		if st.f != nil {
			st.f.Close()
			st.f = nil
		}
		if !st.entry.Dir && !st.done {
			os.Remove(st.partial)
		}
		st.mu.Unlock()
	}
}

func perm(mode uint32, def os.FileMode) os.FileMode {
	if m := os.FileMode(mode) & os.ModePerm; m != 0 {
		return m | 0o200 // keep files writable by their new owner
	}
	return def
}

// SafeJoin joins a sender-supplied relative path onto root, rejecting anything that
// could escape root or that the local filesystem can't represent.
func SafeJoin(root, rel string) (string, error) {
	bad := func(why string) (string, error) {
		return "", fmt.Errorf("unsafe path %q: %s", rel, why)
	}
	if rel == "" || strings.HasPrefix(rel, "/") || strings.ContainsRune(rel, 0) {
		return bad("empty, absolute or contains NUL")
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." {
			return bad("empty, '.' or '..' component")
		}
		if runtime.GOOS == "windows" {
			if err := windowsNameOK(part); err != nil {
				return bad(err.Error())
			}
		}
	}
	p := filepath.Join(root, filepath.FromSlash(rel))
	r, err := filepath.Rel(root, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(r) {
		return bad("escapes destination")
	}
	return p, nil
}

var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

func windowsNameOK(name string) error {
	if strings.ContainsAny(name, `<>:"\|?*`) {
		return errors.New(`contains a character Windows forbids (<>:"\|?*)`)
	}
	for _, r := range name {
		if r < 32 {
			return errors.New("contains a control character")
		}
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return errors.New("ends with a dot or space")
	}
	stem, _, _ := strings.Cut(name, ".")
	if windowsReserved[strings.ToUpper(stem)] {
		return errors.New("reserved Windows device name")
	}
	return nil
}

// Package store writes incoming files on the receiver: it validates paths,
// applies the conflict policy, creates directories, writes blocks at their
// exact offsets (in any order, from many goroutines), and renames each file
// into place once all its bytes arrive. A resume journal records which byte
// ranges are safely on disk, so an interrupted transfer continues where it
// stopped instead of starting over.
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

	"github.com/sshivamanand/bit-flow/internal/planner"
	"github.com/sshivamanand/bit-flow/internal/proto"
)

// PartialSuffix marks files that are still being received.
const PartialSuffix = ".bitflow-partial"

// mtimeSlack absorbs filesystems that store coarse timestamps (FAT: 2 s).
const mtimeSlack = 2 * time.Second

// checkpointEvery bounds how much progress a crash can lose. Each checkpoint
// fsyncs open files, which is slow on macOS (F_FULLFSYNC), so keep it seconds apart.
const checkpointEvery = 3 * time.Second

// Policy decides what happens when a file already exists at the destination
// and differs from the incoming one. Identical files (same size and mtime) are
// always skipped, except under Overwrite.
type Policy int

const (
	PolicyError     Policy = iota // refuse the transfer, listing the conflicts
	PolicySkip                    // keep existing files, send the rest
	PolicyRename                  // put each conflicting top-level item at "Name (1)"
	PolicyOverwrite               // replace existing files
	PolicyUpdate                  // skip identical files, replace the rest (game updates)
)

func ParsePolicy(s string) (Policy, error) {
	switch s {
	case "", "error":
		return PolicyError, nil
	case "skip":
		return PolicySkip, nil
	case "rename":
		return PolicyRename, nil
	case "overwrite":
		return PolicyOverwrite, nil
	case "update":
		return PolicyUpdate, nil
	}
	return 0, fmt.Errorf("unknown conflict policy %q (use error, skip, rename, overwrite or update)", s)
}

func (p Policy) String() string {
	return [...]string{"error", "skip", "rename", "overwrite", "update"}[p]
}

// Options configure a Plan.
type Options struct {
	Policy Policy
	// StateDir holds resume journals. Empty disables resume (failed transfers
	// then remove their partial files).
	StateDir string
}

// ConflictError lists destination paths that already exist and differ.
type ConflictError struct{ Paths []string }

func (e *ConflictError) Error() string {
	shown := e.Paths
	if len(shown) > 5 {
		shown = shown[:5]
	}
	msg := fmt.Sprintf("%d item(s) already exist at the destination and differ: %s", len(e.Paths), strings.Join(shown, ", "))
	if len(e.Paths) > len(shown) {
		msg += ", …"
	}
	return msg + " (receiver can use --on-conflict=skip, rename or overwrite)"
}

// SpaceError reports a destination without enough free space.
type SpaceError struct{ Need, Free int64 }

func (e *SpaceError) Error() string {
	return fmt.Sprintf("not enough free space at the destination: need %.2f GB, %.2f GB free",
		float64(e.Need)/1e9, float64(e.Free)/1e9)
}

type fileState struct {
	mu        sync.Mutex
	entry     proto.FileEntry
	final     string
	partial   string
	f         *os.File
	have      rangeSet // bytes written, for the resume journal
	remaining int64
	resumed   bool // partial file with earlier progress exists
	skip      bool // already at the destination; not sent
	done      bool
}

// Plan is the receiver's view of one incoming transfer.
type Plan struct {
	root    string
	opts    Options
	files   []fileState
	renames map[string]string // top-level renames under PolicyRename
	key     string
	journal string

	ckMu   sync.Mutex
	stopCk chan struct{}
	ckDone chan struct{}
}

// Prepare validates every path, applies the conflict policy, loads any resume
// journal and checks free space. It has no side effects, so the user can still
// decline afterwards.
func Prepare(root string, entries []proto.FileEntry, opts Options) (*Plan, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	p := &Plan{root: root, opts: opts, files: make([]fileState, len(entries)), key: JournalKey(root, entries)}

	var j *journal
	if opts.StateDir != "" {
		p.journal = filepath.Join(opts.StateDir, "resume", p.key+".json")
		j = loadJournal(p.journal, p.key, root)
	}
	if j != nil {
		p.renames = j.Renames
	} else if opts.Policy == PolicyRename {
		p.renames = topLevelRenames(root, entries)
	}

	if err := checkLinks(entries); err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(entries))
	var conflicts []string
	for i, e := range entries {
		rel := p.rename(e.Path)
		dst, err := SafeJoin(root, rel)
		if err != nil {
			return nil, err
		}
		if seen[dst] {
			return nil, fmt.Errorf("duplicate path in offer: %q", e.Path)
		}
		seen[dst] = true
		if e.Size < 0 || ((e.Dir || e.Link != "") && e.Size != 0) {
			return nil, fmt.Errorf("invalid size for %q", e.Path)
		}
		st := &p.files[i]
		st.entry, st.final, st.partial, st.remaining = e, dst, dst+PartialSuffix, e.Size

		info, err := os.Lstat(dst)
		if err != nil {
			if j != nil {
				st.resume(j.Files[i])
			}
			continue
		}
		if e.Dir {
			if !info.IsDir() {
				conflicts = append(conflicts, rel)
			}
			continue
		}
		if e.Link != "" {
			if t, err := os.Readlink(dst); err == nil && t == filepath.FromSlash(e.Link) && opts.Policy != PolicyOverwrite {
				st.skip, st.done = true, true
			} else if opts.Policy == PolicySkip {
				st.skip, st.done = true, true
			} else if (opts.Policy != PolicyOverwrite && opts.Policy != PolicyUpdate) || info.IsDir() {
				conflicts = append(conflicts, rel)
			}
			continue
		}
		identical := info.Mode().IsRegular() && info.Size() == e.Size &&
			absDur(info.ModTime().Sub(time.Unix(0, e.MTime))) <= mtimeSlack
		switch {
		case info.IsDir():
			conflicts = append(conflicts, rel) // never replace a directory with a file
		case identical && opts.Policy != PolicyOverwrite, opts.Policy == PolicySkip:
			st.skip, st.done, st.remaining = true, true, 0
		case opts.Policy == PolicyOverwrite, opts.Policy == PolicyUpdate:
			if j != nil {
				st.resume(j.Files[i])
			}
		default:
			conflicts = append(conflicts, rel)
		}
	}
	if len(conflicts) > 0 {
		return nil, &ConflictError{Paths: conflicts}
	}

	need := p.Needed()
	if free, err := diskFree(existingAncestor(root)); err == nil && free >= 0 && need > 0 && free < need+(64<<20) {
		return nil, &SpaceError{Need: need, Free: free}
	}
	return p, nil
}

// resume adopts ranges from the journal if the partial file still matches.
func (st *fileState) resume(ranges []proto.Range) {
	if len(ranges) == 0 || st.entry.Dir {
		return
	}
	info, err := os.Stat(st.partial)
	if err != nil || info.Size() != st.entry.Size {
		return
	}
	for _, r := range ranges {
		if r.Off >= 0 && r.Len > 0 && r.Off+r.Len <= st.entry.Size {
			st.have.add(r.Off, r.Len)
		}
	}
	st.remaining = st.entry.Size - st.have.total()
	st.resumed = st.have.total() > 0
}

func (p *Plan) rename(path string) string {
	if len(p.renames) == 0 {
		return path
	}
	top, rest, nested := strings.Cut(path, "/")
	if to, ok := p.renames[top]; ok {
		if nested {
			return to + "/" + rest
		}
		return to
	}
	return path
}

// topLevelRenames picks "Name (n)" for every top-level item that exists at root.
func topLevelRenames(root string, entries []proto.FileEntry) map[string]string {
	m := map[string]string{}
	for _, e := range entries {
		if strings.Contains(e.Path, "/") {
			continue
		}
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(e.Path))); err != nil {
			continue
		}
		ext := ""
		if !e.Dir {
			ext = filepath.Ext(e.Path)
		}
		stem := strings.TrimSuffix(e.Path, ext)
		for n := 1; ; n++ {
			cand := fmt.Sprintf("%s (%d)%s", stem, n, ext)
			if _, err := os.Lstat(filepath.Join(root, cand)); err != nil {
				m[e.Path] = cand
				break
			}
		}
	}
	return m
}

// Renames reports top-level items that will be saved under a different name.
func (p *Plan) Renames() map[string]string { return p.renames }

// Root is the absolute destination folder.
func (p *Plan) Root() string { return p.root }

// Skip lists files the sender must not send (already complete here).
func (p *Plan) Skip() []uint32 {
	var out []uint32
	for i := range p.files {
		if p.files[i].skip {
			out = append(out, uint32(i))
		}
	}
	return out
}

// Have lists ranges of partially received files (resume).
func (p *Plan) Have() []proto.FileRanges {
	var out []proto.FileRanges
	for i := range p.files {
		if st := &p.files[i]; st.resumed {
			out = append(out, proto.FileRanges{File: uint32(i), Ranges: st.have.clone()})
		}
	}
	return out
}

// Already is how many bytes of the offer are already here.
func (p *Plan) Already() int64 {
	var n int64
	for i := range p.files {
		st := &p.files[i]
		if st.skip {
			n += st.entry.Size
		} else {
			n += st.have.total()
		}
	}
	return n
}

// Needed is how many bytes still have to arrive.
func (p *Plan) Needed() int64 {
	var n int64
	for i := range p.files {
		n += p.files[i].remaining
	}
	return n
}

// Resumed reports whether this plan continues an earlier interrupted transfer.
func (p *Plan) Resumed() bool {
	for i := range p.files {
		if p.files[i].resumed {
			return true
		}
	}
	return false
}

// Materialize creates directories and empty files, and starts checkpointing.
func (p *Plan) Materialize() error {
	if err := os.MkdirAll(p.root, 0o755); err != nil {
		return err
	}
	for i := range p.files {
		st := &p.files[i]
		if st.skip {
			continue
		}
		if st.entry.Dir {
			if err := os.MkdirAll(st.final, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(st.final), 0o755); err != nil {
			return err
		}
		if st.entry.Link != "" {
			continue // created by Finish, after every file is in place
		}
		if st.entry.Size == 0 {
			if err := st.writeWhole(nil); err != nil {
				return err
			}
		}
	}
	if p.journal != "" {
		p.stopCk, p.ckDone = make(chan struct{}), make(chan struct{})
		go p.checkpointLoop()
	}
	return nil
}

// WriteAt writes one verified block of a big file. Safe for concurrent use;
// blocks of the same file may arrive in any order on any stream.
func (p *Plan) WriteAt(idx uint32, off int64, data []byte) error {
	st, err := p.file(idx)
	if err != nil {
		return err
	}
	if off < 0 || off+int64(len(data)) > st.entry.Size {
		return fmt.Errorf("block out of range for %q (offset %d, len %d)", st.entry.Path, off, len(data))
	}

	st.mu.Lock()
	if st.done {
		st.mu.Unlock()
		return fmt.Errorf("unexpected block for completed %q", st.entry.Path)
	}
	if st.f == nil {
		if err := st.open(); err != nil {
			st.mu.Unlock()
			return err
		}
	}
	f := st.f
	st.mu.Unlock()

	if _, err := f.WriteAt(data, off); err != nil {
		return fmt.Errorf("write %q: %w", st.entry.Path, err)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	st.have.add(off, int64(len(data)))
	st.remaining -= int64(len(data))
	switch {
	case st.remaining < 0:
		return fmt.Errorf("received too many bytes for %q", st.entry.Path)
	case st.remaining == 0:
		return st.finish()
	}
	return nil
}

// open creates (or, when resuming, reopens) the partial file. Caller holds st.mu.
func (st *fileState) open() error {
	if st.resumed {
		f, err := os.OpenFile(st.partial, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		st.f = f
		return nil
	}
	f, err := os.OpenFile(st.partial, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	// Reserve the space up front (fewer fragments, early ENOSPC), then size the
	// file so out-of-order writes never extend it.
	_ = preallocate(f, st.entry.Size)
	if err := f.Truncate(st.entry.Size); err != nil {
		f.Close()
		return err
	}
	st.f = f
	return nil
}

// WriteFile writes a whole small file received in a pack.
func (p *Plan) WriteFile(idx uint32, data []byte) error {
	st, err := p.file(idx)
	if err != nil {
		return err
	}
	if int64(len(data)) != st.entry.Size {
		return fmt.Errorf("pack entry for %q has %d bytes, expected %d", st.entry.Path, len(data), st.entry.Size)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.done || st.f != nil {
		return fmt.Errorf("unexpected pack entry for %q", st.entry.Path)
	}
	return st.writeWhole(data)
}

// writeWhole writes a complete file in one go. Caller holds st.mu (or has exclusive access).
func (st *fileState) writeWhole(data []byte) error {
	f, err := os.OpenFile(st.partial, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	st.f = f
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %q: %w", st.entry.Path, err)
	}
	st.have.add(0, int64(len(data)))
	st.remaining = 0
	return st.finish()
}

func (p *Plan) file(idx uint32) (*fileState, error) {
	if int(idx) >= len(p.files) {
		return nil, fmt.Errorf("data for unknown file #%d", idx)
	}
	st := &p.files[idx]
	if st.entry.Dir || st.entry.Link != "" || st.skip {
		return nil, fmt.Errorf("unexpected data for %q", st.entry.Path)
	}
	return st, nil
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

// Finish checks every file arrived, drops the resume journal, and restores
// directory modes and mtimes (deepest first, since writing into a directory
// bumps its mtime).
func (p *Plan) Finish() error {
	p.stopCheckpoints()
	var missing []string
	for i := range p.files {
		st := &p.files[i]
		if !st.entry.Dir && st.entry.Link == "" && !st.done {
			missing = append(missing, st.entry.Path)
		}
	}
	if len(missing) == 0 {
		if err := p.makeLinks(); err != nil {
			return err
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%d file(s) incomplete, e.g. %q", len(missing), missing[0])
	}
	if p.journal != "" {
		_ = os.Remove(p.journal)
	}
	for i := len(p.files) - 1; i >= 0; i-- {
		st := &p.files[i]
		if !st.entry.Dir || st.skip {
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

// makeLinks creates the transfer's symlinks. It runs last, so no data is ever
// written through a link.
func (p *Plan) makeLinks() error {
	for i := range p.files {
		st := &p.files[i]
		if st.entry.Link == "" || st.done {
			continue
		}
		if p.opts.Policy == PolicyOverwrite || p.opts.Policy == PolicyUpdate {
			if info, err := os.Lstat(st.final); err == nil && !info.IsDir() {
				_ = os.Remove(st.final)
			}
		}
		if err := os.Symlink(filepath.FromSlash(st.entry.Link), st.final); err != nil {
			if runtime.GOOS == "windows" {
				continue // needs Developer Mode or admin rights; the rest of the transfer is fine
			}
			return fmt.Errorf("create link %q: %w", st.entry.Path, err)
		}
		st.done = true
	}
	return nil
}

// checkLinks rejects symlinks that point outside the transfer, and any entry
// that would be written through a link.
func checkLinks(entries []proto.FileEntry) error {
	links := map[string]bool{}
	for _, e := range entries {
		if e.Link == "" {
			continue
		}
		if e.Dir || !planner.LinkInside(e.Path, e.Link) {
			return fmt.Errorf("unsafe link %q -> %q", e.Path, e.Link)
		}
		links[e.Path] = true
	}
	if len(links) == 0 {
		return nil
	}
	for _, e := range entries {
		for dir := e.Path; ; {
			i := strings.LastIndex(dir, "/")
			if i < 0 {
				break
			}
			dir = dir[:i]
			if links[dir] {
				return fmt.Errorf("unsafe path %q: inside link %q", e.Path, dir)
			}
		}
	}
	return nil
}

// Suspend ends an interrupted transfer. With resume enabled it saves a final
// checkpoint and keeps partial files so the same offer can continue later;
// otherwise it removes them.
func (p *Plan) Suspend() {
	p.stopCheckpoints()
	if p.journal != "" {
		_ = p.checkpoint()
	}
	for i := range p.files {
		st := &p.files[i]
		st.mu.Lock()
		if st.f != nil {
			st.f.Close()
			st.f = nil
		}
		if p.journal == "" && !st.entry.Dir && !st.done && !st.skip {
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

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// existingAncestor returns the nearest existing directory at or above path.
func existingAncestor(path string) string {
	for {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
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

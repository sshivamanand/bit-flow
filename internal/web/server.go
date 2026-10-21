package web

import (
	"archive/zip"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sshivamanand/bit-flow/internal/store"
)

//go:embed page.html
var page []byte

// Options describe what this computer offers to browsers.
type Options struct {
	Name      func() string          // device name shown on the page
	CheckCode func(code string) bool // verifies the pairing code (rate-limited by the caller)
	UploadDir func() string          // non-nil: browsers can send files into this folder
	Shared    []Shared               // non-empty: browsers can download these
	Binaries  func() []Binary        // offered at /get
	OnUpload  func(name string, size int64, from string)
}

// Shared is one item a browser may download.
type Shared struct {
	Name string
	Path string
	Size int64
	Dir  bool
}

const (
	webCookie  = "bitflow_web"
	webSession = 12 * time.Hour
	maxChunk   = 16 << 20
	// uploadIdle drops uploads a browser abandoned, closing their files and
	// removing the partial data (an open handle also blocks deletion on Windows).
	uploadIdle = 15 * time.Minute
)

// Server serves browsers. Close it to release any unfinished uploads.
type Server struct {
	http.Handler
	s    *server
	stop chan struct{}
	once sync.Once
}

type server struct {
	o Options

	mu       sync.Mutex
	sessions map[string]time.Time
	uploads  map[string]*upload
}

type upload struct {
	mu      sync.Mutex
	final   string
	partial string
	f       *os.File
	size    int64
	chunks  map[int64]int64 // offset → length, so a retried chunk isn't counted twice
	written int64
	from    string
	done    bool
	touched time.Time
}

// NewServer returns the server for browser requests.
func NewServer(o Options) *Server {
	s := &server{o: o, sessions: map[string]time.Time{}, uploads: map[string]*upload{}}
	srv := &Server{s: s, stop: make(chan struct{})}
	go srv.reap()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /get", s.index)
	mux.HandleFunc("GET /get/sh", s.installer)
	mux.HandleFunc("GET /get/{platform}", s.binary)
	mux.HandleFunc("GET /web/info", s.info)
	mux.HandleFunc("POST /web/login", s.login)
	mux.HandleFunc("POST /web/upload", s.auth(s.startUpload))
	mux.HandleFunc("PUT /web/upload/{id}", s.auth(s.putChunk))
	mux.HandleFunc("GET /web/list", s.auth(s.list))
	mux.HandleFunc("GET /web/dl/{i}", s.auth(s.download))
	srv.Handler = securityHeaders(mux)
	return srv
}

// Close drops unfinished uploads.
func (srv *Server) Close() {
	srv.once.Do(func() { close(srv.stop) })
	srv.s.dropUploads(func(*upload) bool { return true })
}

func (srv *Server) reap() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-srv.stop:
			return
		case now := <-t.C:
			srv.s.dropUploads(func(u *upload) bool { return now.Sub(u.touched) > uploadIdle })
		}
	}
}

// dropUploads closes and removes unfinished uploads matching drop.
func (s *server) dropUploads(drop func(*upload) bool) {
	s.mu.Lock()
	var victims []*upload
	for id, u := range s.uploads {
		u.mu.Lock()
		if !u.done && drop(u) {
			victims = append(victims, u)
			delete(s.uploads, id)
		}
		u.mu.Unlock()
	}
	s.mu.Unlock()
	for _, u := range victims {
		u.mu.Lock()
		u.done = true
		u.f.Close()
		os.Remove(u.partial)
		u.mu.Unlock()
	}
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:")
		h.ServeHTTP(w, r)
	})
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(page)
}

// info tells the page what this computer offers.
func (s *server) info(w http.ResponseWriter, r *http.Request) {
	type bin struct {
		Platform string `json:"platform"`
		OS       string `json:"os"`
		Arch     string `json:"arch"`
		Size     int64  `json:"size"`
	}
	var bins []bin
	if s.o.Binaries != nil {
		for _, b := range s.o.Binaries() {
			bins = append(bins, bin{Platform: b.Platform(), OS: b.OS, Arch: b.Arch, Size: b.Size})
		}
	}
	writeJSON(w, map[string]any{
		"name":     s.o.Name(),
		"upload":   s.o.UploadDir != nil,
		"share":    len(s.o.Shared) > 0,
		"binaries": bins,
		"host":     r.Host,
		"signedIn": s.signedIn(r),
	})
}

// ---- sessions ----

func (s *server) signedIn(r *http.Request) bool {
	c, err := r.Cookie(webCookie)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[c.Value]
	if ok && time.Now().After(exp) {
		delete(s.sessions, c.Value)
		return false
	}
	return ok
}

func (s *server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.signedIn(r) {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]string{"error": "enter the pairing code first"})
			return
		}
		// Browsers only send this header from our own page (no cross-site forms).
		if r.Method != http.MethodGet && r.Header.Get("X-Bitflow") != "1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if r.Header.Get("X-Bitflow") != "1" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, req.Code)
	if digits == "" || !s.o.CheckCode(digits) {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]string{"error": "wrong code — check the code shown on " + s.o.Name()})
		return
	}
	tok := randomID(24)
	s.mu.Lock()
	s.sessions[tok] = time.Now().Add(webSession)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: webCookie, Value: tok, Path: "/", MaxAge: int(webSession.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, map[string]bool{"ok": true})
}

// ---- uploads: browser → this computer ----

func (s *server) startUpload(w http.ResponseWriter, r *http.Request) {
	if s.o.UploadDir == nil {
		http.Error(w, "not receiving", http.StatusNotFound)
		return
	}
	var req struct {
		Name string `json:"name"` // may include folders: "Photos/2026/a.jpg"
		Size int64  `json:"size"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil || req.Size < 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	rel := strings.Trim(path.Clean("/"+strings.ReplaceAll(req.Name, "\\", "/")), "/")
	root := s.o.UploadDir()
	final, err := store.SafeJoin(root, rel)
	if err != nil {
		replyErr(w, err)
		return
	}
	final = freeName(final)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		replyErr(w, err)
		return
	}
	partial := final + store.PartialSuffix
	f, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err == nil {
		err = f.Truncate(req.Size)
	}
	if err != nil {
		replyErr(w, err)
		return
	}
	u := &upload{final: final, partial: partial, f: f, size: req.Size, chunks: map[int64]int64{}, from: clientName(r), touched: time.Now()}
	id := randomID(12)
	s.mu.Lock()
	s.uploads[id] = u
	s.mu.Unlock()
	if req.Size == 0 {
		if err := s.finish(id, u); err != nil {
			replyErr(w, err)
			return
		}
	}
	writeJSON(w, map[string]any{"id": id, "name": filepath.Base(final)})
}

// putChunk writes one chunk at ?off=N. Chunks may arrive in any order and in parallel.
func (s *server) putChunk(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	u := s.uploads[id]
	s.mu.Unlock()
	if u == nil {
		http.Error(w, "unknown upload", http.StatusNotFound)
		return
	}
	off, err := strconv.ParseInt(r.URL.Query().Get("off"), 10, 64)
	if err != nil || off < 0 || r.ContentLength <= 0 || r.ContentLength > maxChunk || off+r.ContentLength > u.size {
		http.Error(w, "bad chunk", http.StatusBadRequest)
		return
	}
	buf := make([]byte, r.ContentLength)
	if _, err := io.ReadFull(r.Body, buf); err != nil {
		http.Error(w, "short chunk", http.StatusBadRequest)
		return
	}
	u.mu.Lock()
	if u.done {
		u.mu.Unlock()
		writeJSON(w, map[string]bool{"done": true})
		return
	}
	if _, err := u.f.WriteAt(buf, off); err != nil {
		u.mu.Unlock()
		replyErr(w, err)
		return
	}
	if _, seen := u.chunks[off]; !seen {
		u.chunks[off] = int64(len(buf))
		u.written += int64(len(buf))
	}
	u.touched = time.Now()
	complete := u.written >= u.size
	u.mu.Unlock()
	if complete {
		if err := s.finish(id, u); err != nil {
			replyErr(w, err)
			return
		}
	}
	writeJSON(w, map[string]bool{"done": complete})
}

func (s *server) finish(id string, u *upload) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.done {
		return nil
	}
	u.done = true
	// Keep answering for a while: a phone that lost the last response retries
	// its chunk and must hear "done", not "unknown upload".
	time.AfterFunc(10*time.Minute, func() {
		s.mu.Lock()
		delete(s.uploads, id)
		s.mu.Unlock()
	})
	if err := u.f.Close(); err != nil {
		return err
	}
	final := freeName(u.final) // something may have appeared there meanwhile
	if err := os.Rename(u.partial, final); err != nil {
		return err
	}
	if s.o.OnUpload != nil {
		s.o.OnUpload(filepath.Base(final), u.size, u.from)
	}
	return nil
}

// freeName returns p, or "name (n).ext" if p exists. Browser uploads never overwrite.
func freeName(p string) string {
	if _, err := os.Lstat(p); err != nil {
		return p
	}
	ext := filepath.Ext(p)
	stem := strings.TrimSuffix(p, ext)
	for n := 1; ; n++ {
		c := fmt.Sprintf("%s (%d)%s", stem, n, ext)
		if _, err := os.Lstat(c); err != nil {
			return c
		}
	}
}

// ---- downloads: this computer → browser ----

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	type item struct {
		I    int    `json:"i"`
		Name string `json:"name"`
		Size int64  `json:"size"`
		Dir  bool   `json:"dir"`
	}
	var items []item
	for i, sh := range s.o.Shared {
		items = append(items, item{I: i, Name: sh.Name, Size: sh.Size, Dir: sh.Dir})
	}
	writeJSON(w, items)
}

func (s *server) download(w http.ResponseWriter, r *http.Request) {
	i, err := strconv.Atoi(r.PathValue("i"))
	if err != nil || i < 0 || i >= len(s.o.Shared) {
		http.NotFound(w, r)
		return
	}
	sh := s.o.Shared[i]
	if !sh.Dir {
		f, err := os.Open(sh.Path)
		if err != nil {
			replyErr(w, err)
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			replyErr(w, err)
			return
		}
		w.Header().Set("Content-Disposition", contentDisposition(sh.Name))
		http.ServeContent(w, r, sh.Name, st.ModTime(), f) // Range requests: resumable
		return
	}
	// Folders stream as an uncompressed zip: no temp file, starts instantly.
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition(sh.Name+".zip"))
	zw := zip.NewWriter(w)
	defer zw.Close()
	base := filepath.Dir(sh.Path)
	_ = filepath.WalkDir(sh.Path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || r.Context().Err() != nil {
			return err
		}
		info, err := d.Info()
		if err != nil || !(info.Mode().IsRegular() || info.IsDir()) {
			return nil
		}
		rel, _ := filepath.Rel(base, p)
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return nil
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
			_, err = zw.CreateHeader(hdr)
			return err
		}
		hdr.Method = zip.Store
		zf, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		_, err = io.Copy(zf, f)
		return err
	})
}

func contentDisposition(name string) string {
	safe := strings.Map(func(r rune) rune {
		if r < 32 || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf(`attachment; filename="%s"`, safe)
}

// ---- helpers ----

func clientName(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ua := r.UserAgent()
	switch {
	case strings.Contains(ua, "iPhone"):
		return "iPhone (" + host + ")"
	case strings.Contains(ua, "iPad"):
		return "iPad (" + host + ")"
	case strings.Contains(ua, "Android"):
		return "Android (" + host + ")"
	}
	return "browser (" + host + ")"
}

func randomID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func replyErr(w http.ResponseWriter, err error) {
	w.WriteHeader(http.StatusUnprocessableEntity)
	var se *store.SpaceError
	if errors.As(err, &se) {
		writeJSON(w, map[string]string{"error": se.Error()})
		return
	}
	writeJSON(w, map[string]string{"error": err.Error()})
}

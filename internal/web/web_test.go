package web

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSplitRoutesByFirstBytes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	peers, browsers := Split(ln)
	defer peers.Close()

	send := func(prefix string) { // runs in a goroutine: report, don't Fatal
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Error(err)
			return
		}
		c.Write([]byte(prefix + "-rest-of-stream"))
		c.Close()
	}
	go send("BFLW")
	c, err := peers.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(c); string(b) != "BFLW-rest-of-stream" {
		t.Fatalf("peer got %q (peeked bytes must be replayed)", b)
	}
	go send("GET ")
	c, err = browsers.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(c); !strings.HasPrefix(string(b), "GET -rest") {
		t.Fatalf("browser got %q", b)
	}
}

type fixture struct {
	srv    *httptest.Server
	client *http.Client
	up     string
	shared string
	got    []string
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{up: t.TempDir(), shared: t.TempDir()}
	must(t, os.WriteFile(filepath.Join(f.shared, "movie.mkv"), bytes.Repeat([]byte("0123456789"), 100_000), 0o644))
	must(t, os.MkdirAll(filepath.Join(f.shared, "Photos", "2026"), 0o755))
	must(t, os.WriteFile(filepath.Join(f.shared, "Photos", "2026", "a.jpg"), []byte("jpeg-a"), 0o644))
	must(t, os.WriteFile(filepath.Join(f.shared, "Photos", "b.jpg"), []byte("jpeg-bb"), 0o644))
	h := NewServer(Options{
		Name:      func() string { return "desk" },
		CheckCode: func(c string) bool { return c == "123456" },
		UploadDir: func() string { return f.up },
		Shared: []Shared{
			{Name: "movie.mkv", Path: filepath.Join(f.shared, "movie.mkv"), Size: 1_000_000},
			{Name: "Photos", Path: filepath.Join(f.shared, "Photos"), Dir: true, Size: 13},
		},
		Binaries: func() []Binary {
			return []Binary{{OS: "linux", Arch: "amd64", Path: filepath.Join(f.shared, "movie.mkv"), Size: 1_000_000}}
		},
		OnUpload: func(name string, size int64, from string) { f.got = append(f.got, name) },
	})
	f.srv = httptest.NewServer(h)
	t.Cleanup(f.srv.Close)
	t.Cleanup(h.Close) // release unfinished uploads before TempDir cleanup
	jar, _ := cookiejar.New(nil)
	f.client = &http.Client{Jar: jar}
	return f
}

func (f *fixture) do(t *testing.T, method, path, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	req.Header.Set("X-Bitflow", "1")
	res, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res, string(b)
}

func TestLoginRequired(t *testing.T) {
	f := newFixture(t)
	if res, _ := f.do(t, "GET", "/web/list", ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("list before login: %d", res.StatusCode)
	}
	if res, _ := f.do(t, "POST", "/web/login", `{"code":"000000"}`); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong code: %d", res.StatusCode)
	}
	if res, _ := f.do(t, "POST", "/web/login", `{"code":"123 456"}`); res.StatusCode != http.StatusOK {
		t.Fatalf("right code: %d", res.StatusCode)
	}
	if res, _ := f.do(t, "GET", "/web/list", ""); res.StatusCode != http.StatusOK {
		t.Fatalf("list after login: %d", res.StatusCode)
	}
}

func TestUploadChunksOutOfOrder(t *testing.T) {
	f := newFixture(t)
	f.do(t, "POST", "/web/login", `{"code":"123456"}`)
	data := bytes.Repeat([]byte("abcdefgh"), 3000) // 24000 bytes
	_, body := f.do(t, "POST", "/web/upload", `{"name":"trip/clip.mov","size":24000}`)
	id := between(body, `"id":"`, `"`)
	for _, off := range []int{16000, 0, 8000, 0} { // out of order, one retried
		res, _ := f.do(t, "PUT", fmt.Sprintf("/web/upload/%s?off=%d", id, off), string(data[off:off+8000]))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("chunk %d: %d", off, res.StatusCode)
		}
	}
	got, err := os.ReadFile(filepath.Join(f.up, "trip", "clip.mov"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("uploaded file wrong: %v", err)
	}
	if len(f.got) != 1 || f.got[0] != "trip/clip.mov" {
		t.Fatalf("OnUpload calls: %v", f.got)
	}

	// The same name again is kept alongside, never overwritten.
	_, body = f.do(t, "POST", "/web/upload", `{"name":"trip/clip.mov","size":0}`)
	if !strings.Contains(body, "clip (1).mov") {
		t.Fatalf("second upload: %s", body)
	}
}

func TestUploadRejectsEscapes(t *testing.T) {
	f := newFixture(t)
	f.do(t, "POST", "/web/login", `{"code":"123456"}`)
	// Escaping names are cleaned into the upload folder; nothing may land outside it.
	for _, name := range []string{"../../etc/x", "/abs/y", `..\..\z`} {
		f.do(t, "POST", "/web/upload", `{"name":"`+strings.ReplaceAll(name, `\`, `\\`)+`","size":1}`)
	}
	for _, p := range []string{"etc/x", "abs/y", "z"} {
		if _, err := os.Stat(filepath.Join(f.up, p+".bitflow-partial")); err != nil {
			t.Errorf("%s should have been placed inside the upload folder: %v", p, err)
		}
	}
	for _, p := range []string{"etc", "abs", "z.bitflow-partial"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(f.up), p)); err == nil {
			t.Errorf("%s escaped the upload folder", p)
		}
	}
}

func TestDownloadRangeAndZip(t *testing.T) {
	f := newFixture(t)
	f.do(t, "POST", "/web/login", `{"code":"123456"}`)
	req, _ := http.NewRequest("GET", f.srv.URL+"/web/dl/0", nil)
	req.Header.Set("Range", "bytes=10-19")
	res, err := f.client.Do(req)
	must(t, err)
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusPartialContent || string(b) != "0123456789" {
		t.Fatalf("range: %d %q", res.StatusCode, b)
	}

	res2, body := f.do(t, "GET", "/web/dl/1", "")
	if res2.Header.Get("Content-Type") != "application/zip" {
		t.Fatalf("folder download type %q", res2.Header.Get("Content-Type"))
	}
	zr, err := zip.NewReader(bytes.NewReader([]byte(body)), int64(len(body)))
	must(t, err)
	names := map[string]bool{}
	for _, zf := range zr.File {
		names[zf.Name] = true
	}
	if !names["Photos/2026/a.jpg"] || !names["Photos/b.jpg"] {
		t.Fatalf("zip entries %v", names)
	}
}

func TestInstallerAndBinaries(t *testing.T) {
	f := newFixture(t)
	res, body := f.do(t, "GET", "/get/sh", "")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "/get/$os-$arch") {
		t.Fatalf("installer: %d", res.StatusCode)
	}
	if res, _ := f.do(t, "GET", "/get/linux-amd64", ""); res.StatusCode != http.StatusOK {
		t.Fatalf("binary: %d", res.StatusCode)
	}
	if res, _ := f.do(t, "GET", "/get/plan9-386", ""); res.StatusCode != http.StatusNotFound {
		t.Fatalf("missing binary: %d", res.StatusCode)
	}
	for _, h := range []string{"evil.com\"; rm -rf ~; \"", "a b", ""} {
		if hostOK(h) {
			t.Fatalf("hostOK(%q)", h)
		}
	}
	if !hostOK("192.168.1.40:7700") {
		t.Fatal("normal host rejected")
	}
}

func TestAbandonedUploadsDropped(t *testing.T) {
	up := t.TempDir()
	srv := NewServer(Options{Name: func() string { return "x" }, CheckCode: func(string) bool { return true }, UploadDir: func() string { return up }})
	defer srv.Close()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	post := func(path, body string) {
		req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(body))
		req.Header.Set("X-Bitflow", "1")
		res, err := c.Do(req)
		must(t, err)
		res.Body.Close()
	}
	post("/web/login", `{"code":"1"}`)
	post("/web/upload", `{"name":"half.bin","size":100}`)
	partial := filepath.Join(up, "half.bin.bitflow-partial")
	if _, err := os.Stat(partial); err != nil {
		t.Fatal("partial file should exist while the upload is open")
	}
	srv.s.dropUploads(func(u *upload) bool { return time.Since(u.touched) >= 0 }) // as if idle long enough
	if _, err := os.Stat(partial); err == nil {
		t.Fatal("abandoned upload's partial file should be removed")
	}
}

func TestQR(t *testing.T) {
	if q := TerminalQR("http://192.168.1.40:7700"); strings.Count(q, "\n") < 10 {
		t.Fatalf("terminal QR too small:\n%s", q)
	}
	if s := SVGQR("http://192.168.1.40:7700"); !strings.HasPrefix(s, "<svg") || !strings.Contains(s, "path") {
		t.Fatal("bad SVG QR")
	}
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	j := strings.Index(s, b)
	if j < 0 {
		return ""
	}
	return s[:j]
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

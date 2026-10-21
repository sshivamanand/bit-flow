package transfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sshivamanand/bit-flow/internal/planner"
	"github.com/sshivamanand/bit-flow/internal/proto"
	"github.com/sshivamanand/bit-flow/internal/secure"
	"github.com/sshivamanand/bit-flow/internal/store"
)

const testCode = "424242"

// startReceiver runs a receiver on a random loopback port. With once set, done
// yields the first transfer's error.
func startReceiver(t *testing.T, r *Receiver, once bool) (addr string, done <-chan error) {
	t.Helper()
	if r.Code == "" {
		r.Code = testCode
	}
	if r.StateDir == "" {
		r.StateDir = t.TempDir()
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { ch <- r.Serve(ctx, ln, once) }()
	return ln.Addr().String(), ch
}

// small chunks and blocks so files split into many units across streams.
func testOpts() Options {
	return Options{Code: testCode, Streams: 4, ChunkSize: 1 << 20, BlockSize: 256 << 10, Retries: -1}
}

func writeRandom(t *testing.T, path string, size int, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, size)
	_, _ = rand.Read(b)
	if err := os.WriteFile(path, b, mode); err != nil {
		t.Fatal(err)
	}
}

// makeGame builds a tree with big, odd-sized, small, empty and executable files.
func makeGame(t *testing.T) string {
	src := filepath.Join(t.TempDir(), "Game")
	writeRandom(t, filepath.Join(src, "big.pak"), 9<<20+123, 0o644)
	writeRandom(t, filepath.Join(src, "data", "textures.bin"), 2<<20+1, 0o644)
	writeRandom(t, filepath.Join(src, "data", "deep", "level.dat"), 300_001, 0o644)
	writeRandom(t, filepath.Join(src, "run.sh"), 64, 0o755)
	for i := range 300 {
		writeRandom(t, filepath.Join(src, "small", strings.Repeat("f", 1+i%5)+string(rune('a'+i%26))+string(rune('a'+i/26))+".txt"), i*37, 0o644)
	}
	writeRandom(t, filepath.Join(src, "empty.txt"), 0, 0o644)
	if err := os.MkdirAll(filepath.Join(src, "saves", "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	return src
}

// assertSameTree checks every file and directory under src exists identically under dst.
func assertSameTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		got := filepath.Join(dst, rel)
		gi, err := os.Stat(got)
		if err != nil {
			t.Errorf("missing %s", rel)
			return nil
		}
		si, _ := d.Info()
		if d.IsDir() {
			if !gi.IsDir() {
				t.Errorf("%s should be a directory", rel)
			}
			return nil
		}
		want, _ := os.ReadFile(path)
		have, _ := os.ReadFile(got)
		if !bytes.Equal(want, have) {
			t.Errorf("%s content differs", rel)
		}
		if si.Mode().Perm() != gi.Mode().Perm() {
			t.Errorf("%s mode %v, want %v", rel, gi.Mode().Perm(), si.Mode().Perm())
		}
		if !si.ModTime().Equal(gi.ModTime()) {
			t.Errorf("%s mtime %v, want %v", rel, gi.ModTime(), si.ModTime())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNoPartials(t, dst)
}

func assertNoPartials(t *testing.T, dir string) {
	t.Helper()
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if strings.HasSuffix(path, store.PartialSuffix) {
			t.Errorf("leftover partial file %s", path)
		}
		return nil
	})
}

func send(t *testing.T, addr string, o Options, paths ...string) (Result, error) {
	t.Helper()
	m, err := planner.Build(paths)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return NewSender(o).Send(ctx, addr, m)
}

func TestSendTree(t *testing.T) {
	for _, raw := range []bool{false, true} {
		name := "tls"
		if raw {
			name = "raw"
		}
		t.Run(name, func(t *testing.T) {
			src := makeGame(t)
			dst := t.TempDir()
			addr, done := startReceiver(t, &Receiver{Dest: dst, AllowRaw: raw}, true)
			o := testOpts()
			o.Raw = raw
			res, err := send(t, addr, o, src)
			if err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatalf("receiver: %v", err)
			}
			m, _ := planner.Build([]string{src})
			if res.Bytes != m.Total || res.Files != m.FileCount() || res.Already != 0 {
				t.Fatalf("result %+v, manifest total %d files %d", res, m.Total, m.FileCount())
			}
			assertSameTree(t, src, filepath.Join(dst, "Game"))
		})
	}
}

func TestWrongCode(t *testing.T) {
	src := makeGame(t)
	dst := t.TempDir()
	addr, done := startReceiver(t, &Receiver{Dest: dst}, true)

	o := testOpts()
	o.Code = "111111"
	o.Retries = 3 // a wrong code must not be retried
	start := time.Now()
	_, err := send(t, addr, o, src)
	if !errors.Is(err, secure.ErrWrongCode) || !IsPermanent(err) {
		t.Fatalf("want permanent wrong-code error, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("wrong code was retried")
	}
	// The receiver keeps waiting for the right sender.
	if _, err := send(t, addr, testOpts(), src); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertSameTree(t, src, filepath.Join(dst, "Game"))
}

func TestCodeRotatesAfterFailures(t *testing.T) {
	rotated := make(chan string, 1)
	r := &Receiver{Dest: t.TempDir(), OnCodeChange: func(c string) { rotated <- c }}
	addr, _ := startReceiver(t, r, false)
	src := makeGame(t)
	o := testOpts()
	o.Code = "000000"
	for range maxPairFailures {
		if _, err := send(t, addr, o, src); !errors.Is(err, secure.ErrWrongCode) {
			t.Fatalf("got %v", err)
		}
	}
	select {
	case c := <-rotated:
		if c == testCode || r.CurrentCode() != c {
			t.Fatalf("code did not rotate: %q", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("code was not rotated")
	}
}

func TestIdenticalFilesSkipped(t *testing.T) {
	src := makeGame(t)
	dst := t.TempDir()
	addr, _ := startReceiver(t, &Receiver{Dest: dst}, false)
	if _, err := send(t, addr, testOpts(), src); err != nil {
		t.Fatal(err)
	}
	res, err := send(t, addr, testOpts(), src)
	if err != nil {
		t.Fatal(err)
	}
	if res.Bytes != 0 || res.Already == 0 {
		t.Fatalf("second send should move nothing: %+v", res)
	}
	assertSameTree(t, src, filepath.Join(dst, "Game"))
}

func TestConflictPolicies(t *testing.T) {
	src := makeGame(t)
	setup := func(t *testing.T) string {
		dst := t.TempDir()
		writeRandom(t, filepath.Join(dst, "Game", "run.sh"), 1, 0o644) // differs from the source
		return dst
	}

	t.Run("error", func(t *testing.T) {
		dst := setup(t)
		addr, done := startReceiver(t, &Receiver{Dest: dst}, true)
		o := testOpts()
		o.Retries = 3
		_, err := send(t, addr, o, src)
		if err == nil || !strings.Contains(err.Error(), "already exist") || !IsPermanent(err) {
			t.Fatalf("want permanent conflict error, got %v", err)
		}
		<-done
		if b, _ := os.ReadFile(filepath.Join(dst, "Game", "run.sh")); len(b) != 1 {
			t.Fatal("existing file was modified")
		}
	})
	t.Run("skip", func(t *testing.T) {
		dst := setup(t)
		addr, done := startReceiver(t, &Receiver{Dest: dst, Policy: store.PolicySkip}, true)
		if _, err := send(t, addr, testOpts(), src); err != nil {
			t.Fatal(err)
		}
		<-done
		if b, _ := os.ReadFile(filepath.Join(dst, "Game", "run.sh")); len(b) != 1 {
			t.Fatal("existing file was modified")
		}
		if _, err := os.Stat(filepath.Join(dst, "Game", "big.pak")); err != nil {
			t.Fatal("other files should still arrive")
		}
	})
	t.Run("rename", func(t *testing.T) {
		dst := setup(t)
		addr, done := startReceiver(t, &Receiver{Dest: dst, Policy: store.PolicyRename}, true)
		if _, err := send(t, addr, testOpts(), src); err != nil {
			t.Fatal(err)
		}
		<-done
		assertSameTree(t, src, filepath.Join(dst, "Game (1)"))
		if b, _ := os.ReadFile(filepath.Join(dst, "Game", "run.sh")); len(b) != 1 {
			t.Fatal("existing file was modified")
		}
	})
	t.Run("overwrite", func(t *testing.T) {
		dst := setup(t)
		addr, done := startReceiver(t, &Receiver{Dest: dst, Policy: store.PolicyOverwrite}, true)
		if _, err := send(t, addr, testOpts(), src); err != nil {
			t.Fatal(err)
		}
		<-done
		assertSameTree(t, src, filepath.Join(dst, "Game"))
	})
}

func TestDeclined(t *testing.T) {
	src := makeGame(t)
	dst := t.TempDir()
	addr, done := startReceiver(t, &Receiver{Dest: dst, Confirm: func(*proto.Offer, OfferInfo) bool { return false }}, true)
	_, err := send(t, addr, testOpts(), src)
	if err == nil || !strings.Contains(err.Error(), "declined") || !IsPermanent(err) {
		t.Fatalf("want declined error, got %v", err)
	}
	<-done
	if entries, _ := os.ReadDir(dst); len(entries) != 0 {
		t.Fatalf("declined transfer created files: %v", entries)
	}
}

func TestBench(t *testing.T) {
	for _, raw := range []bool{false, true} {
		addr, done := startReceiver(t, &Receiver{AllowRaw: true}, true)
		o := Options{Code: testCode, Streams: 3, ChunkSize: 3 << 20, BlockSize: 1 << 20, Raw: raw}
		res, err := NewSender(o).Bench(context.Background(), addr, 50<<20+7)
		if err != nil {
			t.Fatal(err)
		}
		if res.Bytes != 50<<20+7 {
			t.Fatalf("bench moved %d bytes", res.Bytes)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// fakeSender pairs and offers like a real sender, then hands back one open data stream.
func fakeSender(t *testing.T, addr string, offer proto.Offer) (ctrl, data net.Conn) {
	t.Helper()
	id, _ := secure.NewIdentity()
	var sid proto.SessionID
	_, _ = rand.Read(sid[:])
	c, err := net.Dial("tcp", addr)
	must(t, err)
	must(t, proto.WriteHello(c, proto.Hello{Kind: proto.KindControl, Session: sid}))
	paired, err := secure.PairDialer(c, testCode, sid, id)
	must(t, err)
	tc := id.Client(c, paired.PeerFP)
	must(t, proto.WriteMsg(tc, proto.MsgOffer, offer))
	if typ, _, err := proto.ReadMsg(tc); err != nil || typ != proto.MsgAccept {
		t.Fatalf("typ=%d err=%v", typ, err)
	}
	d, err := net.Dial("tcp", addr)
	must(t, err)
	must(t, proto.WriteHello(d, proto.Hello{Kind: proto.KindData, Session: sid}))
	td := id.Client(d, paired.PeerFP)
	must(t, td.Handshake())
	return tc, td
}

func TestResumeAfterSenderVanishes(t *testing.T) {
	src := makeGame(t)
	dst := t.TempDir()
	state := t.TempDir()
	m, _ := planner.Build([]string{src})

	// Deliver the first 2 MiB of big.pak, then disappear.
	addr, done := startReceiver(t, &Receiver{Dest: dst, StateDir: state}, true)
	o := testOpts()
	offer := proto.Offer{From: "flaky", Files: m.Files, Total: m.Total, Streams: 1, BlockSize: o.BlockSize}
	ctrl, data := fakeSender(t, addr, offer)
	var big uint32
	for i, f := range m.Files {
		if f.Path == "Game/big.pak" {
			big = uint32(i)
		}
	}
	content, _ := os.ReadFile(filepath.Join(src, "big.pak"))
	for off := 0; off < 2<<20; off += o.BlockSize {
		block := content[off : off+o.BlockSize]
		frame := make([]byte, proto.FrameHeaderSize+len(block))
		(&proto.FrameHeader{File: big, Offset: uint64(off), Len: uint32(len(block)), Sum: proto.Checksum(block)}).Put(frame)
		copy(frame[proto.FrameHeaderSize:], block)
		_, err := data.Write(frame)
		must(t, err)
	}
	time.Sleep(200 * time.Millisecond) // let the blocks reach the partial file
	data.Close()
	ctrl.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("receiver should report the interrupted transfer")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("receiver did not notice the sender leaving")
	}
	if _, err := os.Stat(filepath.Join(dst, "Game", "big.pak"+store.PartialSuffix)); err != nil {
		t.Fatal("partial file should be kept for resume")
	}

	// A real sender now resumes: the first 2 MiB are not sent again.
	addr, done = startReceiver(t, &Receiver{Dest: dst, StateDir: state}, true)
	res, err := send(t, addr, testOpts(), src)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if res.Already != 2<<20 || res.Bytes != m.Total-2<<20 {
		t.Fatalf("resume moved %d bytes with %d already there", res.Bytes, res.Already)
	}
	assertSameTree(t, src, filepath.Join(dst, "Game"))
	if entries, _ := os.ReadDir(filepath.Join(state, "resume")); len(entries) != 0 {
		t.Fatalf("journal should be removed after success: %v", entries)
	}
}

// flakyProxy forwards TCP connections and cuts all of them once, after limit
// bytes have flowed from the sender.
type flakyProxy struct {
	ln      net.Listener
	backend string
	limit   int64
	sent    atomic.Int64
	tripped atomic.Bool
	mu      sync.Mutex
	conns   []net.Conn
}

func newFlakyProxy(t *testing.T, backend string, limit int64) *flakyProxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	p := &flakyProxy{ln: ln, backend: backend, limit: limit}
	t.Cleanup(func() { ln.Close() })
	go p.serve()
	return p
}

func (p *flakyProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		b, err := net.Dial("tcp", p.backend)
		if err != nil {
			c.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, c, b)
		p.mu.Unlock()
		go func() { io.Copy(c, b); c.Close() }()
		go func() {
			buf := make([]byte, 64<<10)
			for {
				n, err := c.Read(buf)
				if n > 0 {
					if _, werr := b.Write(buf[:n]); werr != nil {
						break
					}
					if p.sent.Add(int64(n)) > p.limit && p.tripped.CompareAndSwap(false, true) {
						p.cut()
					}
				}
				if err != nil {
					break
				}
			}
			b.Close()
		}()
	}
}

func (p *flakyProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.Close()
	}
	p.conns = nil
}

func TestReconnectResumes(t *testing.T) {
	src := makeGame(t)
	dst := t.TempDir()
	addr, _ := startReceiver(t, &Receiver{Dest: dst}, false)
	proxy := newFlakyProxy(t, addr, 6<<20)

	var retries atomic.Int32
	o := testOpts()
	o.Retries = 5
	o.OnRetry = func(int, error, time.Duration) { retries.Add(1) }
	res, err := send(t, proxy.ln.Addr().String(), o, src)
	if err != nil {
		t.Fatal(err)
	}
	if retries.Load() == 0 {
		t.Fatal("expected at least one reconnect")
	}
	m, _ := planner.Build([]string{src})
	if res.Bytes+res.Already != m.Total {
		t.Fatalf("result %+v doesn't add up to %d", res, m.Total)
	}
	assertSameTree(t, src, filepath.Join(dst, "Game"))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestNoReceiverFailsFast(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	o := testOpts()
	o.Retries = 5
	start := time.Now()
	if _, err := send(t, addr, o, makeGame(t)); err == nil {
		t.Fatal("expected an error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("unreachable receiver should not be retried")
	}
}

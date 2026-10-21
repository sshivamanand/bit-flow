package transfer

import (
	"context"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sshivamanand/bit-flow/internal/planner"
)

// makeCompressible builds a tree of text-like files that zstd shrinks a lot.
func makeCompressible(t *testing.T) string {
	src := filepath.Join(t.TempDir(), "Logs")
	if err := os.MkdirAll(filepath.Join(src, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	line := "2026-10-21T12:00:00Z INFO bit-flow transferred chunk ok streams=8 rate=112MB/s\n"
	big := strings.Repeat(line, 120_000) // ~9.6 MB
	must(t, os.WriteFile(filepath.Join(src, "app.log"), []byte(big), 0o644))
	for i := range 50 {
		must(t, os.WriteFile(filepath.Join(src, "old", "part"+string(rune('a'+i%26))+string(rune('a'+i/26))+".log"), []byte(strings.Repeat(line, 100+i)), 0o644))
	}
	// One incompressible file that must still arrive intact.
	noise := make([]byte, 3<<20)
	_, _ = rand.Read(noise)
	must(t, os.WriteFile(filepath.Join(src, "noise.bin"), noise, 0o644))
	return src
}

func TestCompressedTransfer(t *testing.T) {
	src := makeCompressible(t)
	dst := t.TempDir()
	addr, done := startReceiver(t, &Receiver{Dest: dst}, true)
	o := testOpts()
	o.Compress = CompressOn
	m, _ := planner.Build([]string{src})
	s := NewSender(o)
	if _, err := s.Send(context.Background(), addr, m); err != nil {
		t.Fatal(err)
	}
	must(t, <-done)
	assertSameTree(t, src, filepath.Join(dst, "Logs"))
	if st := s.Stats(); st.Saved < 5<<20 {
		t.Fatalf("compression saved only %d bytes", st.Saved)
	}
}

func TestCompressOffSendsRaw(t *testing.T) {
	src := makeCompressible(t)
	dst := t.TempDir()
	addr, done := startReceiver(t, &Receiver{Dest: dst}, true)
	o := testOpts()
	o.Compress = CompressOff
	m, _ := planner.Build([]string{src})
	s := NewSender(o)
	if _, err := s.Send(context.Background(), addr, m); err != nil {
		t.Fatal(err)
	}
	must(t, <-done)
	assertSameTree(t, src, filepath.Join(dst, "Logs"))
	if st := s.Stats(); st.Saved != 0 {
		t.Fatalf("compression was off but saved %d", st.Saved)
	}
}

func TestCompressorAutoPausesOnRandomData(t *testing.T) {
	c := newCompressor(CompressAuto, nil)
	raw := make([]byte, 4<<20)
	_, _ = rand.Read(raw)
	dst := make([]byte, 0, len(raw)+len(raw)/64+1024)
	for range compressWindow/len(raw) + 2 {
		if _, ok := c.compress(raw, dst, -1); ok {
			t.Fatal("random data should not be sent compressed")
		}
	}
	c.adjust(50e6, 4) // Wi-Fi speed: CPU isn't the issue, the data is
	if c.enabled.Load() || c.cpuBound.Load() {
		t.Fatalf("expected a pause for incompressible data (enabled=%v cpuBound=%v)", c.enabled.Load(), c.cpuBound.Load())
	}
	c.pausedUntil.Store(time.Now().UnixNano() - 1)
	c.adjust(50e6, 4)
	if !c.enabled.Load() {
		t.Fatal("compression should be retried after the pause")
	}
}

func TestCompressorStopsWhenLinkIsFaster(t *testing.T) {
	c := newCompressor(CompressAuto, nil)
	raw := []byte(strings.Repeat("compressible line of text\n", 200_000))
	dst := make([]byte, 0, len(raw)+len(raw)/64+1024)
	for range 20 {
		c.compress(raw, dst, -1)
	}
	c.adjust(1e12, 1) // a link far faster than one core of zstd
	if c.enabled.Load() || !c.cpuBound.Load() {
		t.Fatal("compression should stop for good when it is the bottleneck")
	}
}

func TestIncompressibleExtensionsSkipped(t *testing.T) {
	c := newCompressor(CompressOn, []string{"movie.mp4", "level.dat"})
	if c.wants(0) {
		t.Fatal(".mp4 should never be compressed")
	}
	if !c.wants(1) || !c.wants(-1) {
		t.Fatal("other files and packs should be tried")
	}
}

func TestGate(t *testing.T) {
	g := newGate(1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !g.wait(ctx, 0) {
		t.Fatal("stream 0 should pass")
	}
	passed := make(chan bool, 1)
	go func() { passed <- g.wait(ctx, 3) }()
	select {
	case <-passed:
		t.Fatal("stream 3 should be held back")
	case <-time.After(50 * time.Millisecond):
	}
	g.setLimit(4)
	if !<-passed {
		t.Fatal("stream 3 should pass after the limit rises")
	}
	g.release()
	if !g.wait(ctx, 99) {
		t.Fatal("release should let everyone through")
	}
}

func TestTunerGrowsThenSettles(t *testing.T) {
	g := newGate(4)
	tn := newTuner(g, 16)
	now := time.Now()
	done := int64(0)
	sample := func(rate float64) {
		now = now.Add(time.Second)
		done += int64(rate)
		tn.observe(done, now)
	}
	sample(0)     // first sample only sets the baseline
	sample(100e6) // 4 streams: 100 MB/s → try 8
	if g.current() != 8 {
		t.Fatalf("limit %d, want 8", g.current())
	}
	sample(150e6) // 8 streams: +50% → try 16
	if g.current() != 16 {
		t.Fatalf("limit %d, want 16", g.current())
	}
	sample(152e6) // 16 streams: no real gain → back to 8, frozen
	if g.current() != 8 || !tn.frozen {
		t.Fatalf("limit %d frozen %v, want 8 frozen", g.current(), tn.frozen)
	}
	sample(400e6)
	if g.current() != 8 {
		t.Fatal("a frozen tuner must not change the limit")
	}
}

func TestAutoStreams(t *testing.T) {
	src := makeGame(t)
	dst := t.TempDir()
	addr, done := startReceiver(t, &Receiver{Dest: dst}, true)
	o := testOpts()
	o.Streams = 0 // auto
	if _, err := send(t, addr, o, src); err != nil {
		t.Fatal(err)
	}
	must(t, <-done)
	assertSameTree(t, src, filepath.Join(dst, "Game"))
}

func TestMultipath(t *testing.T) {
	var lan string
	for _, a := range localAddrs() {
		if ip := net.ParseIP(a.IP); ip != nil && !ip.IsLoopback() && ip.To4() != nil {
			lan = a.IP
			break
		}
	}
	if lan == "" {
		t.Skip("no non-loopback IPv4 address to use as a second path")
	}
	// Listen on every interface so both 127.0.0.1 and the LAN address reach the receiver.
	ln, err := net.Listen("tcp", ":0")
	must(t, err)
	r := &Receiver{Dest: t.TempDir(), Code: testCode, StateDir: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Serve(ctx, ln, false)
	port := ln.Addr().(*net.TCPAddr).Port

	paths := selectPaths(net.JoinHostPort("127.0.0.1", itoa(port)), localAddrs())
	if len(paths) < 2 {
		t.Skipf("only one usable path: %+v", paths)
	}
	src := makeGame(t)
	m, _ := planner.Build([]string{src})
	o := testOpts()
	o.Streams = 4
	s := NewSender(o)
	if _, err := s.Send(ctx, net.JoinHostPort("127.0.0.1", itoa(port)), m); err != nil {
		t.Fatal(err)
	}
	st := s.Stats()
	if len(st.Paths) < 2 {
		t.Fatalf("expected traffic on 2+ links, got %+v", st.Paths)
	}
	for _, p := range st.Paths {
		if p.Bytes == 0 {
			t.Fatalf("link %q carried nothing: %+v", p.Label, st.Paths)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

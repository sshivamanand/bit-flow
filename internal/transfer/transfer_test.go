package transfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sshivamanand/bit-flow/internal/planner"
	"github.com/sshivamanand/bit-flow/internal/proto"
)

// startReceiver runs a once-mode receiver on a random loopback port.
func startReceiver(t *testing.T, r *Receiver) (addr string, done <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { ch <- r.Serve(ctx, ln, true) }()
	return ln.Addr().String(), ch
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
	for i := range 50 {
		writeRandom(t, filepath.Join(src, "small", strings.Repeat("f", 1+i%5)+string(rune('a'+i%26))+".txt"), i*37, 0o644)
	}
	writeRandom(t, filepath.Join(src, "empty.txt"), 0, 0o644)
	if err := os.MkdirAll(filepath.Join(src, "saves", "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	return src
}

func TestSendTree(t *testing.T) {
	src := makeGame(t)
	dst := t.TempDir()
	addr, done := startReceiver(t, &Receiver{Dest: dst})

	m, err := planner.Build([]string{src})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Small chunks and blocks so files split into many units across streams.
	s := NewSender(Options{Streams: 4, ChunkSize: 1 << 20, BlockSize: 256 << 10})
	res, err := s.Send(ctx, addr, m)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("receiver: %v", err)
	}
	if res.Bytes != m.Total || res.Files != m.FileCount() {
		t.Fatalf("result %+v, manifest total %d files %d", res, m.Total, m.FileCount())
	}

	// Every file and directory must match, with no partial files left behind.
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(filepath.Dir(src), path)
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
	filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
		if strings.HasSuffix(path, ".bitflow-partial") {
			t.Errorf("leftover partial file %s", path)
		}
		return nil
	})
}

func TestConflictRejected(t *testing.T) {
	src := makeGame(t)
	dst := t.TempDir()
	writeRandom(t, filepath.Join(dst, "Game", "run.sh"), 1, 0o644)
	addr, done := startReceiver(t, &Receiver{Dest: dst})

	m, _ := planner.Build([]string{src})
	_, err := NewSender(Options{Streams: 2}).Send(context.Background(), addr, m)
	if err == nil || !strings.Contains(err.Error(), "already exist") {
		t.Fatalf("want conflict error, got %v", err)
	}
	<-done
	if b, _ := os.ReadFile(filepath.Join(dst, "Game", "run.sh")); len(b) != 1 {
		t.Fatal("existing file was modified")
	}
}

func TestDeclined(t *testing.T) {
	src := makeGame(t)
	dst := t.TempDir()
	addr, done := startReceiver(t, &Receiver{Dest: dst, Confirm: func(*proto.Offer) bool { return false }})

	m, _ := planner.Build([]string{src})
	_, err := NewSender(Options{}).Send(context.Background(), addr, m)
	if err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("want declined error, got %v", err)
	}
	<-done
	if entries, _ := os.ReadDir(dst); len(entries) != 0 {
		t.Fatalf("declined transfer created files: %v", entries)
	}
}

func TestBench(t *testing.T) {
	addr, done := startReceiver(t, &Receiver{})
	res, err := NewSender(Options{Streams: 3, ChunkSize: 3 << 20, BlockSize: 1 << 20}).Bench(context.Background(), addr, 50<<20+7)
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

func TestSenderDisappears(t *testing.T) {
	// A sender that delivers one block and then vanishes must fail the transfer
	// without leaving partial files behind.
	dst := t.TempDir()
	addr, done := startReceiver(t, &Receiver{Dest: dst})

	block := make([]byte, 64<<10)
	offer := proto.Offer{From: "flaky", Total: 1 << 20, Streams: 1, BlockSize: len(block),
		Files: []proto.FileEntry{{Path: "Game", Dir: true}, {Path: "Game/big.pak", Size: 1 << 20}}}

	var sid proto.SessionID
	sid[0] = 1
	ctrl, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	must(t, proto.WriteHello(ctrl, proto.Hello{Kind: proto.KindControl, Session: sid}))
	must(t, proto.WriteMsg(ctrl, proto.MsgOffer, offer))
	if typ, _, err := proto.ReadMsg(ctrl); err != nil || typ != proto.MsgAccept {
		t.Fatalf("typ=%d err=%v", typ, err)
	}
	data, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	must(t, proto.WriteHello(data, proto.Hello{Kind: proto.KindData, Session: sid}))
	var hdr [proto.FrameHeaderSize]byte
	(&proto.FrameHeader{File: 1, Len: uint32(len(block)), Sum: proto.Checksum(block)}).Put(hdr[:])
	_, err = (&net.Buffers{hdr[:], block}).WriteTo(data)
	must(t, err)
	time.Sleep(100 * time.Millisecond) // let the block land in the partial file
	data.Close()
	ctrl.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("receiver should report the failed transfer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("receiver did not notice the sender leaving")
	}
	filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
		if strings.HasSuffix(path, ".bitflow-partial") || strings.HasSuffix(path, "big.pak") {
			t.Errorf("leftover file %s", path)
		}
		return nil
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

package proto

import (
	"bytes"
	"testing"
)

func TestHelloRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := Hello{Kind: KindData, Stream: 7}
	in.Session[0], in.Session[15] = 0xAB, 0xCD
	if err := WriteHello(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadHello(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("got %+v, want %+v", out, in)
	}
}

func TestHelloRejectsStranger(t *testing.T) {
	if _, err := ReadHello(bytes.NewReader(make([]byte, 24))); err == nil {
		t.Fatal("expected error for bad magic")
	}
}

func TestFrameHeaderRoundTrip(t *testing.T) {
	in := FrameHeader{File: 42, Offset: 1 << 40, Len: 4 << 20, Flags: FlagEnd, Sum: Checksum([]byte("hello"))}
	var b [FrameHeaderSize]byte
	in.Put(b[:])
	if out := ParseFrameHeader(b[:]); out != in {
		t.Fatalf("got %+v, want %+v", out, in)
	}
}

func TestMsgRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := Offer{From: "laptop", Total: 10, Streams: 8, BlockSize: 1 << 22,
		Files: []FileEntry{{Path: "Game", Dir: true}, {Path: "Game/a.pak", Size: 10, Mode: 0o644}}}
	if err := WriteMsg(&buf, MsgOffer, in); err != nil {
		t.Fatal(err)
	}
	typ, body, err := ReadMsg(&buf)
	if err != nil || typ != MsgOffer {
		t.Fatalf("typ=%d err=%v", typ, err)
	}
	var out Offer
	if err := Decode(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.From != in.From || len(out.Files) != 2 || out.Files[1] != in.Files[1] {
		t.Fatalf("got %+v", out)
	}
}

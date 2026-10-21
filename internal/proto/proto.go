// Package proto defines the bit-flow wire protocol.
//
// Every TCP connection starts with a fixed 24-byte Hello identifying it as the
// session's control connection or as one of its N data streams. The control
// connection then pairs (see package secure), upgrades to TLS, and carries
// length-prefixed JSON messages (offer, accept, progress, complete, error).
// Data streams carry back-to-back frames: a fixed 33-byte header followed by
// the block payload, terminated by an END frame. A PACK frame carries several
// whole small files.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/zeebo/xxh3"
)

const (
	Magic       = "BFLW"
	Version     = 3
	DefaultPort = 7700
)

// ConnKind says what a connection is used for.
type ConnKind uint8

const (
	KindControl ConnKind = 1
	KindData    ConnKind = 2 // TLS data stream
	KindDataRaw ConnKind = 3 // raw data stream, authenticated by a token
	KindProbe   ConnKind = 4 // discovery: answer with device info, then close
)

// SessionID ties a transfer's data streams to its control connection.
type SessionID [16]byte

// Hello is the first thing written on every connection.
type Hello struct {
	Kind    ConnKind
	Session SessionID
	Stream  uint16
}

const helloSize = 4 + 1 + 1 + 16 + 2

func WriteHello(w io.Writer, h Hello) error {
	var b [helloSize]byte
	copy(b[:4], Magic)
	b[4] = Version
	b[5] = byte(h.Kind)
	copy(b[6:22], h.Session[:])
	binary.BigEndian.PutUint16(b[22:], h.Stream)
	_, err := w.Write(b[:])
	return err
}

func ReadHello(r io.Reader) (Hello, error) {
	var b [helloSize]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return Hello{}, err
	}
	if string(b[:4]) != Magic {
		return Hello{}, errors.New("not a bit-flow peer")
	}
	if b[4] != Version {
		return Hello{}, fmt.Errorf("protocol version mismatch: peer v%d, we v%d", b[4], Version)
	}
	h := Hello{Kind: ConnKind(b[5]), Stream: binary.BigEndian.Uint16(b[22:])}
	copy(h.Session[:], b[6:22])
	return h, nil
}

// ---- control messages ----

type MsgType uint8

const (
	MsgOffer MsgType = iota + 1
	MsgAccept
	MsgProgress
	MsgComplete
	MsgError
	MsgInfo // discovery probe answer
)

// maxMsg bounds a control message; a manifest of ~1M files fits comfortably.
const maxMsg = 512 << 20

// FileEntry describes one file or directory in an offer. Paths are relative,
// slash-separated, and always start with the name of a top-level item the sender picked.
type FileEntry struct {
	Path  string `json:"p"`
	Size  int64  `json:"s,omitempty"`
	Mode  uint32 `json:"m,omitempty"` // permission bits
	MTime int64  `json:"t,omitempty"` // unix nanoseconds
	Dir   bool   `json:"d,omitempty"`
}

type Offer struct {
	From      string      `json:"from"`
	Files     []FileEntry `json:"files,omitempty"`
	Total     int64       `json:"total"`
	Streams   int         `json:"streams"`
	BlockSize int         `json:"block"`
	Bench     bool        `json:"bench,omitempty"`  // synthetic data, receiver discards it
	NoHash    bool        `json:"nohash,omitempty"` // skip per-block checksums
	Raw       bool        `json:"raw,omitempty"`    // sender asks for unencrypted data streams
}

// Range is a byte range [Off, Off+Len) of one file.
type Range struct {
	Off int64 `json:"o"`
	Len int64 `json:"l"`
}

// FileRanges lists the ranges of a file the receiver already holds.
type FileRanges struct {
	File   uint32  `json:"f"`
	Ranges []Range `json:"r"`
}

type Accept struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
	Raw    bool   `json:"raw,omitempty"` // data streams skip TLS (both sides opted in)
	Dest   string `json:"dest,omitempty"`

	// Resume / dedupe: the sender must not send these.
	Skip []uint32     `json:"skip,omitempty"` // files already complete at the receiver
	Have []FileRanges `json:"have,omitempty"` // partially received files
	// Already is the byte count covered by Skip and Have.
	Already int64 `json:"already,omitempty"`
}

// Progress reports bytes the receiver holds, including Accept.Already.
type Progress struct {
	Bytes int64 `json:"bytes"`
}

type Complete struct {
	Bytes     int64 `json:"bytes"` // transferred in this session
	Already   int64 `json:"already,omitempty"`
	Files     int   `json:"files"`
	ElapsedNS int64 `json:"elapsed_ns"`
}

type ErrorMsg struct {
	Message string `json:"message"`
	Fatal   bool   `json:"fatal,omitempty"` // retrying the same offer cannot succeed
}

func WriteMsg(w io.Writer, t MsgType, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(body) > maxMsg {
		return fmt.Errorf("control message too large (%d bytes)", len(body))
	}
	buf := make([]byte, 5+len(body))
	binary.BigEndian.PutUint32(buf, uint32(len(body)))
	buf[4] = byte(t)
	copy(buf[5:], body)
	_, err = w.Write(buf)
	return err
}

func ReadMsg(r io.Reader) (MsgType, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:4])
	if n > maxMsg {
		return 0, nil, fmt.Errorf("control message too large (%d bytes)", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return MsgType(hdr[4]), body, nil
}

func Decode(body []byte, v any) error { return json.Unmarshal(body, v) }

// ---- data frames ----

// FrameHeaderSize is file(4) + offset(8) + len(4) + flags(1) + checksum(16).
const FrameHeaderSize = 4 + 8 + 4 + 1 + 16

const (
	FlagEnd  uint8 = 1 << 0 // last frame on this stream; no payload
	FlagPack uint8 = 1 << 1 // payload is a pack of whole small files
)

// PackEntryHeader precedes each file inside a pack payload: file u32, size u32.
const PackEntryHeader = 8

// PutPackEntry writes a pack entry header into b.
func PutPackEntry(b []byte, file uint32, size uint32) {
	binary.BigEndian.PutUint32(b[0:], file)
	binary.BigEndian.PutUint32(b[4:], size)
}

// NextPackEntry splits the first file off a pack payload.
func NextPackEntry(p []byte) (file uint32, data, rest []byte, err error) {
	if len(p) < PackEntryHeader {
		return 0, nil, nil, errors.New("truncated pack entry")
	}
	file = binary.BigEndian.Uint32(p[0:])
	size := binary.BigEndian.Uint32(p[4:])
	p = p[PackEntryHeader:]
	if uint64(size) > uint64(len(p)) {
		return 0, nil, nil, errors.New("pack entry overruns payload")
	}
	return file, p[:size], p[size:], nil
}

type FrameHeader struct {
	File   uint32
	Offset uint64
	Len    uint32
	Flags  uint8
	Sum    [16]byte
}

func (h *FrameHeader) Put(b []byte) {
	_ = b[FrameHeaderSize-1]
	binary.BigEndian.PutUint32(b[0:], h.File)
	binary.BigEndian.PutUint64(b[4:], h.Offset)
	binary.BigEndian.PutUint32(b[12:], h.Len)
	b[16] = h.Flags
	copy(b[17:33], h.Sum[:])
}

func ParseFrameHeader(b []byte) FrameHeader {
	_ = b[FrameHeaderSize-1]
	h := FrameHeader{
		File:   binary.BigEndian.Uint32(b[0:]),
		Offset: binary.BigEndian.Uint64(b[4:]),
		Len:    binary.BigEndian.Uint32(b[12:]),
		Flags:  b[16],
	}
	copy(h.Sum[:], b[17:33])
	return h
}

// Checksum is the per-block integrity check: XXH3-128, ~35 GB/s per core on Apple M4.
// It detects corruption; authenticity comes from TLS (added in M2).
func Checksum(p []byte) [16]byte { return xxh3.Hash128(p).Bytes() }

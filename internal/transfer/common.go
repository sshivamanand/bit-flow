// Package transfer is the bit-flow engine: one control connection plus N data
// streams per transfer, each stream a small pipeline joined by bounded channels.
//
//	sender:   [read + checksum] ──chan──▶ [write header+block to TLS/TCP]
//	receiver: [read frame] ──chan──▶ [verify + pwrite at offset | unpack small files]
//
// All streams pull work units from one shared queue, so a faster stream (or,
// later, a faster path) simply takes more of the work. A dropped connection
// fails the attempt; the sender reconnects and the receiver's resume journal
// tells it which bytes are already safe.
package transfer

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/sshivamanand/bit-flow/internal/proto"
)

const (
	DefaultStreams   = 8
	DefaultChunkSize = 32 << 20
	DefaultBlockSize = 4 << 20
	DefaultRetries   = 10

	MinBlockSize = 4 << 10
	MaxBlockSize = 64 << 20
	MaxStreams   = 256

	// pipelineDepth is how many blocks may sit between two stages of one stream.
	pipelineDepth = 4
	// idleTimeout fails a transfer whose streams make no progress (Wi-Fi gone,
	// peer asleep), so the sender can reconnect instead of hanging for minutes.
	idleTimeout = 30 * time.Second
	// handshakeTimeout bounds pairing and TLS handshakes.
	handshakeTimeout = 15 * time.Second
	// smallFileWorkers write files unpacked from packs. File creation, not
	// bandwidth, limits small files, so they get more parallelism than streams.
	smallFileWorkers = 16
)

// Result summarizes a finished transfer.
type Result struct {
	Bytes   int64 // moved over the network
	Already int64 // already at the receiver (identical files, resumed partials)
	Files   int
	Elapsed time.Duration
}

// permanentError marks failures that retrying the same offer cannot fix.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

func permanent(err error) error { return permanentError{err} }

// IsPermanent reports whether err will recur on retry (declined, conflict, wrong code, …).
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// bufPool recycles frame buffers so steady-state transfers allocate nothing.
// Each buffer has FrameHeaderSize bytes of headroom so a frame goes out in one write.
type bufPool struct {
	p sync.Pool
}

func newBufPool(blockSize int) *bufPool {
	bp := &bufPool{}
	bp.p.New = func() any { b := make([]byte, proto.FrameHeaderSize+blockSize); return &b }
	return bp
}

func (bp *bufPool) get() *[]byte  { return bp.p.Get().(*[]byte) }
func (bp *bufPool) put(b *[]byte) { bp.p.Put(b) }

// tune applies socket options to a TCP connection. sockBuf <= 0 keeps OS autotuning.
func tune(c net.Conn, sockBuf int) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	if sockBuf > 0 {
		_ = tc.SetReadBuffer(sockBuf)
		_ = tc.SetWriteBuffer(sockBuf)
	}
}

func dial(ctx context.Context, addr string, sockBuf int) (net.Conn, error) {
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	tune(c, sockBuf)
	return c, nil
}

// closeOnDone closes c when ctx is cancelled, unblocking any pending I/O.
func closeOnDone(ctx context.Context, c net.Conn) (stop func() bool) {
	return context.AfterFunc(ctx, func() { c.Close() })
}

// firstError keeps the first error reported by any goroutine.
type firstError struct {
	mu  sync.Mutex
	err error
}

func (f *firstError) set(err error) {
	f.mu.Lock()
	if f.err == nil {
		f.err = err
	}
	f.mu.Unlock()
}

func (f *firstError) get() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

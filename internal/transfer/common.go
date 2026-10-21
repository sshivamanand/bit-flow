// Package transfer is the bit-flow engine: one control connection plus N data
// streams per transfer, each stream a small pipeline joined by bounded channels.
//
//	sender:   [read + checksum] ──chan──▶ [writev header+block to TCP]
//	receiver: [read frame from TCP] ──chan──▶ [verify + pwrite at offset]
//
// All streams pull work units from one shared queue, so a faster stream (or,
// later, a faster path) simply takes more of the work.
package transfer

import (
	"context"
	"net"
	"sync"
	"time"
)

const (
	DefaultStreams   = 8
	DefaultChunkSize = 32 << 20
	DefaultBlockSize = 4 << 20

	MinBlockSize = 4 << 10
	MaxBlockSize = 64 << 20
	MaxStreams   = 256

	// pipelineDepth is how many blocks may sit between two stages of one stream.
	pipelineDepth = 4
)

// Result summarizes a finished transfer.
type Result struct {
	Bytes   int64
	Files   int
	Elapsed time.Duration
}

// bufPool recycles block buffers so steady-state transfers allocate nothing.
type bufPool struct {
	p sync.Pool
}

func newBufPool(size int) *bufPool {
	bp := &bufPool{}
	bp.p.New = func() any { b := make([]byte, size); return &b }
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

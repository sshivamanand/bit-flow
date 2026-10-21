package transfer

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sshivamanand/bit-flow/internal/planner"
	"github.com/sshivamanand/bit-flow/internal/proto"
)

// Options tune the sender. Zero values mean defaults.
type Options struct {
	Name      string // shown to the receiver
	Streams   int
	ChunkSize int64
	BlockSize int
	NoHash    bool
	SockBuf   int
}

func (o *Options) fill() {
	if o.Name == "" {
		o.Name, _ = os.Hostname()
	}
	if o.Streams <= 0 {
		o.Streams = DefaultStreams
	}
	if o.ChunkSize <= 0 {
		o.ChunkSize = DefaultChunkSize
	}
	if o.BlockSize <= 0 {
		o.BlockSize = DefaultBlockSize
	}
}

// SendStats is a live snapshot of a running send.
type SendStats struct {
	Accepted  bool
	Sent      int64 // written to sockets
	Committed int64 // verified and written by the receiver
	Total     int64
}

type Sender struct {
	opts      Options
	accepted  atomic.Bool
	sent      atomic.Int64
	committed atomic.Int64
	total     atomic.Int64
}

func NewSender(opts Options) *Sender {
	opts.fill()
	return &Sender{opts: opts}
}

func (s *Sender) Stats() SendStats {
	return SendStats{
		Accepted:  s.accepted.Load(),
		Sent:      s.sent.Load(),
		Committed: s.committed.Load(),
		Total:     s.total.Load(),
	}
}

// Send offers the manifest to the receiver at addr and, once accepted, streams it.
func (s *Sender) Send(ctx context.Context, addr string, m *planner.Manifest) (Result, error) {
	if err := s.validate(); err != nil {
		return Result{}, err
	}
	offer := s.offer(m.Total)
	offer.Files = m.Files
	return s.run(ctx, addr, offer, m.Units(s.opts.ChunkSize), fileSource(m.Sources))
}

// Bench streams size bytes of synthetic data that the receiver verifies and discards.
// It measures network + pipeline throughput with no disk involved.
func (s *Sender) Bench(ctx context.Context, addr string, size int64) (Result, error) {
	if err := s.validate(); err != nil {
		return Result{}, err
	}
	offer := s.offer(size)
	offer.Bench = true
	var units []planner.Unit
	for off := int64(0); off < size; off += s.opts.ChunkSize {
		units = append(units, planner.Unit{Offset: off, Len: min(s.opts.ChunkSize, size-off)})
	}
	return s.run(ctx, addr, offer, units, newSynthSource(s.opts.BlockSize))
}

func (s *Sender) validate() error {
	if s.opts.BlockSize < MinBlockSize || s.opts.BlockSize > MaxBlockSize {
		return fmt.Errorf("block size must be between %d KiB and %d MiB", MinBlockSize>>10, MaxBlockSize>>20)
	}
	if s.opts.Streams > MaxStreams {
		return fmt.Errorf("at most %d streams", MaxStreams)
	}
	return nil
}

func (s *Sender) offer(total int64) proto.Offer {
	return proto.Offer{
		From:      s.opts.Name,
		Total:     total,
		Streams:   s.opts.Streams,
		BlockSize: s.opts.BlockSize,
		NoHash:    s.opts.NoHash,
	}
}

func (s *Sender) run(ctx context.Context, addr string, offer proto.Offer, units []planner.Unit, src source) (Result, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var sid proto.SessionID
	if _, err := rand.Read(sid[:]); err != nil {
		return Result{}, err
	}

	ctrl, err := dial(ctx, addr, 0)
	if err != nil {
		return Result{}, fmt.Errorf("connect to receiver: %w", err)
	}
	defer ctrl.Close()
	defer closeOnDone(ctx, ctrl)()

	if err := proto.WriteHello(ctrl, proto.Hello{Kind: proto.KindControl, Session: sid}); err != nil {
		return Result{}, err
	}
	if err := proto.WriteMsg(ctrl, proto.MsgOffer, offer); err != nil {
		return Result{}, err
	}

	// Wait for the receiver to accept (a human may be deciding).
	typ, body, err := proto.ReadMsg(ctrl)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, context.Cause(ctx)
		}
		return Result{}, fmt.Errorf("waiting for receiver: %w", err)
	}
	switch typ {
	case proto.MsgAccept:
		var a proto.Accept
		if err := proto.Decode(body, &a); err != nil {
			return Result{}, err
		}
		if !a.OK {
			return Result{}, fmt.Errorf("receiver declined: %s", a.Reason)
		}
	case proto.MsgError:
		return Result{}, remoteError(body)
	default:
		return Result{}, fmt.Errorf("unexpected message %d from receiver", typ)
	}
	s.total.Store(offer.Total)
	s.accepted.Store(true)
	start := time.Now()

	// Open the data streams in parallel.
	conns := make([]net.Conn, offer.Streams)
	var dialErr error
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := dial(ctx, addr, s.opts.SockBuf)
			if err == nil {
				err = proto.WriteHello(c, proto.Hello{Kind: proto.KindData, Session: sid, Stream: uint16(i)})
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if c != nil {
					c.Close()
				}
				if dialErr == nil {
					dialErr = err
				}
				return
			}
			conns[i] = c
		}()
	}
	wg.Wait()
	for _, c := range conns {
		if c != nil {
			defer c.Close()
			defer closeOnDone(ctx, c)()
		}
	}
	if dialErr != nil {
		return Result{}, fmt.Errorf("open data streams: %w", dialErr)
	}

	// Control reader: progress, completion, remote errors.
	complete := make(chan proto.Complete, 1)
	go func() {
		for {
			typ, body, err := proto.ReadMsg(ctrl)
			if err != nil {
				cancel(fmt.Errorf("lost connection to receiver: %w", err))
				return
			}
			switch typ {
			case proto.MsgProgress:
				var p proto.Progress
				if proto.Decode(body, &p) == nil {
					s.committed.Store(p.Bytes)
				}
			case proto.MsgComplete:
				var c proto.Complete
				_ = proto.Decode(body, &c)
				s.committed.Store(c.Bytes)
				complete <- c
				return
			case proto.MsgError:
				cancel(remoteError(body))
				return
			}
		}
	}()

	// Shared work queue: every stream pulls the next unit when it's free.
	queue := make(chan planner.Unit, len(units))
	for _, u := range units {
		queue <- u
	}
	close(queue)

	pool := newBufPool(offer.BlockSize)
	for _, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.stream(ctx, c, queue, src, pool, offer); err != nil {
				cancel(err)
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return Result{}, context.Cause(ctx)
	}

	select {
	case c := <-complete:
		return Result{Bytes: c.Bytes, Files: c.Files, Elapsed: time.Since(start)}, nil
	case <-ctx.Done():
		return Result{}, context.Cause(ctx)
	}
}

type outBlock struct {
	hdr  [proto.FrameHeaderSize]byte
	data []byte
	buf  *[]byte // returned to the pool after sending; nil for synthetic data
}

// stream runs one data stream: a producer goroutine reads and checksums blocks
// while this goroutine writes them to the socket.
func (s *Sender) stream(ctx context.Context, conn net.Conn, queue <-chan planner.Unit, src source, pool *bufPool, offer proto.Offer) error {
	ch := make(chan outBlock, pipelineDepth)
	errc := make(chan error, 1)
	go func() {
		defer close(ch)
		errc <- produce(ctx, queue, src, pool, offer, ch)
	}()

	for b := range ch {
		bufs := net.Buffers{b.hdr[:], b.data} // header + block in one writev syscall
		_, err := bufs.WriteTo(conn)
		if b.buf != nil {
			pool.put(b.buf)
		}
		if err != nil {
			return err // run cancels the session, which stops the producer
		}
		s.sent.Add(int64(len(b.data)))
	}
	if err := <-errc; err != nil {
		return err
	}
	var end [proto.FrameHeaderSize]byte
	(&proto.FrameHeader{Flags: proto.FlagEnd}).Put(end[:])
	_, err := conn.Write(end[:])
	return err
}

func produce(ctx context.Context, queue <-chan planner.Unit, src source, pool *bufPool, offer proto.Offer, ch chan<- outBlock) error {
	for u := range queue {
		if ctx.Err() != nil {
			return nil
		}
		r, err := src.open(u.File)
		if err != nil {
			return err
		}
		end := u.Offset + u.Len
		for off := u.Offset; off < end; {
			n := int(min(int64(offer.BlockSize), end-off))
			data, buf, err := r.readBlock(pool, off, n)
			if err != nil {
				r.Close()
				return err
			}
			b := outBlock{data: data, buf: buf}
			h := proto.FrameHeader{File: u.File, Offset: uint64(off), Len: uint32(n)}
			if !offer.NoHash {
				h.Sum = proto.Checksum(data)
			}
			h.Put(b.hdr[:])
			select {
			case ch <- b:
			case <-ctx.Done():
				if buf != nil {
					pool.put(buf)
				}
				r.Close()
				return nil
			}
			off += int64(n)
		}
		r.Close()
	}
	return nil
}

// ---- sources ----

type source interface {
	open(file uint32) (blockReader, error)
}

type blockReader interface {
	readBlock(pool *bufPool, off int64, n int) (data []byte, buf *[]byte, err error)
	Close() error
}

// fileSource reads blocks from the sender's files.
type fileSource []string

func (fs fileSource) open(file uint32) (blockReader, error) {
	f, err := os.Open(fs[file])
	if err != nil {
		return nil, err
	}
	return fileReader{f}, nil
}

type fileReader struct{ f *os.File }

func (r fileReader) readBlock(pool *bufPool, off int64, n int) ([]byte, *[]byte, error) {
	buf := pool.get()
	data := (*buf)[:n]
	got, err := r.f.ReadAt(data, off)
	if got < n {
		pool.put(buf)
		if err == nil || errors.Is(err, io.EOF) {
			err = fmt.Errorf("%s shrank while being sent", r.f.Name())
		}
		return nil, nil, err
	}
	return data, buf, nil
}

func (r fileReader) Close() error { return r.f.Close() }

// synthSource serves the same random block forever (bench mode, no disk).
type synthSource struct{ block []byte }

func newSynthSource(blockSize int) synthSource {
	b := make([]byte, blockSize)
	_, _ = rand.Read(b)
	return synthSource{block: b}
}

func (s synthSource) open(uint32) (blockReader, error) { return s, nil }

func (s synthSource) readBlock(_ *bufPool, _ int64, n int) ([]byte, *[]byte, error) {
	return s.block[:n], nil, nil
}

func (s synthSource) Close() error { return nil }

func remoteError(body []byte) error {
	var e proto.ErrorMsg
	_ = proto.Decode(body, &e)
	return fmt.Errorf("receiver: %s", e.Message)
}

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
	"github.com/sshivamanand/bit-flow/internal/secure"
)

// Options tune the sender. Zero values mean defaults.
type Options struct {
	Name      string // shown to the receiver
	Code      string // pairing code shown by the receiver
	Streams   int
	ChunkSize int64
	BlockSize int
	NoHash    bool
	Raw       bool // ask for unencrypted data streams (receiver must allow it)
	SockBuf   int
	Retries   int // reconnect attempts after a dropped connection; 0 = default, <0 = none
	OnRetry   func(attempt int, err error, wait time.Duration)
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
	switch {
	case o.Retries == 0:
		o.Retries = DefaultRetries
	case o.Retries < 0:
		o.Retries = 0
	}
}

// SendStats is a live snapshot of a running send.
type SendStats struct {
	Accepted  bool
	Sent      int64 // written to sockets in the current attempt
	Committed int64 // held by the receiver (including what it already had)
	Total     int64
}

type Sender struct {
	opts Options

	idOnce sync.Once
	id     *secure.Identity
	idErr  error

	accepted  atomic.Bool
	sent      atomic.Int64
	committed atomic.Int64
	total     atomic.Int64

	firstAccept  atomic.Int64 // unix nanos of the first accepted attempt
	firstAlready atomic.Int64
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

func (s *Sender) identity() (*secure.Identity, error) {
	s.idOnce.Do(func() { s.id, s.idErr = secure.NewIdentity() })
	return s.id, s.idErr
}

// Send offers the manifest to the receiver at addr and, once accepted, streams
// it. Dropped connections are retried; the receiver resumes where it stopped.
func (s *Sender) Send(ctx context.Context, addr string, m *planner.Manifest) (Result, error) {
	if err := s.validate(); err != nil {
		return Result{}, err
	}
	offer := s.offer(m.Total)
	offer.Files = m.Files
	plan := func(a *proto.Accept) []planner.Unit {
		skip := make(map[uint32]bool, len(a.Skip))
		for _, f := range a.Skip {
			skip[f] = true
		}
		have := make(map[uint32][]proto.Range, len(a.Have))
		for _, h := range a.Have {
			have[h.File] = h.Ranges
		}
		return m.Units(s.opts.ChunkSize, s.opts.BlockSize, skip, have)
	}
	src := fileSource{paths: m.Sources, files: m.Files}

	for attempt := 1; ; attempt++ {
		c, err := s.attempt(ctx, addr, offer, plan, src)
		if err == nil {
			return s.result(c), nil
		}
		// Retry only transfers that got going: before the first accept, an error
		// means a wrong address or a receiver that isn't running, so fail fast.
		if IsPermanent(err) || ctx.Err() != nil || attempt > s.opts.Retries || s.firstAccept.Load() == 0 {
			return Result{}, err
		}
		wait := min(time.Duration(1<<(attempt-1))*time.Second, 10*time.Second)
		if s.opts.OnRetry != nil {
			s.opts.OnRetry(attempt, err, wait)
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return Result{}, err
		}
	}
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
	plan := func(*proto.Accept) []planner.Unit { return units }
	c, err := s.attempt(ctx, addr, offer, plan, newSynthSource(s.opts.BlockSize))
	if err != nil {
		return Result{}, err
	}
	return s.result(c), nil
}

func (s *Sender) result(c proto.Complete) Result {
	already := s.firstAlready.Load()
	return Result{
		Bytes:   s.total.Load() - already,
		Already: already,
		Files:   c.Files,
		Elapsed: time.Since(time.Unix(0, s.firstAccept.Load())),
	}
}

func (s *Sender) validate() error {
	if s.opts.BlockSize < MinBlockSize || s.opts.BlockSize > MaxBlockSize {
		return fmt.Errorf("block size must be between %d KiB and %d MiB", MinBlockSize>>10, MaxBlockSize>>20)
	}
	if s.opts.Streams > MaxStreams {
		return fmt.Errorf("at most %d streams", MaxStreams)
	}
	if s.opts.Code == "" {
		return errors.New("a pairing code is required (shown by the receiver)")
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
		Raw:       s.opts.Raw,
	}
}

// attempt runs one connection's worth of a transfer: pair, offer, stream.
func (s *Sender) attempt(ctx context.Context, addr string, offer proto.Offer, plan func(*proto.Accept) []planner.Unit, src source) (proto.Complete, error) {
	id, err := s.identity()
	if err != nil {
		return proto.Complete{}, permanent(err)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	s.accepted.Store(false)
	s.sent.Store(0)

	var sid proto.SessionID
	if _, err := rand.Read(sid[:]); err != nil {
		return proto.Complete{}, err
	}

	conn, err := dial(ctx, addr, 0)
	if err != nil {
		return proto.Complete{}, fmt.Errorf("connect to receiver: %w", err)
	}
	defer conn.Close()
	defer closeOnDone(ctx, conn)()

	// Pair over the plain connection, then switch it to pinned TLS.
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := proto.WriteHello(conn, proto.Hello{Kind: proto.KindControl, Session: sid}); err != nil {
		return proto.Complete{}, err
	}
	paired, err := secure.PairDialer(conn, s.opts.Code, sid, id)
	if errors.Is(err, secure.ErrWrongCode) {
		return proto.Complete{}, permanent(fmt.Errorf("%w: check the code shown on the receiver", err))
	}
	if err != nil {
		return proto.Complete{}, fmt.Errorf("pairing: %w", err)
	}
	ctrl := id.Client(conn, paired.PeerFP)
	if err := ctrl.HandshakeContext(ctx); err != nil {
		return proto.Complete{}, fmt.Errorf("secure channel: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})

	if err := proto.WriteMsg(ctrl, proto.MsgOffer, offer); err != nil {
		return proto.Complete{}, err
	}
	// Wait for the receiver to accept (a human may be deciding).
	typ, body, err := proto.ReadMsg(ctrl)
	if err != nil {
		if ctx.Err() != nil {
			return proto.Complete{}, context.Cause(ctx)
		}
		return proto.Complete{}, fmt.Errorf("waiting for receiver: %w", err)
	}
	var accept proto.Accept
	switch typ {
	case proto.MsgAccept:
		if err := proto.Decode(body, &accept); err != nil {
			return proto.Complete{}, err
		}
		if !accept.OK {
			return proto.Complete{}, permanent(fmt.Errorf("receiver declined: %s", accept.Reason))
		}
	case proto.MsgError:
		return proto.Complete{}, remoteError(body)
	default:
		return proto.Complete{}, fmt.Errorf("unexpected message %d from receiver", typ)
	}
	units := plan(&accept)
	s.total.Store(offer.Total)
	s.committed.Store(accept.Already)
	if s.firstAccept.CompareAndSwap(0, time.Now().UnixNano()) {
		s.firstAlready.Store(accept.Already)
	}
	s.accepted.Store(true)

	conns, err := s.openStreams(ctx, addr, sid, offer.Streams, accept.Raw, paired, id)
	for _, c := range conns {
		if c != nil {
			defer c.Close()
			defer closeOnDone(ctx, c)()
		}
	}
	if err != nil {
		return proto.Complete{}, fmt.Errorf("open data streams: %w", err)
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
				s.committed.Store(c.Bytes + c.Already)
				complete <- c
				return
			case proto.MsgError:
				cancel(remoteError(body))
				return
			}
		}
	}()
	go s.watchdog(ctx, cancel, offer.Total)

	// Shared work queue: every stream pulls the next unit when it's free.
	queue := make(chan planner.Unit, len(units))
	for _, u := range units {
		queue <- u
	}
	close(queue)

	pool := newBufPool(offer.BlockSize)
	var wg sync.WaitGroup
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
		return proto.Complete{}, context.Cause(ctx)
	}

	select {
	case c := <-complete:
		return c, nil
	case <-ctx.Done():
		return proto.Complete{}, context.Cause(ctx)
	}
}

// openStreams dials n data connections in parallel and authenticates each one.
func (s *Sender) openStreams(ctx context.Context, addr string, sid proto.SessionID, n int, raw bool, paired secure.Paired, id *secure.Identity) ([]net.Conn, error) {
	conns := make([]net.Conn, n)
	var errs firstError
	var wg sync.WaitGroup
	for i := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := dial(ctx, addr, s.opts.SockBuf)
			if err != nil {
				errs.set(err)
				return
			}
			conns[i] = c
			_ = c.SetDeadline(time.Now().Add(handshakeTimeout))
			if err := proto.WriteHello(c, proto.Hello{Kind: proto.KindData, Session: sid, Stream: uint16(i)}); err != nil {
				errs.set(err)
				return
			}
			if raw {
				tok := secure.StreamToken(paired.Key, sid, uint16(i))
				if _, err := c.Write(tok[:]); err != nil {
					errs.set(err)
					return
				}
			} else {
				tc := id.Client(c, paired.PeerFP)
				if err := tc.HandshakeContext(ctx); err != nil {
					errs.set(err)
					return
				}
				conns[i] = tc
			}
			_ = conns[i].SetDeadline(time.Time{})
		}()
	}
	wg.Wait()
	return conns, errs.get()
}

// watchdog cancels the attempt if the receiver stops making progress.
func (s *Sender) watchdog(ctx context.Context, cancel context.CancelCauseFunc, total int64) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	last, since := s.committed.Load(), time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if c := s.committed.Load(); c != last || c >= total {
			last, since = c, time.Now()
		} else if time.Since(since) > idleTimeout {
			cancel(fmt.Errorf("transfer stalled: no progress for %s", idleTimeout))
			return
		}
	}
}

type outBlock struct {
	frame []byte // header + payload
	n     int64  // file bytes carried
	buf   *[]byte
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
		_ = conn.SetWriteDeadline(time.Now().Add(idleTimeout))
		_, err := conn.Write(b.frame)
		pool.put(b.buf)
		if err != nil {
			return err // the attempt is cancelled, which stops the producer
		}
		s.sent.Add(b.n)
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
	const H = proto.FrameHeaderSize
	emit := func(b outBlock) bool {
		select {
		case ch <- b:
			return true
		case <-ctx.Done():
			pool.put(b.buf)
			return false
		}
	}
	for u := range queue {
		if ctx.Err() != nil {
			return nil
		}
		if u.Pack != nil {
			b, err := readPack(u, src, pool, offer.NoHash)
			if err != nil {
				return err
			}
			if !emit(b) {
				return nil
			}
			continue
		}
		r, err := src.open(u.File)
		if err != nil {
			return err
		}
		end := u.Offset + u.Len
		for off := u.Offset; off < end; {
			n := int(min(int64(offer.BlockSize), end-off))
			buf := pool.get()
			frame := (*buf)[:H+n]
			if err := readFull(r, frame[H:], off); err != nil {
				pool.put(buf)
				r.Close()
				return err
			}
			h := proto.FrameHeader{File: u.File, Offset: uint64(off), Len: uint32(n)}
			if !offer.NoHash {
				h.Sum = proto.Checksum(frame[H:])
			}
			h.Put(frame)
			if !emit(outBlock{frame: frame, n: int64(n), buf: buf}) {
				r.Close()
				return nil
			}
			off += int64(n)
		}
		r.Close()
	}
	return nil
}

// readPack reads whole small files into one PACK frame.
func readPack(u planner.Unit, src source, pool *bufPool, noHash bool) (outBlock, error) {
	const H = proto.FrameHeaderSize
	buf := pool.get()
	b := *buf
	p := H
	for _, f := range u.Pack {
		size := int(src.size(f))
		r, err := src.open(f)
		if err != nil {
			pool.put(buf)
			return outBlock{}, err
		}
		proto.PutPackEntry(b[p:], f, uint32(size))
		err = readFull(r, b[p+proto.PackEntryHeader:p+proto.PackEntryHeader+size], 0)
		r.Close()
		if err != nil {
			pool.put(buf)
			return outBlock{}, err
		}
		p += proto.PackEntryHeader + size
	}
	h := proto.FrameHeader{Flags: proto.FlagPack, Len: uint32(p - H)}
	if !noHash {
		h.Sum = proto.Checksum(b[H:p])
	}
	h.Put(b)
	return outBlock{frame: b[:p], n: u.Len, buf: buf}, nil
}

// ---- sources ----

type source interface {
	open(file uint32) (fileReader, error)
	size(file uint32) int64
}

type fileReader interface {
	io.ReaderAt
	io.Closer
	Name() string
}

func readFull(r fileReader, p []byte, off int64) error {
	n, err := r.ReadAt(p, off)
	if n < len(p) {
		if err == nil || errors.Is(err, io.EOF) {
			err = fmt.Errorf("%s shrank while being sent", r.Name())
		}
		return err
	}
	return nil
}

// fileSource reads the sender's files.
type fileSource struct {
	paths []string
	files []proto.FileEntry
}

func (fs fileSource) open(file uint32) (fileReader, error) { return os.Open(fs.paths[file]) }
func (fs fileSource) size(file uint32) int64               { return fs.files[file].Size }

// synthSource serves the same random block forever (bench mode, no disk).
type synthSource struct{ block []byte }

func newSynthSource(blockSize int) synthSource {
	b := make([]byte, blockSize)
	_, _ = rand.Read(b)
	return synthSource{block: b}
}

func (s synthSource) open(uint32) (fileReader, error) { return s, nil }
func (s synthSource) size(uint32) int64               { return 0 }
func (s synthSource) ReadAt(p []byte, _ int64) (int, error) {
	return copy(p, s.block[:len(p)]), nil
}
func (s synthSource) Close() error { return nil }
func (s synthSource) Name() string { return "synthetic data" }

func remoteError(body []byte) error {
	var e proto.ErrorMsg
	_ = proto.Decode(body, &e)
	err := fmt.Errorf("receiver: %s", e.Message)
	if e.Fatal {
		return permanent(err)
	}
	return err
}

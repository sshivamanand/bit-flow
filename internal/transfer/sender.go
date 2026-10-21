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
	Streams   int    // 0: auto-tune up to AutoStreamsMax; >0: exactly this many
	ChunkSize int64
	BlockSize int
	NoHash    bool
	Raw       bool   // ask for unencrypted data streams (receiver must allow it)
	Compress  string // CompressAuto (default), CompressOn or CompressOff
	SockBuf   int
	Retries   int // reconnect attempts after a dropped connection; 0 = default, <0 = none
	OnRetry   func(attempt int, err error, wait time.Duration)
	// OnCodeChange reports a new pairing code after repeated wrong guesses (Serve only).
	OnCodeChange func(code string)
}

func (o *Options) fill() {
	if o.Name == "" {
		o.Name, _ = os.Hostname()
	}
	if o.ChunkSize <= 0 {
		o.ChunkSize = DefaultChunkSize
	}
	if o.BlockSize <= 0 {
		o.BlockSize = DefaultBlockSize
	}
	if o.Compress == "" {
		o.Compress = CompressAuto
	}
	switch {
	case o.Retries == 0:
		o.Retries = DefaultRetries
	case o.Retries < 0:
		o.Retries = 0
	}
}

// streamPlan is how many connections to open and how many to start with.
func (o *Options) streamPlan() (open, start int, tune bool) {
	if o.Streams > 0 {
		return o.Streams, o.Streams, false
	}
	return AutoStreamsMax, autoStreamsStart, true
}

// SendStats is a live snapshot of a running send.
type SendStats struct {
	Accepted  bool
	Sent      int64 // file bytes written to sockets in the current attempt
	Committed int64 // held by the receiver (including what it already had)
	Total     int64
	Streams   int        // streams currently allowed to carry data
	Saved     int64      // bytes compression kept off the wire
	Compress  string     // "on", "off", "paused (…)"
	Route     Route      // how the bytes travel
	Paths     []PathStat // per link, when more than one is used
}

// Route describes the link a transfer uses, for display and advice.
type Route struct {
	LocalLabel string // our interface, e.g. "Wi-Fi"
	PeerLabel  string // the receiver's interface we reached
	LocalIP    string
	PeerIP     string
}

// PathStat is the traffic carried by one link.
type PathStat struct {
	Label string
	Bytes int64
}

type Sender struct {
	opts Options

	idOnce sync.Once
	id     *secure.Identity
	idErr  error

	hubOnce sync.Once
	hub     *hub
	hubErr  error

	accepted  atomic.Bool
	sent      atomic.Int64
	committed atomic.Int64
	total     atomic.Int64

	firstAccept  atomic.Int64 // unix nanos of the first accepted attempt
	firstAlready atomic.Int64

	mu    sync.Mutex // guards the fields below
	gate  *gate
	comp  *compressor
	route Route
	paths []string        // label per data stream
	bytes []*atomic.Int64 // file bytes per data stream
}

func NewSender(opts Options) *Sender {
	opts.fill()
	return &Sender{opts: opts}
}

func (s *Sender) Stats() SendStats {
	st := SendStats{
		Accepted:  s.accepted.Load(),
		Sent:      s.sent.Load(),
		Committed: s.committed.Load(),
		Total:     s.total.Load(),
		Compress:  CompressOff,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gate != nil {
		st.Streams = s.gate.current()
	}
	if c := s.comp; c != nil && c.enc != nil {
		st.Saved = c.saved.Load()
		switch {
		case c.enabled.Load():
			st.Compress = CompressOn
		case c.cpuBound.Load():
			st.Compress = "off (link faster than compression)"
		default:
			st.Compress = "paused (data doesn't compress)"
		}
	}
	st.Route = s.route
	byLabel := map[string]int64{}
	var order []string
	for i, label := range s.paths {
		if _, ok := byLabel[label]; !ok {
			order = append(order, label)
		}
		byLabel[label] += s.bytes[i].Load()
	}
	if len(order) > 1 {
		for _, l := range order {
			st.Paths = append(st.Paths, PathStat{Label: l, Bytes: byLabel[l]})
		}
	}
	return st
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
	offer, plan, src := s.prepare(m)
	for attempt := 1; ; attempt++ {
		c, err := s.dialAttempt(ctx, addr, offer, plan, src)
		if err == nil {
			return s.result(c), nil
		}
		// Retry only transfers that got going: before the first accept, an error
		// means a wrong address or a receiver that isn't running, so fail fast.
		if IsPermanent(err) || ctx.Err() != nil || attempt > s.opts.Retries || s.firstAccept.Load() == 0 {
			return Result{}, err
		}
		wait := backoff(attempt)
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

func (s *Sender) ensureHub() (*hub, error) {
	s.hubOnce.Do(func() {
		s.hub, s.hubErr = newHub(s.opts.Code, s.opts.SockBuf)
		if s.hubErr == nil {
			s.hub.onCodeChange = s.opts.OnCodeChange
		}
	})
	return s.hub, s.hubErr
}

// CheckCode verifies a pairing code typed into a browser (Serve mode).
func (s *Sender) CheckCode(code string) bool {
	h, err := s.ensureHub()
	return err == nil && h.checkCode(code)
}

// Serve waits on ln for a receiver to fetch the manifest (reverse mode, for
// when the receiver can't accept incoming connections). Interrupted transfers
// resume when the receiver reconnects. It returns after one complete transfer.
func (s *Sender) Serve(ctx context.Context, ln net.Listener, m *planner.Manifest) (Result, error) {
	if err := s.validate(); err != nil {
		return Result{}, err
	}
	h, err := s.ensureHub()
	if err != nil {
		return Result{}, err
	}
	offer, plan, src := s.prepare(m)

	type outcome struct {
		c   proto.Complete
		err error
	}
	results := make(chan outcome, 1)
	var busy sync.Mutex
	h.onControl = func(ctx context.Context, ctrl net.Conn, lk *hubLink) error {
		defer ctrl.Close()
		if !busy.TryLock() {
			_ = proto.WriteMsg(ctrl, proto.MsgError, proto.ErrorMsg{Message: "sender is busy with another receiver"})
			return errBusy
		}
		defer busy.Unlock()
		c, err := s.session(ctx, ctrl, lk, offer, plan, src)
		if err == nil || IsPermanent(err) {
			select {
			case results <- outcome{c, err}:
			default:
			}
		}
		return err
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go h.serve(sctx, ln, false)
	select {
	case o := <-results:
		if o.err != nil {
			return Result{}, o.err
		}
		return s.result(o.c), nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

func backoff(attempt int) time.Duration {
	return min(time.Duration(1<<(attempt-1))*time.Second, 10*time.Second)
}

// prepare builds the offer and the planning function for a manifest.
func (s *Sender) prepare(m *planner.Manifest) (proto.Offer, func(*proto.Accept) []planner.Unit, source) {
	offer := s.offer(m.Total)
	offer.Files = m.Files
	offer.Steam = m.Steam
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
	return offer, plan, fileSource{paths: m.Sources, files: m.Files}
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
	c, err := s.dialAttempt(ctx, addr, offer, plan, newSynthSource(s.opts.BlockSize))
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
	switch s.opts.Compress {
	case CompressAuto, CompressOn, CompressOff:
	default:
		return fmt.Errorf("compression must be auto, on or off")
	}
	if s.opts.Code == "" {
		return errors.New("a pairing code is required (shown by the receiver)")
	}
	return nil
}

func (s *Sender) offer(total int64) proto.Offer {
	open, _, _ := s.opts.streamPlan()
	return proto.Offer{
		From:      s.opts.Name,
		Total:     total,
		Streams:   open,
		BlockSize: s.opts.BlockSize,
		NoHash:    s.opts.NoHash,
		Raw:       s.opts.Raw,
		Addrs:     localAddrs(),
	}
}

// dialAttempt connects to a listening receiver and runs one session.
func (s *Sender) dialAttempt(ctx context.Context, addr string, offer proto.Offer, plan func(*proto.Accept) []planner.Unit, src source) (proto.Complete, error) {
	id, err := s.identity()
	if err != nil {
		return proto.Complete{}, permanent(err)
	}
	ctrl, lk, err := dialControl(ctx, addr, s.opts.Code, id, s.opts.SockBuf)
	if err != nil {
		return proto.Complete{}, err
	}
	defer ctrl.Close()
	return s.session(ctx, ctrl, lk, offer, plan, src)
}

// session runs the sender's side of one transfer on a secured control connection.
func (s *Sender) session(ctx context.Context, ctrl net.Conn, lk link, offer proto.Offer, plan func(*proto.Accept) []planner.Unit, src source) (proto.Complete, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	defer closeOnDone(ctx, ctrl)()
	s.accepted.Store(false)
	s.sent.Store(0)

	lk.expect(offer.Streams, offer.Raw)
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

	lk.usePeerAddrs(accept.Addrs)
	conns, labels, err := lk.streams(ctx, offer.Streams, accept.Raw)
	if err != nil {
		return proto.Complete{}, err
	}
	for _, c := range conns {
		defer c.Close()
		defer closeOnDone(ctx, c)()
	}

	_, start, tune := s.opts.streamPlan()
	g := newGate(min(start, len(conns)))
	mode := s.opts.Compress
	if offer.Bench && mode == CompressAuto {
		mode = CompressOff // synthetic data is random; measure the link, not zstd
	}
	comp := newCompressor(mode, src.names())
	bytes := make([]*atomic.Int64, len(conns))
	for i := range bytes {
		bytes[i] = new(atomic.Int64)
	}
	s.mu.Lock()
	s.gate, s.comp, s.paths, s.bytes = g, comp, labels, bytes
	s.route = routeFor(ctrl, accept.Addrs)
	s.mu.Unlock()
	s.accepted.Store(true)

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
	go s.tune(ctx, g, comp, tune, len(conns))

	// Shared work queue: every stream pulls the next unit when it's free.
	queue := make(chan planner.Unit, len(units))
	for _, u := range units {
		queue <- u
	}
	close(queue)

	pool := newBufPool(offer.BlockSize)
	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := producer{idx: i, gate: g, queue: queue, src: src, pool: pool, offer: offer, comp: comp}
			if err := s.stream(ctx, c, p, bytes[i]); err != nil {
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

// routeFor describes the control connection's link using both sides' interface lists.
func routeFor(ctrl net.Conn, peer []proto.NetAddr) Route {
	var r Route
	if a, ok := ctrl.LocalAddr().(*net.TCPAddr); ok {
		r.LocalIP = a.IP.String()
	}
	if a, ok := ctrl.RemoteAddr().(*net.TCPAddr); ok {
		r.PeerIP = a.IP.String()
	}
	for _, a := range localAddrs() {
		if a.IP == r.LocalIP {
			r.LocalLabel = a.Label
		}
	}
	for _, a := range peer {
		if a.IP == r.PeerIP {
			r.PeerLabel = a.Label
		}
	}
	return r
}

// tune grows the active stream count while it helps, and lets the compressor
// back off when it stops paying.
func (s *Sender) tune(ctx context.Context, g *gate, comp *compressor, tuneStreams bool, max int) {
	t := newTuner(g, max)
	if !tuneStreams {
		t.frozen = true
	}
	tick := time.NewTicker(tuneEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			rate := t.observe(s.committed.Load(), now)
			comp.adjust(rate, g.current())
		}
	}
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

// stream runs one data stream: a producer goroutine reads, checksums and
// compresses blocks while this goroutine writes them to the socket.
func (s *Sender) stream(ctx context.Context, conn net.Conn, p producer, carried *atomic.Int64) error {
	ch := make(chan outBlock, pipelineDepth)
	errc := make(chan error, 1)
	go func() {
		defer close(ch)
		errc <- p.run(ctx, ch)
	}()

	for b := range ch {
		_ = conn.SetWriteDeadline(time.Now().Add(idleTimeout))
		_, err := conn.Write(b.frame)
		p.pool.put(b.buf)
		if err != nil {
			return err // the attempt is cancelled, which stops the producer
		}
		s.sent.Add(b.n)
		carried.Add(b.n)
	}
	if err := <-errc; err != nil {
		return err
	}
	var end [proto.FrameHeaderSize]byte
	(&proto.FrameHeader{Flags: proto.FlagEnd}).Put(end[:])
	_, err := conn.Write(end[:])
	return err
}

// producer turns work units into frames for one stream.
type producer struct {
	idx   int
	gate  *gate
	queue <-chan planner.Unit
	src   source
	pool  *bufPool
	offer proto.Offer
	comp  *compressor
}

func (p producer) run(ctx context.Context, ch chan<- outBlock) error {
	const H = proto.FrameHeaderSize
	emit := func(b outBlock) bool {
		select {
		case ch <- b:
			return true
		case <-ctx.Done():
			p.pool.put(b.buf)
			return false
		}
	}
	for {
		if !p.gate.wait(ctx, p.idx) {
			return nil
		}
		u, ok := <-p.queue
		if !ok {
			p.gate.release() // nothing left: let held-back streams finish too
			return nil
		}
		if u.Pack != nil {
			b, err := p.readPack(u)
			if err != nil {
				return err
			}
			if !emit(b) {
				return nil
			}
			continue
		}
		r, err := p.src.open(u.File)
		if err != nil {
			return err
		}
		end := u.Offset + u.Len
		for off := u.Offset; off < end; {
			n := int(min(int64(p.offer.BlockSize), end-off))
			buf := p.pool.get()
			if err := readFull(r, (*buf)[H:H+n], off); err != nil {
				p.pool.put(buf)
				r.Close()
				return err
			}
			h := proto.FrameHeader{File: u.File, Offset: uint64(off), Len: uint32(n), RawLen: uint32(n)}
			if !emit(p.frame(h, buf, n, int(u.File))) {
				r.Close()
				return nil
			}
			off += int64(n)
		}
		r.Close()
	}
}

// frame checksums the raw payload in buf[H:H+n], compresses it if that pays,
// and returns the finished frame.
func (p producer) frame(h proto.FrameHeader, buf *[]byte, n, file int) outBlock {
	const H = proto.FrameHeaderSize
	raw := (*buf)[H : H+n]
	if !p.offer.NoHash {
		h.Sum = proto.Checksum(raw)
	}
	if p.comp.wants(file) {
		cbuf := p.pool.get()
		// Pool buffers leave room for zstd's worst case, so out stays inside cbuf.
		if out, ok := p.comp.compress(raw, (*cbuf)[H:H], file); ok && len(out) > 0 && &out[0] == &(*cbuf)[H] {
			p.pool.put(buf)
			h.Len = uint32(len(out))
			h.Flags |= proto.FlagCompressed
			frame := (*cbuf)[:H+len(out)]
			h.Put(frame)
			return outBlock{frame: frame, n: int64(n), buf: cbuf}
		}
		p.pool.put(cbuf)
	}
	frame := (*buf)[:H+n]
	h.Put(frame)
	return outBlock{frame: frame, n: int64(n), buf: buf}
}

// readPack reads whole small files into one PACK frame.
func (p producer) readPack(u planner.Unit) (outBlock, error) {
	const H = proto.FrameHeaderSize
	buf := p.pool.get()
	b := *buf
	at := H
	for _, f := range u.Pack {
		size := int(p.src.size(f))
		r, err := p.src.open(f)
		if err != nil {
			p.pool.put(buf)
			return outBlock{}, err
		}
		proto.PutPackEntry(b[at:], f, uint32(size))
		err = readFull(r, b[at+proto.PackEntryHeader:at+proto.PackEntryHeader+size], 0)
		r.Close()
		if err != nil {
			p.pool.put(buf)
			return outBlock{}, err
		}
		at += proto.PackEntryHeader + size
	}
	n := at - H
	ob := p.frame(proto.FrameHeader{Flags: proto.FlagPack, Len: uint32(n), RawLen: uint32(n)}, buf, n, -1)
	ob.n = u.Len // file bytes, not including entry headers
	return ob, nil
}

// ---- sources ----

type source interface {
	open(file uint32) (fileReader, error)
	size(file uint32) int64
	names() []string // per file, for compression heuristics
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
func (fs fileSource) names() []string                      { return fs.paths }

// synthSource serves the same random block forever (bench mode, no disk).
type synthSource struct{ block []byte }

func newSynthSource(blockSize int) synthSource {
	b := make([]byte, blockSize)
	_, _ = rand.Read(b)
	return synthSource{block: b}
}

func (s synthSource) open(uint32) (fileReader, error) { return s, nil }
func (s synthSource) size(uint32) int64               { return 0 }
func (s synthSource) names() []string                 { return nil }
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

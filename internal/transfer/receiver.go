package transfer

import (
	"bufio"
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sshivamanand/bit-flow/internal/proto"
	"github.com/sshivamanand/bit-flow/internal/secure"
	"github.com/sshivamanand/bit-flow/internal/store"
)

const (
	// maxPairFailures wrong codes in a row rotate the code; each failure also
	// costs pairPenalty, so guessing a 6-digit code online is hopeless.
	maxPairFailures = 5
	pairPenalty     = time.Second
)

var (
	errBusy     = errors.New("receiver is busy with another transfer")
	errDeclined = errors.New("declined")
)

// OfferInfo is what the receiver learned while preparing an offer.
type OfferInfo struct {
	Dest    string
	Already int64 // bytes already here: identical files and resumed partials
	Skipped int   // files already here
	Resumed bool  // continues an interrupted transfer
	Renames map[string]string
	Raw     bool // data streams will be unencrypted
}

// Receiver accepts transfers on a listener, one at a time.
type Receiver struct {
	Dest     string
	Policy   store.Policy
	StateDir string // resume journals; "" disables resume
	Code     string // pairing code; Serve generates one if empty
	AllowRaw bool   // honor senders asking for unencrypted data streams
	SockBuf  int

	// Confirm is asked before accepting an offer; nil accepts everything.
	Confirm func(o *proto.Offer, info OfferInfo) bool
	// Optional event hooks for UIs.
	OnStart      func(o *proto.Offer, info OfferInfo)
	OnDone       func(o *proto.Offer, r Result, err error)
	OnCodeChange func(code string)

	mu       sync.Mutex
	active   *session
	code     string
	failures int
	pairMu   sync.Mutex
	id       *secure.Identity
}

type session struct {
	id     proto.SessionID
	key    string // identifies the offer at this destination (resume journal key)
	offer  *proto.Offer
	data   chan net.Conn
	cancel context.CancelCauseFunc
	done   chan struct{}

	// Set before the session is published; read by data connection handlers.
	raw     bool
	pairKey []byte
	peerFP  [32]byte

	running  atomic.Bool // accepted and streaming
	already  int64
	received atomic.Int64
}

// RecvStats is a live snapshot of the transfer in progress, if any.
type RecvStats struct {
	Active   bool
	From     string
	Received int64 // including bytes that were already here
	Total    int64
}

func (r *Receiver) Stats() RecvStats {
	r.mu.Lock()
	s := r.active
	r.mu.Unlock()
	if s == nil || !s.running.Load() {
		return RecvStats{}
	}
	return RecvStats{Active: true, From: s.offer.From, Received: s.already + s.received.Load(), Total: s.offer.Total}
}

// CurrentCode is the pairing code senders must use right now.
func (r *Receiver) CurrentCode() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.code == "" {
		r.code = r.Code
		if r.code == "" {
			r.code = secure.NewCode()
		}
	}
	return r.code
}

// Serve accepts connections until ctx is cancelled. With once set, it returns
// after the first transfer finishes, with that transfer's error.
func (r *Receiver) Serve(ctx context.Context, ln net.Listener, once bool) error {
	defer ln.Close()
	if r.id == nil {
		id, err := secure.NewIdentity()
		if err != nil {
			return err
		}
		r.id = id
	}
	r.CurrentCode()
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()

	first := make(chan error, 1)
	acceptErr := make(chan error, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				acceptErr <- err
				return
			}
			go func() {
				isSession, err := r.handle(ctx, c)
				if isSession && once && !errors.Is(err, errBusy) {
					select {
					case first <- err:
					default:
					}
				}
			}()
		}
	}()

	select {
	case err := <-first:
		return err
	case err := <-acceptErr:
		if ctx.Err() != nil {
			return nil
		}
		return err
	case <-ctx.Done():
		return nil
	}
}

// handle routes a new connection. It reports whether the connection ran a transfer session.
func (r *Receiver) handle(ctx context.Context, c net.Conn) (bool, error) {
	_ = c.SetDeadline(time.Now().Add(handshakeTimeout))
	h, err := proto.ReadHello(c)
	if err != nil {
		c.Close()
		return false, nil
	}
	tune(c, r.SockBuf)

	switch h.Kind {
	case proto.KindControl:
		paired, err := r.pair(c, h.Session)
		if err != nil {
			c.Close() // wrong codes don't end a --once receiver
			return false, nil
		}
		tc := r.id.Server(c, paired.PeerFP)
		if err := tc.HandshakeContext(ctx); err != nil {
			c.Close()
			return false, nil
		}
		_ = c.SetDeadline(time.Time{})
		return true, r.runSession(ctx, tc, h.Session, paired)

	case proto.KindData:
		r.mu.Lock()
		s := r.active
		r.mu.Unlock()
		if s == nil || s.id != h.Session {
			c.Close()
			return false, nil
		}
		var conn net.Conn = c
		if s.raw {
			var tok [32]byte
			want := secure.StreamToken(s.pairKey, h.Session, h.Stream)
			if _, err := io.ReadFull(c, tok[:]); err != nil || !hmac.Equal(tok[:], want[:]) {
				c.Close()
				return false, nil
			}
		} else {
			tc := r.id.Server(c, s.peerFP)
			if err := tc.HandshakeContext(ctx); err != nil {
				c.Close()
				return false, nil
			}
			conn = tc
		}
		_ = c.SetDeadline(time.Time{})
		select {
		case s.data <- conn:
		default:
			conn.Close() // more streams than offered
		}
	default:
		c.Close()
	}
	return false, nil
}

// pair authenticates a sender with the current code. Attempts are serialized
// and failures are slow, which keeps online guessing impractical.
func (r *Receiver) pair(c net.Conn, sid proto.SessionID) (secure.Paired, error) {
	r.pairMu.Lock()
	defer r.pairMu.Unlock()
	p, err := secure.PairListener(c, r.CurrentCode(), sid, r.id)
	if err == nil {
		r.mu.Lock()
		r.failures = 0
		r.mu.Unlock()
		return p, nil
	}
	if errors.Is(err, secure.ErrWrongCode) {
		r.mu.Lock()
		r.failures++
		rotated := ""
		if r.failures >= maxPairFailures {
			r.code, r.failures = secure.NewCode(), 0
			rotated = r.code
		}
		r.mu.Unlock()
		if rotated != "" && r.OnCodeChange != nil {
			r.OnCodeChange(rotated)
		}
		time.Sleep(pairPenalty)
	}
	return p, err
}

// claim makes s the active session. A reconnecting sender (same files, same
// destination) replaces its own dead session instead of being told "busy".
func (r *Receiver) claim(s *session) error {
	r.mu.Lock()
	a := r.active
	if a == nil {
		r.active = s
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	if s.key == "" || a.key != s.key {
		return errBusy
	}
	a.cancel(errors.New("replaced by a reconnecting sender"))
	select {
	case <-a.done:
	case <-time.After(10 * time.Second):
		return errBusy
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != nil {
		return errBusy
	}
	r.active = s
	return nil
}

func validateOffer(o *proto.Offer) error {
	switch {
	case o.Streams < 1 || o.Streams > MaxStreams:
		return fmt.Errorf("stream count %d out of range", o.Streams)
	case o.BlockSize < MinBlockSize || o.BlockSize > MaxBlockSize:
		return fmt.Errorf("block size %d out of range", o.BlockSize)
	case o.Total < 0:
		return errors.New("negative total size")
	}
	if !o.Bench {
		var sum int64
		for _, f := range o.Files {
			sum += f.Size
		}
		if sum != o.Total {
			return errors.New("offer total does not match its files")
		}
	}
	return nil
}

func (r *Receiver) runSession(parent context.Context, ctrl net.Conn, sid proto.SessionID, paired secure.Paired) (err error) {
	defer ctrl.Close()

	_ = ctrl.SetReadDeadline(time.Now().Add(30 * time.Second))
	typ, body, err := proto.ReadMsg(ctrl)
	_ = ctrl.SetReadDeadline(time.Time{})
	if err != nil {
		return err
	}
	if typ != proto.MsgOffer {
		return fmt.Errorf("expected offer, got message %d", typ)
	}
	var offer proto.Offer
	if err := proto.Decode(body, &offer); err != nil {
		return err
	}
	var ctrlMu sync.Mutex
	sendErr := func(e error, fatal bool) error {
		ctrlMu.Lock()
		defer ctrlMu.Unlock()
		_ = proto.WriteMsg(ctrl, proto.MsgError, proto.ErrorMsg{Message: e.Error(), Fatal: fatal})
		return e
	}
	if err := validateOffer(&offer); err != nil {
		return sendErr(err, true)
	}

	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	s := &session{
		id: sid, offer: &offer, data: make(chan net.Conn, offer.Streams),
		cancel: cancel, done: make(chan struct{}),
		raw: offer.Raw && r.AllowRaw, pairKey: paired.Key, peerFP: paired.PeerFP,
	}
	if !offer.Bench {
		if dest, err := filepath.Abs(r.Dest); err == nil {
			s.key = store.JournalKey(dest, offer.Files)
		}
	}
	defer close(s.done)
	if err := r.claim(s); err != nil {
		return sendErr(err, false)
	}
	defer func() {
		r.mu.Lock()
		r.active = nil
		r.mu.Unlock()
	}()

	info := OfferInfo{Raw: s.raw}
	var plan *store.Plan
	if !offer.Bench {
		plan, err = store.Prepare(r.Dest, offer.Files, store.Options{Policy: r.Policy, StateDir: r.StateDir})
		if err != nil {
			return sendErr(err, true)
		}
		info.Dest, info.Already, info.Skipped = plan.Root(), plan.Already(), len(plan.Skip())
		info.Resumed, info.Renames = plan.Resumed(), plan.Renames()
	}

	materialized := false
	var res Result
	defer func() {
		if err != nil && materialized {
			plan.Suspend()
		}
		if r.OnDone != nil {
			r.OnDone(&offer, res, err)
		}
	}()

	if r.Confirm != nil && !r.Confirm(&offer, info) {
		_ = proto.WriteMsg(ctrl, proto.MsgAccept, proto.Accept{OK: false, Reason: "declined by the receiver"})
		return errDeclined
	}
	if plan != nil {
		materialized = true
		if err := plan.Materialize(); err != nil {
			return sendErr(err, true)
		}
	}
	accept := proto.Accept{OK: true, Raw: s.raw, Dest: info.Dest, Already: info.Already}
	if plan != nil {
		accept.Skip, accept.Have = plan.Skip(), plan.Have()
	}
	s.already = info.Already
	if err := proto.WriteMsg(ctrl, proto.MsgAccept, accept); err != nil {
		return err
	}
	s.running.Store(true)
	if r.OnStart != nil {
		r.OnStart(&offer, info)
	}
	start := time.Now()

	// The sender sends nothing more on the control connection, so any read
	// result means it went away.
	go func() {
		_, _, _ = proto.ReadMsg(ctrl)
		cancel(errors.New("sender disconnected"))
	}()

	// Collect the data streams.
	conns := make([]net.Conn, 0, offer.Streams)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	timeout := time.NewTimer(handshakeTimeout)
	defer timeout.Stop()
	for len(conns) < offer.Streams {
		select {
		case c := <-s.data:
			conns = append(conns, c)
			defer closeOnDone(ctx, c)()
		case <-timeout.C:
			return sendErr(errors.New("timed out waiting for data streams"), false)
		case <-ctx.Done():
			return sendErr(context.Cause(ctx), false)
		}
	}

	// Report progress to the sender.
	tickDone := make(chan struct{})
	var tickWG sync.WaitGroup
	tickWG.Add(1)
	go func() {
		defer tickWG.Done()
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				ctrlMu.Lock()
				_ = proto.WriteMsg(ctrl, proto.MsgProgress, proto.Progress{Bytes: s.already + s.received.Load()})
				ctrlMu.Unlock()
			case <-tickDone:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	var small *smallWriter
	if plan != nil {
		small = newSmallWriter(plan, smallFileWorkers)
	}
	pool := newBufPool(offer.BlockSize)
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := recvStream(ctx, cancel, c, &offer, plan, small, s, pool); err != nil {
				cancel(err)
			}
		}()
	}
	wg.Wait()
	if small != nil {
		small.close()
	}
	close(tickDone)
	tickWG.Wait()

	if ctx.Err() != nil {
		return sendErr(context.Cause(ctx), false)
	}
	if got, want := s.received.Load(), offer.Total-s.already; got != want {
		return sendErr(fmt.Errorf("received %d of %d bytes", got, want), false)
	}
	files := 0
	if plan != nil {
		if err := plan.Finish(); err != nil {
			return sendErr(err, false)
		}
		for _, f := range offer.Files {
			if !f.Dir {
				files++
			}
		}
	}
	res = Result{Bytes: s.received.Load(), Already: s.already, Files: files, Elapsed: time.Since(start)}
	ctrlMu.Lock()
	err = proto.WriteMsg(ctrl, proto.MsgComplete, proto.Complete{
		Bytes: res.Bytes, Already: res.Already, Files: res.Files, ElapsedNS: int64(res.Elapsed),
	})
	ctrlMu.Unlock()
	return err
}

type inBlock struct {
	h   proto.FrameHeader
	buf *[]byte
}

// recvStream runs one data stream: this goroutine reads frames off the socket
// while a writer goroutine verifies and writes them to disk.
func recvStream(ctx context.Context, cancel context.CancelCauseFunc, conn net.Conn, offer *proto.Offer, plan *store.Plan, small *smallWriter, s *session, pool *bufPool) error {
	ch := make(chan inBlock, pipelineDepth)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		failed := false
		for b := range ch {
			if !failed {
				if err := commit(b, offer, plan, small, s); err != nil {
					failed = true
					cancel(err) // closes every connection, unblocking all readers
				}
			}
			pool.put(b.buf)
		}
	}()
	defer func() { close(ch); <-writerDone }()

	br := bufio.NewReaderSize(conn, 256<<10)
	var hb [proto.FrameHeaderSize]byte
	for {
		if ctx.Err() != nil {
			return nil
		}
		_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
		if _, err := io.ReadFull(br, hb[:]); err != nil {
			return fmt.Errorf("data stream: %w", err)
		}
		h := proto.ParseFrameHeader(hb[:])
		if h.Flags&proto.FlagEnd != 0 {
			return nil
		}
		if h.Len == 0 || int(h.Len) > offer.BlockSize {
			return fmt.Errorf("bad frame length %d", h.Len)
		}
		buf := pool.get()
		if _, err := io.ReadFull(br, (*buf)[:h.Len]); err != nil {
			pool.put(buf)
			return fmt.Errorf("data stream: %w", err)
		}
		select {
		case ch <- inBlock{h: h, buf: buf}:
		case <-ctx.Done():
			pool.put(buf)
			return nil
		}
	}
}

func commit(b inBlock, offer *proto.Offer, plan *store.Plan, small *smallWriter, s *session) error {
	data := (*b.buf)[:b.h.Len]
	if !offer.NoHash && proto.Checksum(data) != b.h.Sum {
		return fmt.Errorf("checksum mismatch in file #%d at offset %d", b.h.File, b.h.Offset)
	}
	if b.h.Flags&proto.FlagPack != 0 {
		if small == nil {
			return errors.New("unexpected pack frame")
		}
		n, err := small.writePack(data)
		s.received.Add(n)
		return err
	}
	if plan != nil {
		if err := plan.WriteAt(b.h.File, int64(b.h.Offset), data); err != nil {
			return err
		}
	}
	s.received.Add(int64(len(data)))
	return nil
}

// smallWriter writes the files of a pack in parallel: creating, closing and
// renaming files costs far more than writing their few bytes.
type smallWriter struct {
	plan  *store.Plan
	tasks chan smallTask
	wg    sync.WaitGroup
}

type smallTask struct {
	file uint32
	data []byte
	done *sync.WaitGroup
	errs *firstError
}

func newSmallWriter(plan *store.Plan, workers int) *smallWriter {
	w := &smallWriter{plan: plan, tasks: make(chan smallTask, workers*2)}
	for range workers {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			for t := range w.tasks {
				if err := w.plan.WriteFile(t.file, t.data); err != nil {
					t.errs.set(err)
				}
				t.done.Done()
			}
		}()
	}
	return w
}

// writePack writes every file in a pack payload and returns the file bytes written.
func (w *smallWriter) writePack(p []byte) (int64, error) {
	var done sync.WaitGroup
	var errs firstError
	var n int64
	for len(p) > 0 {
		file, data, rest, err := proto.NextPackEntry(p)
		if err != nil {
			errs.set(err)
			break
		}
		done.Add(1)
		w.tasks <- smallTask{file: file, data: data, done: &done, errs: &errs}
		n += int64(len(data))
		p = rest
	}
	done.Wait()
	if err := errs.get(); err != nil {
		return 0, err
	}
	return n, nil
}

func (w *smallWriter) close() {
	close(w.tasks)
	w.wg.Wait()
}

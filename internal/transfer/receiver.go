package transfer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sshivamanand/bit-flow/internal/proto"
	"github.com/sshivamanand/bit-flow/internal/store"
)

// Receiver accepts transfers on a listener, one at a time.
type Receiver struct {
	Dest      string
	Overwrite bool
	SockBuf   int

	// Confirm is asked before accepting an offer; nil accepts everything.
	Confirm func(o *proto.Offer) bool
	// OnStart and OnDone are optional event hooks for UIs.
	OnStart func(o *proto.Offer)
	OnDone  func(o *proto.Offer, r Result, err error)

	mu     sync.Mutex
	active *session
}

type session struct {
	id       proto.SessionID
	offer    *proto.Offer
	data     chan net.Conn
	running  atomic.Bool // accepted and streaming
	received atomic.Int64
}

var errBusy = errors.New("receiver is busy with another transfer")

// RecvStats is a live snapshot of the transfer in progress, if any.
type RecvStats struct {
	Active   bool
	From     string
	Received int64
	Total    int64
}

func (r *Receiver) Stats() RecvStats {
	r.mu.Lock()
	s := r.active
	r.mu.Unlock()
	if s == nil || !s.running.Load() {
		return RecvStats{}
	}
	return RecvStats{Active: true, From: s.offer.From, Received: s.received.Load(), Total: s.offer.Total}
}

// Serve accepts connections until ctx is cancelled. With once set, it returns
// after the first transfer finishes, with that transfer's error.
func (r *Receiver) Serve(ctx context.Context, ln net.Listener, once bool) error {
	defer ln.Close()
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
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	h, err := proto.ReadHello(c)
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		c.Close()
		return false, nil
	}
	tune(c, r.SockBuf)

	switch h.Kind {
	case proto.KindControl:
		return true, r.runSession(ctx, c, h.Session)
	case proto.KindData:
		r.mu.Lock()
		s := r.active
		r.mu.Unlock()
		if s == nil || s.id != h.Session {
			c.Close()
			return false, nil
		}
		select {
		case s.data <- c:
		default:
			c.Close() // more streams than offered
		}
	default:
		c.Close()
	}
	return false, nil
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

func (r *Receiver) runSession(parent context.Context, ctrl net.Conn, id proto.SessionID) (err error) {
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
	sendErr := func(e error) error {
		ctrlMu.Lock()
		defer ctrlMu.Unlock()
		_ = proto.WriteMsg(ctrl, proto.MsgError, proto.ErrorMsg{Message: e.Error()})
		return e
	}
	if err := validateOffer(&offer); err != nil {
		return sendErr(err)
	}

	// Claim the receiver; one transfer at a time (including while the user decides).
	s := &session{id: id, offer: &offer, data: make(chan net.Conn, offer.Streams)}
	r.mu.Lock()
	if r.active != nil {
		r.mu.Unlock()
		return sendErr(errBusy)
	}
	r.active = s
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.active = nil
		r.mu.Unlock()
	}()

	var plan *store.Plan
	var res Result
	defer func() {
		if err != nil && plan != nil {
			plan.Abort()
		}
		if r.OnDone != nil {
			r.OnDone(&offer, res, err)
		}
	}()
	if !offer.Bench {
		if plan, err = store.Prepare(r.Dest, offer.Files, r.Overwrite); err != nil {
			return sendErr(err)
		}
	}
	if r.Confirm != nil && !r.Confirm(&offer) {
		_ = proto.WriteMsg(ctrl, proto.MsgAccept, proto.Accept{OK: false, Reason: "declined by the receiver"})
		return errors.New("declined")
	}
	if plan != nil {
		if err := plan.Materialize(); err != nil {
			return sendErr(err)
		}
	}
	if err := proto.WriteMsg(ctrl, proto.MsgAccept, proto.Accept{OK: true}); err != nil {
		return err
	}
	s.running.Store(true)
	if r.OnStart != nil {
		r.OnStart(&offer)
	}

	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	start := time.Now()

	// Watch the control connection: the sender sends nothing more, so any read
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
	timeout := time.NewTimer(15 * time.Second)
	defer timeout.Stop()
	for len(conns) < offer.Streams {
		select {
		case c := <-s.data:
			conns = append(conns, c)
			defer closeOnDone(ctx, c)()
		case <-timeout.C:
			return sendErr(errors.New("timed out waiting for data streams"))
		case <-ctx.Done():
			return context.Cause(ctx)
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
				_ = proto.WriteMsg(ctrl, proto.MsgProgress, proto.Progress{Bytes: s.received.Load()})
				ctrlMu.Unlock()
			case <-tickDone:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	pool := newBufPool(offer.BlockSize)
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := recvStream(ctx, cancel, c, &offer, plan, s, pool); err != nil {
				cancel(err)
			}
		}()
	}
	wg.Wait()
	close(tickDone)
	tickWG.Wait()

	if ctx.Err() != nil {
		return sendErr(context.Cause(ctx))
	}
	if got := s.received.Load(); got != offer.Total {
		return sendErr(fmt.Errorf("received %d of %d bytes", got, offer.Total))
	}
	files := 0
	if plan != nil {
		if err := plan.Finish(); err != nil {
			return sendErr(err)
		}
		for _, f := range offer.Files {
			if !f.Dir {
				files++
			}
		}
	}
	res = Result{Bytes: offer.Total, Files: files, Elapsed: time.Since(start)}
	ctrlMu.Lock()
	err = proto.WriteMsg(ctrl, proto.MsgComplete, proto.Complete{Bytes: res.Bytes, Files: res.Files, ElapsedNS: int64(res.Elapsed)})
	ctrlMu.Unlock()
	return err
}

type inBlock struct {
	h   proto.FrameHeader
	buf *[]byte
}

// recvStream runs one data stream: this goroutine reads frames off the socket
// while a writer goroutine verifies and writes them to disk.
func recvStream(ctx context.Context, cancel context.CancelCauseFunc, conn net.Conn, offer *proto.Offer, plan *store.Plan, s *session, pool *bufPool) error {
	ch := make(chan inBlock, pipelineDepth)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		failed := false
		for b := range ch {
			if !failed {
				if err := commit(b, offer, plan, s, pool); err != nil {
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

func commit(b inBlock, offer *proto.Offer, plan *store.Plan, s *session, pool *bufPool) error {
	data := (*b.buf)[:b.h.Len]
	if !offer.NoHash && proto.Checksum(data) != b.h.Sum {
		return fmt.Errorf("checksum mismatch in file #%d at offset %d", b.h.File, b.h.Offset)
	}
	if plan != nil {
		if err := plan.WriteAt(b.h.File, int64(b.h.Offset), data); err != nil {
			return err
		}
	}
	s.received.Add(int64(len(data)))
	return nil
}

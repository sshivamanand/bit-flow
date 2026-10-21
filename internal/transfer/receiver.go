package transfer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sshivamanand/bit-flow/internal/proto"
	"github.com/sshivamanand/bit-flow/internal/steam"
	"github.com/sshivamanand/bit-flow/internal/store"
)

const (
	// maxPairFailures wrong codes in a row rotate the code; each failure also
	// costs pairPenalty, so guessing a 6-digit code online is hopeless.
	maxPairFailures = 5
	pairPenalty     = time.Second
)

var (
	errBusy      = errors.New("receiver is busy with another transfer")
	errDeclined  = errors.New("declined")
	errCancelled = errors.New("cancelled by the receiver")
)

// IsDeclined reports whether a transfer ended because the receiver said no.
func IsDeclined(err error) bool { return errors.Is(err, errDeclined) }

// OfferInfo is what the receiver learned while preparing an offer.
type OfferInfo struct {
	Dest    string
	Already int64 // bytes already here: identical files and resumed partials
	Skipped int   // files already here
	Resumed bool  // continues an interrupted transfer
	Renames map[string]string
	Raw     bool // data streams will be unencrypted

	Steam        *proto.SteamGame // the offer is a Steam game
	SteamLibrary string           // where it will be installed ("" = no Steam here; saved as a folder)
}

// Decision answers an offer. A non-empty Dest overrides the receiver's default folder.
type Decision struct {
	Accept bool
	Dest   string
}

// Receiver accepts transfers, one at a time: either listening for senders
// (Serve) or fetching from a sender that is waiting (Fetch).
type Receiver struct {
	Dest     string
	Policy   store.Policy
	StateDir string // resume journals; "" disables resume
	Code     string // pairing code when listening; generated if empty
	AllowRaw bool   // honor senders asking for unencrypted data streams
	SockBuf  int
	// SteamLibrary receives Steam games: "" finds this computer's main library,
	// "-" turns that off (games are saved like any folder).
	SteamLibrary string
	Retries      int // Fetch: reconnect attempts after a dropped connection; 0 = default, <0 = none

	// Confirm is asked before accepting an offer; nil accepts everything.
	Confirm func(o *proto.Offer, info OfferInfo) Decision
	// Probe answers discovery probes on the listening port; nil ignores them.
	Probe func() any
	// Optional event hooks for UIs.
	OnStart      func(o *proto.Offer, info OfferInfo)
	OnDone       func(o *proto.Offer, r Result, err error)
	OnCodeChange func(code string)
	OnRetry      func(attempt int, err error, wait time.Duration)

	hubOnce sync.Once
	hub     *hub
	hubErr  error

	mu     sync.Mutex
	active *session
}

type session struct {
	key    string // identifies the offer at its destination (resume journal key)
	offer  *proto.Offer
	cancel context.CancelCauseFunc
	done   chan struct{}

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

func (r *Receiver) ensureHub() (*hub, error) {
	r.hubOnce.Do(func() {
		r.hub, r.hubErr = newHub(r.Code, r.SockBuf)
		if r.hubErr != nil {
			return
		}
		r.hub.onCodeChange = func(c string) {
			if r.OnCodeChange != nil {
				r.OnCodeChange(c)
			}
		}
		r.hub.probe = func() any {
			if r.Probe != nil {
				return r.Probe()
			}
			return nil
		}
		r.hub.onControl = func(ctx context.Context, ctrl net.Conn, lk *hubLink) error {
			_, err := r.runSession(ctx, ctrl, lk)
			return err
		}
	})
	return r.hub, r.hubErr
}

// CurrentCode is the pairing code senders must use right now.
func (r *Receiver) CurrentCode() string {
	h, err := r.ensureHub()
	if err != nil {
		return ""
	}
	return h.currentCode()
}

// CheckCode verifies a pairing code typed into a browser (rate-limited like pairing).
func (r *Receiver) CheckCode(code string) bool {
	h, err := r.ensureHub()
	return err == nil && h.checkCode(code)
}

// Serve accepts senders until ctx is cancelled. With once set, it returns
// after the first transfer finishes, with that transfer's error.
func (r *Receiver) Serve(ctx context.Context, ln net.Listener, once bool) error {
	h, err := r.ensureHub()
	if err != nil {
		return err
	}
	return h.serve(ctx, ln, once)
}

// Fetch connects to a sender waiting at addr (bitflow send without --to) and
// receives its files. Dropped connections are retried and resume.
func (r *Receiver) Fetch(ctx context.Context, addr, code string) (Result, error) {
	h, err := r.ensureHub()
	if err != nil {
		return Result{}, err
	}
	retries := r.Retries
	switch {
	case retries == 0:
		retries = DefaultRetries
	case retries < 0:
		retries = 0
	}
	started := false
	for attempt := 1; ; attempt++ {
		ctrl, lk, err := dialControl(ctx, addr, code, h.id, r.SockBuf)
		var res Result
		if err == nil {
			res, err = r.runSession(ctx, ctrl, lk)
			ctrl.Close()
			if err == nil {
				return res, nil
			}
		}
		if res.started {
			started = true
		}
		if IsPermanent(err) || ctx.Err() != nil || attempt > retries || !started {
			return Result{}, err
		}
		wait := backoff(attempt)
		if r.OnRetry != nil {
			r.OnRetry(attempt, err, wait)
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return Result{}, err
		}
	}
}

func (r *Receiver) steamLibrary() string {
	switch r.SteamLibrary {
	case "-":
		return ""
	case "":
		if libs := steam.Libraries(); len(libs) > 0 {
			return libs[0]
		}
		return ""
	}
	return r.SteamLibrary
}

// SetDefaults changes the destination and conflict policy for future offers.
func (r *Receiver) SetDefaults(dest string, policy store.Policy) {
	r.mu.Lock()
	r.Dest, r.Policy = dest, policy
	r.mu.Unlock()
}

// CancelActive stops the transfer in progress, if any. Partial files are kept
// (resume), but the sender is told not to retry.
func (r *Receiver) CancelActive() {
	r.mu.Lock()
	s := r.active
	r.mu.Unlock()
	if s != nil {
		s.cancel(errCancelled)
	}
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

// runSession runs the receiver's side of one transfer on a secured control connection.
func (r *Receiver) runSession(parent context.Context, ctrl net.Conn, lk link) (res Result, err error) {
	defer ctrl.Close()

	_ = ctrl.SetReadDeadline(time.Now().Add(30 * time.Second))
	typ, body, err := proto.ReadMsg(ctrl)
	_ = ctrl.SetReadDeadline(time.Time{})
	if err != nil {
		return res, err
	}
	if typ == proto.MsgError {
		return res, remoteError(body)
	}
	if typ != proto.MsgOffer {
		return res, fmt.Errorf("expected offer, got message %d", typ)
	}
	var offer proto.Offer
	if err := proto.Decode(body, &offer); err != nil {
		return res, err
	}
	var ctrlMu sync.Mutex
	sendErr := func(e error, fatal bool) error {
		ctrlMu.Lock()
		defer ctrlMu.Unlock()
		_ = proto.WriteMsg(ctrl, proto.MsgError, proto.ErrorMsg{Message: e.Error(), Fatal: fatal})
		if fatal {
			return permanent(e)
		}
		return e
	}
	if err := validateOffer(&offer); err != nil {
		return res, sendErr(err, true)
	}

	r.mu.Lock()
	defaultDest, policy := r.Dest, r.Policy
	r.mu.Unlock()
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	s := &session{offer: &offer, cancel: cancel, done: make(chan struct{})}
	if !offer.Bench {
		if dest, err := filepath.Abs(defaultDest); err == nil {
			s.key = store.JournalKey(dest, offer.Files)
		}
	}
	defer close(s.done)
	if err := r.claim(s); err != nil {
		return res, sendErr(err, false)
	}
	defer func() {
		r.mu.Lock()
		r.active = nil
		r.mu.Unlock()
	}()

	raw := offer.Raw && r.AllowRaw
	steamLib := ""
	if offer.Steam != nil && !offer.Bench {
		if steamLib = r.steamLibrary(); steamLib != "" {
			// Straight into steamapps/, updating an older copy in place.
			defaultDest, policy = filepath.Join(steamLib, "steamapps"), store.PolicyUpdate
		}
	}
	prepare := func(dest string) (*store.Plan, OfferInfo, error) {
		info := OfferInfo{Raw: raw, Steam: offer.Steam, SteamLibrary: steamLib}
		if offer.Bench {
			return nil, info, nil
		}
		plan, err := store.Prepare(dest, offer.Files, store.Options{Policy: policy, StateDir: r.StateDir})
		if err != nil {
			return nil, info, err
		}
		info.Dest, info.Already, info.Skipped = plan.Root(), plan.Already(), len(plan.Skip())
		info.Resumed, info.Renames = plan.Resumed(), plan.Renames()
		return plan, info, nil
	}
	plan, info, err := prepare(defaultDest)
	if err != nil {
		return res, sendErr(err, true)
	}

	materialized := false
	defer func() {
		if err != nil && materialized {
			plan.Suspend()
		}
		if r.OnDone != nil {
			r.OnDone(&offer, res, err)
		}
	}()

	if r.Confirm != nil {
		d := r.Confirm(&offer, info)
		if !d.Accept {
			_ = proto.WriteMsg(ctrl, proto.MsgAccept, proto.Accept{OK: false, Reason: "declined by the receiver"})
			return res, permanent(errDeclined)
		}
		if d.Dest != "" && !offer.Bench && d.Dest != info.Dest {
			if plan, info, err = prepare(d.Dest); err != nil {
				return res, sendErr(err, true)
			}
		}
	}
	if plan != nil {
		materialized = true
		if err := plan.Materialize(); err != nil {
			return res, sendErr(err, true)
		}
	}
	accept := proto.Accept{OK: true, Raw: raw, Dest: info.Dest, Already: info.Already, Addrs: localAddrs()}
	if plan != nil {
		accept.Skip, accept.Have = plan.Skip(), plan.Have()
	}
	s.already = info.Already
	lk.expect(offer.Streams, raw)
	if err := proto.WriteMsg(ctrl, proto.MsgAccept, accept); err != nil {
		return res, err
	}
	s.running.Store(true)
	res.started = true
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

	lk.usePeerAddrs(offer.Addrs)
	conns, _, err := lk.streams(ctx, offer.Streams, raw)
	if err != nil {
		return res, sendErr(err, false)
	}
	dec, err := newDecompressor(offer.BlockSize)
	if err != nil {
		return res, sendErr(err, false)
	}
	defer dec.close()
	for _, c := range conns {
		defer c.Close()
		defer closeOnDone(ctx, c)()
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
			if err := recvStream(ctx, cancel, c, &offer, plan, small, s, pool, dec); err != nil {
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
		cause := context.Cause(ctx)
		return res, sendErr(cause, errors.Is(cause, errCancelled))
	}
	if got, want := s.received.Load(), offer.Total-s.already; got != want {
		return res, sendErr(fmt.Errorf("received %d of %d bytes", got, want), false)
	}
	files := 0
	if plan != nil {
		if err := plan.Finish(); err != nil {
			return res, sendErr(err, false)
		}
		for _, f := range offer.Files {
			if !f.Dir {
				files++
			}
		}
	}
	res = Result{Bytes: s.received.Load(), Already: s.already, Files: files, Elapsed: time.Since(start), started: true}
	ctrlMu.Lock()
	err = proto.WriteMsg(ctrl, proto.MsgComplete, proto.Complete{
		Bytes: res.Bytes, Already: res.Already, Files: res.Files, ElapsedNS: int64(res.Elapsed),
	})
	ctrlMu.Unlock()
	return res, err
}

type inBlock struct {
	h   proto.FrameHeader
	buf *[]byte
}

// recvStream runs one data stream: this goroutine reads frames off the socket
// while a writer goroutine verifies and writes them to disk.
func recvStream(ctx context.Context, cancel context.CancelCauseFunc, conn net.Conn, offer *proto.Offer, plan *store.Plan, small *smallWriter, s *session, pool *bufPool, dec *decompressor) error {
	ch := make(chan inBlock, pipelineDepth)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		failed := false
		for b := range ch {
			if !failed {
				if err := commit(b, offer, plan, small, s, pool, dec); err != nil {
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

func commit(b inBlock, offer *proto.Offer, plan *store.Plan, small *smallWriter, s *session, pool *bufPool, dec *decompressor) error {
	data := (*b.buf)[:b.h.Len]
	if b.h.Flags&proto.FlagCompressed != 0 {
		if b.h.RawLen == 0 || int(b.h.RawLen) > offer.BlockSize {
			return fmt.Errorf("bad decompressed length %d", b.h.RawLen)
		}
		rbuf := pool.get()
		defer pool.put(rbuf)
		out, err := dec.dec.DecodeAll(data, (*rbuf)[:0])
		if err != nil {
			return fmt.Errorf("decompress: %w", err)
		}
		if len(out) != int(b.h.RawLen) {
			return fmt.Errorf("decompressed %d bytes, expected %d", len(out), b.h.RawLen)
		}
		data = out
	}
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

package transfer

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/sshivamanand/bit-flow/internal/proto"
	"github.com/sshivamanand/bit-flow/internal/secure"
)

// Connection establishment is separate from session logic: either peer may be
// the one listening. Normally the receiver listens and the sender dials, but a
// sender can also wait for a receiver to come and fetch (handy when one
// laptop's firewall blocks incoming connections). The dialing side pairs as
// the PAKE "dialer" and is the TLS client; data streams follow the same rule.

// link supplies a session's data connections, however they are established.
type link interface {
	// expect is called before the message that makes the peer open streams
	// (the accept for a listening receiver, the offer for a listening sender).
	expect(n int, allowRaw bool)
	// streams returns n authenticated data connections. raw tells a dialing
	// side which kind to open.
	streams(ctx context.Context, n int, raw bool) ([]net.Conn, error)
	close()
}

// ---- dialing side ----

type dialLink struct {
	addr    string
	sid     proto.SessionID
	paired  secure.Paired
	id      *secure.Identity
	sockBuf int
}

// dialControl opens, pairs and secures a control connection to addr.
func dialControl(ctx context.Context, addr, code string, id *secure.Identity, sockBuf int) (net.Conn, *dialLink, error) {
	var sid proto.SessionID
	if _, err := rand.Read(sid[:]); err != nil {
		return nil, nil, err
	}
	conn, err := dial(ctx, addr, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	stop := closeOnDone(ctx, conn)
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := proto.WriteHello(conn, proto.Hello{Kind: proto.KindControl, Session: sid}); err != nil {
		conn.Close()
		return nil, nil, err
	}
	paired, err := secure.PairDialer(conn, code, sid, id)
	if err != nil {
		conn.Close()
		if errors.Is(err, secure.ErrWrongCode) {
			return nil, nil, permanent(fmt.Errorf("%w: check the code shown on the other computer", err))
		}
		return nil, nil, fmt.Errorf("pairing: %w", err)
	}
	ctrl := id.Client(conn, paired.PeerFP)
	if err := ctrl.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("secure channel: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	return ctrl, &dialLink{addr: addr, sid: sid, paired: paired, id: id, sockBuf: sockBuf}, nil
}

func (l *dialLink) expect(int, bool) {}
func (l *dialLink) close()           {}

// streams dials n data connections in parallel and authenticates each one.
func (l *dialLink) streams(ctx context.Context, n int, raw bool) ([]net.Conn, error) {
	conns := make([]net.Conn, n)
	var errs firstError
	var wg sync.WaitGroup
	for i := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := dial(ctx, l.addr, l.sockBuf)
			if err != nil {
				errs.set(err)
				return
			}
			conns[i] = c
			_ = c.SetDeadline(time.Now().Add(handshakeTimeout))
			kind := proto.KindData
			if raw {
				kind = proto.KindDataRaw
			}
			if err := proto.WriteHello(c, proto.Hello{Kind: kind, Session: l.sid, Stream: uint16(i)}); err != nil {
				errs.set(err)
				return
			}
			if raw {
				tok := secure.StreamToken(l.paired.Key, l.sid, uint16(i))
				if _, err := c.Write(tok[:]); err != nil {
					errs.set(err)
					return
				}
			} else {
				tc := l.id.Client(c, l.paired.PeerFP)
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
	if err := errs.get(); err != nil {
		for _, c := range conns {
			if c != nil {
				c.Close()
			}
		}
		return nil, fmt.Errorf("open data streams: %w", err)
	}
	return conns, nil
}

// ---- listening side ----

// hub accepts connections for a listening peer: it pairs control connections
// against the current code and routes data connections to the session
// expecting them. It also answers discovery probes.
type hub struct {
	id           *secure.Identity
	sockBuf      int
	onCodeChange func(code string)
	// onControl runs a session on a paired, TLS-secured control connection.
	onControl func(ctx context.Context, ctrl net.Conn, lk *hubLink) error
	// probe returns this device's discovery info (KindProbe); nil ignores probes.
	probe func() any

	mu       sync.Mutex
	code     string
	failures int
	sinks    map[proto.SessionID]*sink
	pairMu   sync.Mutex
}

// sink collects the data streams of one session, authenticated with that
// session's pairing material.
type sink struct {
	allowRaw bool
	key      []byte
	peerFP   [32]byte
	ch       chan net.Conn
}

func newHub(code string, sockBuf int) (*hub, error) {
	id, err := secure.NewIdentity()
	if err != nil {
		return nil, err
	}
	if code == "" {
		code = secure.NewCode()
	}
	return &hub{id: id, sockBuf: sockBuf, code: code, sinks: map[proto.SessionID]*sink{}}, nil
}

func (h *hub) currentCode() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.code
}

// serve accepts connections until ctx is cancelled. With once set, it returns
// after the first session ends, with that session's error.
func (h *hub) serve(ctx context.Context, ln net.Listener, once bool) error {
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
				isSession, err := h.handle(ctx, c)
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

// handle routes a new connection and reports whether it ran a session.
func (h *hub) handle(ctx context.Context, c net.Conn) (bool, error) {
	_ = c.SetDeadline(time.Now().Add(handshakeTimeout))
	hello, err := proto.ReadHello(c)
	if err != nil {
		c.Close()
		return false, nil
	}
	tune(c, h.sockBuf)

	switch hello.Kind {
	case proto.KindControl:
		paired, err := h.pair(c, hello.Session)
		if err != nil {
			c.Close() // wrong codes don't end a once-mode listener
			return false, nil
		}
		tc := h.id.Server(c, paired.PeerFP)
		if err := tc.HandshakeContext(ctx); err != nil {
			c.Close()
			return false, nil
		}
		_ = c.SetDeadline(time.Time{})
		lk := &hubLink{h: h, sid: hello.Session, paired: paired}
		defer lk.close()
		return true, h.onControl(ctx, tc, lk)

	case proto.KindData, proto.KindDataRaw:
		h.mu.Lock()
		sk := h.sinks[hello.Session]
		h.mu.Unlock()
		if sk == nil {
			c.Close()
			return false, nil
		}
		conn, err := sk.authenticate(ctx, h.id, c, hello)
		if err != nil {
			c.Close()
			return false, nil
		}
		_ = c.SetDeadline(time.Time{})
		select {
		case sk.ch <- conn:
		default:
			conn.Close() // more streams than expected
		}

	case proto.KindProbe:
		defer c.Close()
		if h.probe != nil {
			_ = proto.WriteMsg(c, proto.MsgInfo, h.probe())
		}

	default:
		c.Close()
	}
	return false, nil
}

// pair authenticates a dialer with the current code. Attempts are serialized
// and failures are slow, which keeps online guessing impractical.
func (h *hub) pair(c net.Conn, sid proto.SessionID) (secure.Paired, error) {
	h.pairMu.Lock()
	defer h.pairMu.Unlock()
	p, err := secure.PairListener(c, h.currentCode(), sid, h.id)
	if err == nil {
		h.mu.Lock()
		h.failures = 0
		h.mu.Unlock()
		return p, nil
	}
	if errors.Is(err, secure.ErrWrongCode) {
		h.mu.Lock()
		h.failures++
		rotated := ""
		if h.failures >= maxPairFailures {
			h.code, h.failures = secure.NewCode(), 0
			rotated = h.code
		}
		h.mu.Unlock()
		if rotated != "" && h.onCodeChange != nil {
			h.onCodeChange(rotated)
		}
		time.Sleep(pairPenalty)
	}
	return p, err
}

// hubLink is a listening session's view of its incoming data streams.
type hubLink struct {
	h      *hub
	sid    proto.SessionID
	paired secure.Paired
	sink   *sink
}

func (l *hubLink) expect(n int, allowRaw bool) {
	l.sink = &sink{allowRaw: allowRaw, key: l.paired.Key, peerFP: l.paired.PeerFP, ch: make(chan net.Conn, n)}
	l.h.mu.Lock()
	l.h.sinks[l.sid] = l.sink
	l.h.mu.Unlock()
}

func (l *hubLink) streams(ctx context.Context, n int, _ bool) ([]net.Conn, error) {
	if l.sink == nil {
		return nil, errors.New("internal: streams before expect")
	}
	conns := make([]net.Conn, 0, n)
	timeout := time.NewTimer(handshakeTimeout)
	defer timeout.Stop()
	for len(conns) < n {
		select {
		case c := <-l.sink.ch:
			conns = append(conns, c)
		case <-timeout.C:
			for _, c := range conns {
				c.Close()
			}
			return nil, errors.New("timed out waiting for data streams")
		case <-ctx.Done():
			for _, c := range conns {
				c.Close()
			}
			return nil, context.Cause(ctx)
		}
	}
	return conns, nil
}

func (l *hubLink) close() {
	if l.sink == nil {
		return
	}
	l.h.mu.Lock()
	if l.h.sinks[l.sid] == l.sink {
		delete(l.h.sinks, l.sid)
	}
	l.h.mu.Unlock()
	for {
		select {
		case c := <-l.sink.ch:
			c.Close()
		default:
			return
		}
	}
}

func (sk *sink) authenticate(ctx context.Context, id *secure.Identity, c net.Conn, hello proto.Hello) (net.Conn, error) {
	if hello.Kind == proto.KindDataRaw {
		if !sk.allowRaw {
			return nil, errors.New("raw stream not allowed for this session")
		}
		var tok [32]byte
		want := secure.StreamToken(sk.key, hello.Session, hello.Stream)
		if _, err := io.ReadFull(c, tok[:]); err != nil || !hmac.Equal(tok[:], want[:]) {
			return nil, errors.New("bad stream token")
		}
		return c, nil
	}
	tc := id.Server(c, sk.peerFP)
	if err := tc.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	return tc, nil
}

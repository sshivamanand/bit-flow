// Package web serves bit-flow to plain browsers on the LAN: the /get page
// that hands out bit-flow binaries (so a friend's laptop needs no internet),
// and simple upload/download pages for phones. It shares the transfer port:
// each new connection's first bytes tell a bit-flow peer ("BFLW") from a
// browser ("GET ", "POST", …), so one firewall rule covers everything.
package web

import (
	"bufio"
	"net"
	"sync"
	"time"
)

// Split returns two listeners fed by ln: peers gets bit-flow connections and
// browsers gets HTTP. Closing either closes ln.
func Split(ln net.Listener) (peers, browsers net.Listener) {
	m := &mux{ln: ln, peers: make(chan net.Conn), browsers: make(chan net.Conn), done: make(chan struct{})}
	go m.run()
	return &side{m: m, ch: m.peers}, &side{m: m, ch: m.browsers}
}

type mux struct {
	ln       net.Listener
	peers    chan net.Conn
	browsers chan net.Conn
	done     chan struct{}
	once     sync.Once
	err      error
}

func (m *mux) run() {
	for {
		c, err := m.ln.Accept()
		if err != nil {
			m.err = err
			m.close()
			return
		}
		go m.route(c)
	}
}

// route peeks at the first bytes without consuming them.
func (m *mux) route(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReaderSize(c, 512)
	head, err := br.Peek(4)
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		c.Close()
		return
	}
	pc := &peekedConn{Conn: c, r: br}
	ch := m.peers
	if isHTTP(head) {
		ch = m.browsers
	}
	select {
	case ch <- pc:
	case <-m.done:
		c.Close()
	}
}

func isHTTP(head []byte) bool {
	switch string(head) {
	case "GET ", "POST", "PUT ", "HEAD", "OPTI", "DELE", "PATC":
		return true
	}
	return false
}

func (m *mux) close() {
	m.once.Do(func() {
		close(m.done)
		m.ln.Close()
	})
}

type side struct {
	m  *mux
	ch chan net.Conn
}

func (s *side) Accept() (net.Conn, error) {
	select {
	case c := <-s.ch:
		return c, nil
	case <-s.m.done:
		if s.m.err != nil {
			return nil, s.m.err
		}
		return nil, net.ErrClosed
	}
}

func (s *side) Close() error   { s.m.close(); return nil }
func (s *side) Addr() net.Addr { return s.m.ln.Addr() }

// peekedConn replays the peeked bytes before reading from the connection.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *peekedConn) Read(b []byte) (int, error) { return p.r.Read(b) }

// Unwrap exposes the underlying connection (e.g. for TCP socket options).
func (p *peekedConn) Unwrap() net.Conn { return p.Conn }

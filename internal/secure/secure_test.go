package secure

import (
	"bytes"
	"errors"
	"net"
	"testing"

	"github.com/sshivamanand/bit-flow/internal/proto"
)

func pair(t *testing.T, dialerCode, listenerCode string) (Paired, error, Paired, error, *Identity, *Identity) {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ida, _ := NewIdentity()
	idb, _ := NewIdentity()
	var sid proto.SessionID
	sid[3] = 9

	type res struct {
		p   Paired
		err error
	}
	ch := make(chan res, 1)
	go func() {
		p, err := PairListener(b, listenerCode, sid, idb)
		b.Close()
		ch <- res{p, err}
	}()
	pd, errd := PairDialer(a, dialerCode, sid, ida)
	a.Close()
	r := <-ch
	return pd, errd, r.p, r.err, ida, idb
}

func TestPairSameCode(t *testing.T) {
	pd, errd, pl, errl, ida, idb := pair(t, "123456", "123456")
	if errd != nil || errl != nil {
		t.Fatalf("dialer=%v listener=%v", errd, errl)
	}
	if !bytes.Equal(pd.Key, pl.Key) {
		t.Fatal("keys differ")
	}
	if pd.PeerFP != idb.FP || pl.PeerFP != ida.FP {
		t.Fatal("fingerprints not exchanged correctly")
	}
}

func TestPairWrongCode(t *testing.T) {
	_, errd, _, errl, _, _ := pair(t, "123456", "654321")
	if !errors.Is(errd, ErrWrongCode) {
		t.Fatalf("dialer error = %v, want ErrWrongCode", errd)
	}
	if !errors.Is(errl, ErrWrongCode) {
		t.Fatalf("listener error = %v, want ErrWrongCode", errl)
	}
}

func TestPinnedTLS(t *testing.T) {
	ida, _ := NewIdentity()
	idb, _ := NewIdentity()
	idc, _ := NewIdentity() // an impostor

	// Real TCP, not net.Pipe: a rejected handshake leaves both sides writing,
	// which deadlocks an unbuffered pipe.
	handshake := func(server *Identity, serverExpects [32]byte) error {
		a, b := tcpPair(t)
		defer a.Close()
		defer b.Close()
		errc := make(chan error, 1)
		go func() { errc <- server.Server(b, serverExpects).Handshake(); b.Close() }()
		cerr := ida.Client(a, idb.FP).Handshake()
		a.Close()
		serr := <-errc
		if cerr != nil {
			return cerr
		}
		return serr
	}
	if err := handshake(idb, ida.FP); err != nil {
		t.Fatalf("pinned handshake failed: %v", err)
	}
	if err := handshake(idc, ida.FP); err == nil {
		t.Fatal("client accepted an impostor server")
	}
	if err := handshake(idb, idc.FP); err == nil {
		t.Fatal("server accepted an unexpected client")
	}
}

func TestCodes(t *testing.T) {
	c := NewCode()
	if len(c) != 6 || NormalizeCode(FormatCode(c)) != c {
		t.Fatalf("code %q", c)
	}
	if NormalizeCode(" 12 3-45 6\n") != "123456" {
		t.Fatal("normalize")
	}
}

func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

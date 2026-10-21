// Package secure authenticates bit-flow peers and encrypts their connections.
//
// The receiver shows a short numeric code. Both sides run CPace, a balanced
// PAKE, over that code on the still-plain control connection. CPace yields a
// shared key without revealing the code to eavesdroppers, and an active
// attacker gets exactly one guess per attempt (no offline brute force). Each
// side then proves it holds the key by MACing the handshake transcript plus
// its ephemeral TLS certificate fingerprint. From then on every connection is
// TLS 1.3 with both certificates pinned to those fingerprints. Raw-mode data
// streams (trusted cables, maximum speed) skip TLS but must present an HMAC
// token derived from the key, so they still can't be injected by others.
package secure

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"time"

	"filippo.io/cpace"

	"github.com/sshivamanand/bit-flow/internal/proto"
)

// ErrWrongCode means the peers used different pairing codes.
var ErrWrongCode = errors.New("wrong pairing code")

// Identity is this process's ephemeral TLS identity.
type Identity struct {
	cert tls.Certificate
	FP   [32]byte // SHA-256 of the DER certificate
}

func NewIdentity() (*Identity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 63))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "bit-flow"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(30 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &Identity{
		cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		FP:   sha256.Sum256(der),
	}, nil
}

func (id *Identity) config(peer [32]byte) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{id.cert},
		// Chain verification is replaced by pinning the fingerprint agreed during pairing.
		InsecureSkipVerify: true,
		ClientAuth:         tls.RequireAnyClientCert,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 || sha256.Sum256(raw[0]) != peer {
				return errors.New("peer certificate does not match the paired device")
			}
			return nil
		},
	}
}

// Client wraps c as the TLS client side, pinned to peer.
func (id *Identity) Client(c net.Conn, peer [32]byte) *tls.Conn {
	return tls.Client(c, id.config(peer))
}

// Server wraps c as the TLS server side, pinned to peer.
func (id *Identity) Server(c net.Conn, peer [32]byte) *tls.Conn {
	return tls.Server(c, id.config(peer))
}

// ---- pairing codes ----

// NewCode returns a random 6-digit code.
func NewCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%06d", n.Int64())
}

// FormatCode renders "123456" as "123-456" for display.
func FormatCode(c string) string {
	if len(c) == 6 {
		return c[:3] + "-" + c[3:]
	}
	return c
}

// NormalizeCode keeps only the digits of user input ("123 456", "123-456").
func NormalizeCode(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ---- pairing handshake ----

// Paired is the outcome of a successful pairing.
type Paired struct {
	PeerFP [32]byte
	Key    []byte
}

const maxBlob = 1024

func contextInfo(sid proto.SessionID) *cpace.ContextInfo {
	return cpace.NewContextInfo("bitflow-dialer", "bitflow-listener", append([]byte("bitflow/v2 pairing "), sid[:]...))
}

// PairDialer runs the pairing handshake as the side that opened the connection.
func PairDialer(rw io.ReadWriter, code string, sid proto.SessionID, me *Identity) (Paired, error) {
	msgA, st, err := cpace.Start(code, contextInfo(sid))
	if err != nil {
		return Paired{}, err
	}
	if err := writeBlob(rw, msgA); err != nil {
		return Paired{}, err
	}
	msgB, err := readBlob(rw)
	if err != nil {
		return Paired{}, err
	}
	var peerFP [32]byte
	peerMAC := make([]byte, sha256.Size)
	if _, err := io.ReadFull(rw, peerFP[:]); err != nil {
		return Paired{}, err
	}
	if _, err := io.ReadFull(rw, peerMAC); err != nil {
		return Paired{}, err
	}
	key, err := st.Finish(msgB)
	if err != nil {
		return Paired{}, err
	}
	t := transcript(sid, msgA, msgB, peerFP[:])
	if !hmac.Equal(peerMAC, confirm(key, "listener", t)) {
		return Paired{}, ErrWrongCode
	}
	t = append(t, me.FP[:]...)
	if _, err := rw.Write(append(me.FP[:], confirm(key, "dialer", t)...)); err != nil {
		return Paired{}, err
	}
	return Paired{PeerFP: peerFP, Key: key}, nil
}

// PairListener runs the pairing handshake as the side that accepted the connection.
func PairListener(rw io.ReadWriter, code string, sid proto.SessionID, me *Identity) (Paired, error) {
	msgA, err := readBlob(rw)
	if err != nil {
		return Paired{}, err
	}
	msgB, key, err := cpace.Exchange(code, contextInfo(sid), msgA)
	if err != nil {
		return Paired{}, err
	}
	t := transcript(sid, msgA, msgB, me.FP[:])
	out := appendBlob(nil, msgB)
	out = append(out, me.FP[:]...)
	out = append(out, confirm(key, "listener", t)...)
	if _, err := rw.Write(out); err != nil {
		return Paired{}, err
	}
	var peerFP [32]byte
	peerMAC := make([]byte, sha256.Size)
	if _, err := io.ReadFull(rw, peerFP[:]); err != nil {
		return Paired{}, ErrWrongCode // a dialer with the wrong code hangs up here
	}
	if _, err := io.ReadFull(rw, peerMAC); err != nil {
		return Paired{}, ErrWrongCode
	}
	t = append(t, peerFP[:]...)
	if !hmac.Equal(peerMAC, confirm(key, "dialer", t)) {
		return Paired{}, ErrWrongCode
	}
	return Paired{PeerFP: peerFP, Key: key}, nil
}

// StreamToken authenticates a raw (non-TLS) data stream.
func StreamToken(key []byte, sid proto.SessionID, stream uint16) [32]byte {
	k := derive(key, "raw stream")
	m := hmac.New(sha256.New, k)
	m.Write(sid[:])
	m.Write([]byte{byte(stream >> 8), byte(stream)})
	var out [32]byte
	copy(out[:], m.Sum(nil))
	return out
}

func derive(key []byte, label string) []byte {
	k, err := hkdf.Expand(sha256.New, key, "bitflow/v2 "+label, 32)
	if err != nil {
		panic(err)
	}
	return k
}

func confirm(key []byte, role string, transcript []byte) []byte {
	m := hmac.New(sha256.New, derive(key, "confirm "+role))
	m.Write(transcript)
	return m.Sum(nil)
}

// transcript length-prefixes every part so no two different handshakes encode the same bytes.
func transcript(sid proto.SessionID, parts ...[]byte) []byte {
	t := append([]byte(nil), sid[:]...)
	for _, p := range parts {
		t = appendBlob(t, p)
	}
	return t
}

func appendBlob(dst, b []byte) []byte {
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(b)))
	return append(dst, b...)
}

func writeBlob(w io.Writer, b []byte) error {
	_, err := w.Write(appendBlob(nil, b))
	return err
}

func readBlob(r io.Reader) ([]byte, error) {
	var n [2]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint16(n[:])
	if size > maxBlob {
		return nil, errors.New("pairing message too large")
	}
	b := make([]byte, size)
	_, err := io.ReadFull(r, b)
	return b, err
}

// Package discovery finds other bit-flow devices on the local network.
//
// A scanner sends a small JSON query several ways at once: to a link-local
// multicast group on every interface, to each subnet's directed broadcast
// address, and to the limited broadcast address. Responders (any running
// `bitflow recv`, a sender waiting to be fetched, the UI) answer by unicast.
// Some networks (guest Wi-Fi, managed APs) drop multicast and broadcast, so if
// nothing answers, the scanner falls back to probing every address of each
// local /24 over TCP. A typed address or QR code still works when even that is
// blocked.
package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/sshivamanand/bit-flow/internal/proto"
)

// Group is the multicast group queries go to (link-local, never routed).
var Group = net.IPv4(224, 0, 0, 177)

// Roles a device can announce.
const (
	RoleReceive = "recv" // accepting files
	RoleSend    = "send" // waiting for a receiver to fetch an offer
)

// Info is what a device tells scanners about itself.
type Info struct {
	Proto int    `json:"bitflow"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	OS    string `json:"os"`
	Port  int    `json:"port"` // TCP port for transfers
	Role  string `json:"role"`
	Offer string `json:"offer,omitempty"` // RoleSend: what's on offer, e.g. "BigGame (98 GB)"
}

// Peer is a discovered device.
type Peer struct {
	Info
	Addrs []string `json:"addrs"` // IPs it answered from
	Via   string   `json:"via"`   // multicast, broadcast, unicast or scan
}

// Addr is the host:port to connect to.
func (p Peer) Addr() string {
	if len(p.Addrs) == 0 {
		return ""
	}
	return net.JoinHostPort(p.Addrs[0], strconv.Itoa(p.Port))
}

type query struct {
	Proto int    `json:"bitflow"`
	Query bool   `json:"q"`
	From  string `json:"from,omitempty"`
}

// Respond answers discovery queries on UDP port until ctx is cancelled.
// info is called for every answer, so it can report current state.
func Respond(ctx context.Context, port int, info func() Info) error {
	lc := net.ListenConfig{Control: reuseControl}
	pc, err := lc.ListenPacket(ctx, "udp4", ":"+strconv.Itoa(port))
	if err != nil {
		return err
	}
	defer pc.Close()
	stop := context.AfterFunc(ctx, func() { pc.Close() })
	defer stop()

	p := ipv4.NewPacketConn(pc)
	for _, ifi := range multicastInterfaces() {
		_ = p.JoinGroup(&ifi, &net.UDPAddr{IP: Group})
	}

	buf := make([]byte, 2048)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		var q query
		if json.Unmarshal(buf[:n], &q) != nil || !q.Query || q.Proto == 0 {
			continue
		}
		me := info()
		if q.From != "" && q.From == me.ID {
			continue
		}
		b, _ := json.Marshal(me)
		_, _ = pc.WriteTo(b, src)
	}
}

// ScanOptions tune a scan. Zero values mean defaults.
type ScanOptions struct {
	Port    int           // UDP and TCP port peers listen on (default 7700)
	Wait    time.Duration // how long to listen for answers (default 1.5 s)
	SelfID  string        // our own ID, to ignore ourselves
	NoProbe bool          // skip the TCP subnet probe fallback
	Targets []string      // extra hosts to ask directly (unicast)
}

// Scan returns the devices that answered, sorted by name.
func Scan(ctx context.Context, o ScanOptions) ([]Peer, error) {
	if o.Port == 0 {
		o.Port = proto.DefaultPort
	}
	if o.Wait == 0 {
		o.Wait = 1500 * time.Millisecond
	}
	pc, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return nil, err
	}
	defer pc.Close()
	p := ipv4.NewPacketConn(pc)
	_ = p.SetMulticastTTL(1)
	_ = p.SetMulticastLoopback(true)

	found := newPeerSet(o.SelfID)
	q, _ := json.Marshal(query{Proto: proto.Version, Query: true, From: o.SelfID})
	send := func() {
		group := &net.UDPAddr{IP: Group, Port: o.Port}
		for _, ifi := range multicastInterfaces() {
			if p.SetMulticastInterface(&ifi) == nil {
				_, _ = pc.WriteTo(q, group)
			}
		}
		for _, b := range broadcastAddrs() {
			_, _ = pc.WriteTo(q, &net.UDPAddr{IP: b, Port: o.Port})
		}
		for _, h := range o.Targets {
			if ip := net.ParseIP(h); ip != nil {
				_, _ = pc.WriteTo(q, &net.UDPAddr{IP: ip, Port: o.Port})
			}
		}
	}

	deadline := time.Now().Add(o.Wait)
	_ = pc.SetReadDeadline(deadline)
	go func() {
		// Repeat the query: UDP on Wi-Fi drops packets, especially broadcast.
		for i := 0; i < 3 && time.Now().Before(deadline) && ctx.Err() == nil; i++ {
			send()
			time.Sleep(o.Wait / 4)
		}
	}()
	stop := context.AfterFunc(ctx, func() { pc.Close() })
	defer stop()

	buf := make([]byte, 2048)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			break
		}
		var info Info
		if json.Unmarshal(buf[:n], &info) != nil || info.Proto == 0 || info.ID == "" {
			continue
		}
		ip := src.(*net.UDPAddr).IP.String()
		via := "multicast"
		for _, t := range o.Targets {
			if t == ip {
				via = "unicast"
			}
		}
		found.add(info, ip, via)
	}
	if ctx.Err() != nil {
		return found.list(), ctx.Err()
	}

	if found.empty() && !o.NoProbe {
		ProbeHosts(ctx, subnetHosts(), o.Port, 400*time.Millisecond, func(info Info, ip string) {
			found.add(info, ip, "scan")
		})
	}
	return found.list(), nil
}

// ProbeHosts asks each host over TCP (KindProbe) for its Info, in parallel.
func ProbeHosts(ctx context.Context, hosts []string, port int, timeout time.Duration, found func(Info, string)) {
	sem := make(chan struct{}, 128)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, h := range hosts {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			info, err := probe(ctx, h, port, timeout)
			if err == nil {
				mu.Lock()
				found(info, h)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
}

func probe(ctx context.Context, host string, port int, timeout time.Duration) (Info, error) {
	d := net.Dialer{Timeout: timeout}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return Info{}, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * timeout))
	if err := proto.WriteHello(c, proto.Hello{Kind: proto.KindProbe}); err != nil {
		return Info{}, err
	}
	typ, body, err := proto.ReadMsg(c)
	if err != nil {
		return Info{}, err
	}
	var info Info
	if typ != proto.MsgInfo || json.Unmarshal(body, &info) != nil || info.ID == "" {
		return Info{}, errors.New("not a bit-flow device")
	}
	return info, nil
}

// Find scans and returns the peers whose name matches (case-insensitive;
// an exact match wins over prefix matches).
func Find(ctx context.Context, name string, o ScanOptions) ([]Peer, error) {
	peers, err := Scan(ctx, o)
	if err != nil {
		return nil, err
	}
	var exact, prefix []Peer
	for _, p := range peers {
		switch n := strings.ToLower(p.Name); {
		case n == strings.ToLower(name):
			exact = append(exact, p)
		case strings.HasPrefix(n, strings.ToLower(name)):
			prefix = append(prefix, p)
		}
	}
	if len(exact) > 0 {
		return exact, nil
	}
	return prefix, nil
}

// ---- interfaces ----

func multicastInterfaces() []net.Interface {
	ifs, _ := net.Interfaces()
	var out []net.Interface
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp != 0 && ifi.Flags&net.FlagMulticast != 0 && hasIPv4(ifi) {
			out = append(out, ifi)
		}
	}
	return out
}

func hasIPv4(ifi net.Interface) bool {
	addrs, _ := ifi.Addrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return true
		}
	}
	return false
}

// localNets returns the IPv4 networks of up, non-loopback interfaces.
func localNets() []*net.IPNet {
	ifs, _ := net.Interfaces()
	var out []*net.IPNet
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
				out = append(out, &net.IPNet{IP: n.IP.To4(), Mask: n.Mask})
			}
		}
	}
	return out
}

func broadcastAddrs() []net.IP {
	out := []net.IP{net.IPv4bcast}
	for _, n := range localNets() {
		if ones, bits := n.Mask.Size(); bits != 32 || ones >= 31 {
			continue
		}
		b := make(net.IP, 4)
		for i := range 4 {
			b[i] = n.IP[i] | ^n.Mask[i]
		}
		out = append(out, b)
	}
	return out
}

// subnetHosts lists every other address in the /24 around each local address.
// Larger subnets are clamped to the local /24 to keep the probe to ~1 second.
func subnetHosts() []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range localNets() {
		ip := n.IP.To4()
		if ip[0] == 169 && ip[1] == 254 {
			continue // link-local /16: too large; direct cables are found via multicast
		}
		for i := 1; i < 255; i++ {
			h := net.IPv4(ip[0], ip[1], ip[2], byte(i)).String()
			if byte(i) != ip[3] && !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	return out
}

// ---- results ----

type peerSet struct {
	self  string
	mu    sync.Mutex
	peers map[string]*Peer
}

func newPeerSet(self string) *peerSet { return &peerSet{self: self, peers: map[string]*Peer{}} }

func (s *peerSet) add(info Info, ip, via string) {
	if info.ID == s.self {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[info.ID]
	if !ok {
		p = &Peer{Info: info, Via: via}
		s.peers[info.ID] = p
	}
	for _, a := range p.Addrs {
		if a == ip {
			return
		}
	}
	p.Addrs = append(p.Addrs, ip)
}

func (s *peerSet) empty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.peers) == 0
}

func (s *peerSet) list() []Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Peer, 0, len(s.peers))
	for _, p := range s.peers {
		sort.Slice(p.Addrs, func(i, j int) bool { return addrRank(p.Addrs[i]) < addrRank(p.Addrs[j]) })
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// addrRank prefers ordinary LAN addresses over link-local and loopback ones.
func addrRank(a string) int {
	ip := net.ParseIP(a)
	switch {
	case ip == nil:
		return 3
	case ip.IsLoopback():
		return 2
	case ip.IsLinkLocalUnicast():
		return 1
	}
	return 0
}

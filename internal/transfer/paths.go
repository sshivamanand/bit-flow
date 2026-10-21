package transfer

import (
	"net"
	"sync"
	"time"

	"github.com/sshivamanand/bit-flow/internal/netinfo"
	"github.com/sshivamanand/bit-flow/internal/proto"
)

// Multipath: if the two computers share more than one link (Wi-Fi plus an
// Ethernet or Thunderbolt cable), data streams are spread over all of them.
// Every stream pulls from the same work queue, so a faster link automatically
// carries more of the data.

// path is one route to the peer.
type path struct {
	addr  string // host:port to dial
	local string // our source IP for it; distinct source = distinct link
	label string // our interface's name, e.g. "Wi-Fi"
}

// localAddrs lists this machine's interface addresses for offers and accepts.
func localAddrs() []proto.NetAddr {
	ifs, _ := netinfo.List()
	var out []proto.NetAddr
	for _, ifc := range ifs {
		for _, ip := range ifc.Addrs {
			out = append(out, proto.NetAddr{IP: ip.String(), Label: ifc.Label})
		}
	}
	return out
}

// sourceIP is the local address the OS would use to reach host. Connecting a
// UDP socket sends nothing; it only asks the routing table.
func sourceIP(host string) string {
	c, err := net.Dial("udp", net.JoinHostPort(host, "9"))
	if err != nil {
		return ""
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}

// probeTimeout bounds how long an extra link may take to answer. Some
// networks (and macOS's firewall) drop rather than refuse connections, so an
// unreachable address would otherwise stall stream setup for many seconds.
const probeTimeout = 700 * time.Millisecond

// virtual interfaces never make a second physical link.
var virtualLabels = map[string]bool{"VPN / tunnel": true, "Virtual": true, "Bridge": true}

// selectPaths returns the primary path plus any peer address that leaves
// through a different local interface and answers a quick probe.
func selectPaths(primary string, peer []proto.NetAddr) []path {
	host, port, err := net.SplitHostPort(primary)
	if err != nil {
		return []path{{addr: primary}}
	}
	labels := map[string]string{}
	for _, a := range localAddrs() {
		labels[a.IP] = a.Label
	}
	first := path{addr: primary, local: sourceIP(host)}
	first.label = labels[first.local]
	paths := []path{first}
	seen := map[string]bool{first.local: true}
	primaryLoop := net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
	var candidates []path
	for _, a := range peer {
		ip := net.ParseIP(a.IP)
		if ip == nil || ip.To4() == nil || a.IP == host || virtualLabels[a.Label] {
			continue
		}
		if ip.IsLoopback() && !primaryLoop {
			continue
		}
		local := sourceIP(a.IP)
		if local == "" || seen[local] {
			continue // same link as a path we already have
		}
		if virtualLabels[labels[local]] {
			continue
		}
		seen[local] = true
		candidates = append(candidates, path{addr: net.JoinHostPort(a.IP, port), local: local, label: labels[local]})
	}

	// Keep only links that answer quickly; probe them all at once.
	ok := make([]bool, len(candidates))
	var wg sync.WaitGroup
	for i, c := range candidates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok[i] = probeLink(c.addr)
		}()
	}
	wg.Wait()
	for i, c := range candidates {
		if ok[i] {
			paths = append(paths, c)
		}
	}
	return paths
}

// probeLink checks that a bit-flow peer answers at addr. The probe hello makes
// the peer close the connection right away instead of waiting for a handshake.
func probeLink(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, probeTimeout)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(probeTimeout))
	return proto.WriteHello(c, proto.Hello{Kind: proto.KindProbe}) == nil
}

// Package advisor turns what bit-flow knows about the network path into the
// one piece of advice that would speed a transfer up the most. No software
// can make the radio faster, but the physical setup often can.
package advisor

import (
	"fmt"
	"net"
	"strings"
)

// Link is one end of the path a transfer uses.
type Link struct {
	Label string // interface name, e.g. "Wi-Fi", "Ethernet", "Thunderbolt Bridge"
	IP    string
}

func wireless(label string) bool {
	l := strings.ToLower(label)
	return strings.Contains(l, "wi-fi") || strings.Contains(l, "wifi") || strings.Contains(l, "wlan") ||
		strings.Contains(l, "wireless") || strings.Contains(l, "airport")
}

func sameSubnet24(a, b string) bool {
	x, y := net.ParseIP(a).To4(), net.ParseIP(b).To4()
	return x != nil && y != nil && x[0] == y[0] && x[1] == y[1] && x[2] == y[2]
}

// Tips returns advice for a transfer from local to peer over links distinct links.
func Tips(local, peer Link, links []string) []string {
	var tips []string
	if len(links) > 1 {
		tips = append(tips, "using "+strings.Join(links, " + ")+" at once")
	}
	if len(links) <= 1 && wireless(local.Label) && wireless(peer.Label) && sameSubnet24(local.IP, peer.IP) {
		tips = append(tips, "both computers are on Wi-Fi through the same router, so every byte crosses the air twice. "+
			"Plugging either one into the router with a cable roughly doubles the speed.")
	}
	if ip := net.ParseIP(peer.IP).To4(); ip != nil && ip[0] == 169 && ip[1] == 254 {
		tips = append(tips, "direct cable link")
	}
	return tips
}

// Unreachable explains a peer that discovery found but TCP can't reach.
func Unreachable(name string) string {
	return fmt.Sprintf("found %s, but couldn't connect to it. This network may block device-to-device traffic "+
		"(client isolation, common on guest and hotel Wi-Fi). Try a phone hotspot or a cable, or run "+
		"`bitflow send … --wait` on this computer and `bitflow recv --from` on the other.", name)
}

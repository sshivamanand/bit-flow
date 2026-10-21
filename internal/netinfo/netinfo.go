// Package netinfo lists the machine's usable network interfaces with human-friendly labels
// (Wi-Fi, Ethernet, Thunderbolt Bridge, ...), so users can pick the fastest path.
package netinfo

import (
	"bufio"
	"bytes"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Iface is an up, non-loopback interface with at least one IPv4 address.
type Iface struct {
	Name  string
	Label string
	Addrs []net.IP
}

// List returns usable interfaces in OS order.
func List() ([]Iface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	labels := hardwareLabels()
	var out []Iface
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		var ips []net.IP
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ip4 := ipn.IP.To4(); ip4 != nil {
				ips = append(ips, ip4)
			}
		}
		if len(ips) == 0 {
			continue
		}
		out = append(out, Iface{Name: ifc.Name, Label: label(ifc.Name, labels), Addrs: ips})
	}
	return out, nil
}

func label(name string, labels map[string]string) string {
	if l, ok := labels[name]; ok {
		return l
	}
	switch {
	case strings.HasPrefix(name, "utun"), strings.HasPrefix(name, "tun"), strings.HasPrefix(name, "wg"):
		return "VPN / tunnel"
	case strings.HasPrefix(name, "bridge"):
		return "Bridge"
	case strings.HasPrefix(name, "docker"), strings.HasPrefix(name, "veth"), strings.HasPrefix(name, "vmnet"):
		return "Virtual"
	}
	return name
}

// hardwareLabels maps interface names to friendly hardware names where the OS exposes them.
func hardwareLabels() map[string]string {
	m := map[string]string{}
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("networksetup", "-listallhardwareports").Output()
		if err != nil {
			return m
		}
		var port string
		sc := bufio.NewScanner(bytes.NewReader(out))
		for sc.Scan() {
			line := sc.Text()
			if v, ok := strings.CutPrefix(line, "Hardware Port: "); ok {
				port = v
			} else if v, ok := strings.CutPrefix(line, "Device: "); ok && port != "" {
				m[v] = port
				port = ""
			}
		}
	case "linux":
		ifs, _ := net.Interfaces()
		for _, ifc := range ifs {
			if _, err := os.Stat("/sys/class/net/" + ifc.Name + "/wireless"); err == nil {
				m[ifc.Name] = "Wi-Fi"
			} else if strings.HasPrefix(ifc.Name, "en") || strings.HasPrefix(ifc.Name, "eth") {
				m[ifc.Name] = "Ethernet"
			} else if strings.HasPrefix(ifc.Name, "thunderbolt") {
				m[ifc.Name] = "Thunderbolt"
			}
		}
	}
	// Windows interface names are already friendly ("Wi-Fi", "Ethernet 2").
	return m
}

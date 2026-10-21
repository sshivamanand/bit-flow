package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/sshivamanand/bit-flow/internal/config"
	"github.com/sshivamanand/bit-flow/internal/discovery"
	"github.com/sshivamanand/bit-flow/internal/proto"
)

func cmdPeers(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("peers", flag.ContinueOnError)
	wait := fs.Duration("wait", 1500*time.Millisecond, "how long to listen for answers")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	fmt.Println("Looking for bit-flow devices…")
	peers, err := discovery.Scan(ctx, discovery.ScanOptions{Wait: *wait, SelfID: cfg.ID})
	if err != nil {
		return err
	}
	if len(peers) == 0 {
		fmt.Println("None found. Is `bitflow recv` running on the other computer, on the same network?")
		fmt.Println("Some networks block device discovery; then use the address it prints (--to 192.168.x.y).")
		return nil
	}
	for _, p := range peers {
		what := "receiving files"
		if p.Role == discovery.RoleSend {
			what = "offering " + p.Offer
		}
		fmt.Printf("  %-22s %-8s %-30s %s (found via %s)\n", p.Name, p.OS, what, strings.Join(p.Addrs, ", "), p.Via)
	}
	return nil
}

// selfInfo describes this device for discovery answers.
func selfInfo(cfg *config.Config, port int, role, offer string) func() discovery.Info {
	return func() discovery.Info {
		return discovery.Info{
			Proto: proto.Version, ID: cfg.ID, Name: cfg.DeviceName(), OS: runtime.GOOS,
			Port: port, Role: role, Offer: offer,
		}
	}
}

// looksLikeAddress reports whether target is an IP, host:port, or DNS name
// rather than a bit-flow device name.
func looksLikeAddress(target string) bool {
	return net.ParseIP(target) != nil || strings.ContainsAny(target, ".:")
}

// resolve turns --to/--from into host:port: addresses pass through, device
// names are looked up with discovery among peers in the given role.
func resolve(ctx context.Context, target, role string, cfg *config.Config) (string, error) {
	if looksLikeAddress(target) {
		return withPort(target), nil
	}
	peers, err := discovery.Find(ctx, target, discovery.ScanOptions{SelfID: cfg.ID})
	if err != nil {
		return "", err
	}
	var match []discovery.Peer
	for _, p := range peers {
		if p.Role == role {
			match = append(match, p)
		}
	}
	switch len(match) {
	case 0:
		return "", fmt.Errorf("no device named %q found on this network (try its address instead)", target)
	case 1:
		return match[0].Addr(), nil
	}
	var names []string
	for _, p := range match {
		names = append(names, p.Name+" ("+p.Addrs[0]+")")
	}
	return "", fmt.Errorf("%q matches several devices: %s", target, strings.Join(names, ", "))
}

// choosePeer scans for peers in role and lets the user pick one. It returns
// nil if none were found.
func choosePeer(ctx context.Context, role string, cfg *config.Config) (*discovery.Peer, error) {
	peers, err := discovery.Scan(ctx, discovery.ScanOptions{SelfID: cfg.ID})
	if err != nil {
		return nil, err
	}
	var match []discovery.Peer
	for _, p := range peers {
		if p.Role == role {
			match = append(match, p)
		}
	}
	switch {
	case len(match) == 0:
		return nil, nil
	case len(match) == 1:
		return &match[0], nil
	case !isTerminal():
		return nil, errors.New("several receivers found; pick one with --to NAME")
	}
	for i, p := range match {
		fmt.Printf("  %d) %s  (%s)\n", i+1, p.Name, strings.Join(p.Addrs, ", "))
	}
	fmt.Print("Send to which one? ")
	line, _ := stdin.ReadString('\n')
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(match) {
		return nil, errors.New("no receiver chosen")
	}
	return &match[n-1], nil
}

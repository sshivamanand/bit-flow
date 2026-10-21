package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/sshivamanand/bit-flow/internal/awake"
	"github.com/sshivamanand/bit-flow/internal/config"
	"github.com/sshivamanand/bit-flow/internal/discovery"
	"github.com/sshivamanand/bit-flow/internal/netinfo"
	"github.com/sshivamanand/bit-flow/internal/planner"
	"github.com/sshivamanand/bit-flow/internal/proto"
	"github.com/sshivamanand/bit-flow/internal/secure"
	"github.com/sshivamanand/bit-flow/internal/transfer"
	"github.com/sshivamanand/bit-flow/internal/units"
)

func cmdSend(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	to := fs.String("to", "", "receiver: its device NAME or HOST[:PORT] (default: find one on the network)")
	code := fs.String("code", "", "pairing code shown by the receiver (asked for if omitted)")
	wait := fs.Bool("wait", false, "don't look for a receiver; wait for one to fetch (bitflow recv --from)")
	port := fs.Int("port", proto.DefaultPort, "with --wait: TCP/UDP port to listen on")
	retries := fs.Int("retries", transfer.DefaultRetries, "reconnect attempts after a dropped connection")
	opts := transferFlags(fs)
	paths, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return errors.New("nothing to send: bitflow send PATH... [--to NAME|HOST]")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	m, err := planner.Build(paths)
	if err != nil {
		return err
	}
	for _, s := range m.Skipped {
		fmt.Printf("  skipping %s (not a regular file)\n", s)
	}
	o := opts()
	o.Name = cfg.DeviceName()
	o.Retries = *retries
	if o.Retries == 0 {
		o.Retries = -1
	}
	o.OnRetry = func(attempt int, err error, wait time.Duration) {
		fmt.Printf("\n⟳ %v\n  reconnecting in %s (attempt %d of %d)…\n", err, wait, attempt, *retries)
	}

	addr := ""
	switch {
	case *wait:
	case *to != "":
		if addr, err = resolve(ctx, *to, discovery.RoleReceive, cfg); err != nil {
			return err
		}
	default:
		fmt.Println("Looking for receivers…")
		p, err := choosePeer(ctx, discovery.RoleReceive, cfg)
		if err != nil {
			return err
		}
		if p != nil {
			fmt.Printf("Found %s (%s)\n", p.Name, p.Addrs[0])
			addr = p.Addr()
		} else {
			fmt.Println("No receiver found, so this computer will wait for one to fetch the files.")
		}
	}

	release := awake.Hold()
	defer release()
	var res transfer.Result
	if addr == "" {
		res, err = waitForReceiver(ctx, &o, m, *port, *code, cfg)
	} else {
		var s *transfer.Sender
		if s, err = withCode(o, *code); err != nil {
			return err
		}
		fmt.Printf("Sending %d files, %s → %s over %d streams\n", m.FileCount(), units.Bytes(m.Total), addr, o.Streams)
		fmt.Println("Waiting for the receiver to accept…")
		stop := watchSender(s)
		res, err = s.Send(ctx, addr, m)
		stop()
	}
	if err != nil {
		return err
	}
	fmt.Printf("✓ Sent %d files: %s%s\n", res.Files, summary(res.Bytes, res.Elapsed), alreadyNote(res.Already))
	return nil
}

func withCode(o transfer.Options, flagVal string) (*transfer.Sender, error) {
	code, err := readCode(flagVal)
	if err != nil {
		return nil, err
	}
	o.Code = code
	return transfer.NewSender(o), nil
}

// waitForReceiver serves the files until a receiver fetches them (reverse mode).
func waitForReceiver(ctx context.Context, o *transfer.Options, m *planner.Manifest, port int, code string, cfg *config.Config) (transfer.Result, error) {
	o.Code = secure.NormalizeCode(code)
	if o.Code == "" {
		o.Code = secure.NewCode()
	}
	o.OnCodeChange = func(c string) {
		fmt.Printf("\n⚠ Several wrong pairing codes were tried. New pairing code: %s\n", secure.FormatCode(c))
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return transfer.Result{}, err
	}
	offer := fmt.Sprintf("%s (%s)", topName(m), units.Bytes(m.Total))
	go discovery.Respond(ctx, proto.DefaultPort, selfInfo(cfg, port, discovery.RoleSend, offer))

	name := cfg.DeviceName()
	fmt.Printf("\nOffering %s: %d files, %s\n\n", topName(m), m.FileCount(), units.Bytes(m.Total))
	fmt.Printf("  Pairing code:  %s\n", secure.FormatCode(o.Code))
	fmt.Printf("  Device name:   %s\n\n", name)
	fmt.Println("On the receiving computer, run:")
	fmt.Printf("  bitflow recv --to <folder> --from %s --code %s\n", name, o.Code)
	if ifs, _ := netinfo.List(); len(ifs) > 0 {
		fmt.Println("or by address:")
		for _, ifc := range ifs {
			for _, ip := range ifc.Addrs {
				fmt.Printf("  %-26s bitflow recv --to <folder> --from %s --code %s\n", ifc.Label+" ("+ifc.Name+")", withPortIf(ip.String(), port), o.Code)
			}
		}
	}
	fmt.Println("\nWaiting for a receiver… (Ctrl+C to stop)")
	s := transfer.NewSender(*o)
	stop := watchSender(s)
	defer stop()
	return s.Serve(ctx, ln, m)
}

func topName(m *planner.Manifest) string {
	var top []string
	for _, f := range m.Files {
		if !strings.Contains(f.Path, "/") {
			top = append(top, f.Path)
		}
	}
	if len(top) == 1 {
		return top[0]
	}
	return fmt.Sprintf("%d items", len(top))
}

func withPortIf(host string, port int) string {
	if port == proto.DefaultPort {
		return host
	}
	return net.JoinHostPort(host, fmt.Sprint(port))
}

func cmdBench(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	to := fs.String("to", "", "benchmark against a running `bitflow recv` at HOST[:PORT]")
	loopback := fs.Bool("loopback", false, "benchmark against an in-process receiver on 127.0.0.1")
	code := fs.String("code", "", "pairing code shown by the receiver (with --to)")
	size := sizeFlag(10e9)
	fs.Var(&size, "size", "bytes to send per run")
	sweep := fs.Bool("sweep", false, "run with 1, 2, 4, 8, 16 and 32 streams")
	opts := transferFlags(fs)
	if pos, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return fmt.Errorf("unexpected argument %q", pos[0])
	}
	if *loopback == (*to != "") {
		return errors.New("use exactly one of --loopback or --to HOST")
	}

	base := opts()
	addr := ""
	if *loopback {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		rctx, cancel := context.WithCancel(ctx)
		defer cancel()
		r := &transfer.Receiver{SockBuf: base.SockBuf, AllowRaw: true}
		base.Code = r.CurrentCode()
		go r.Serve(rctx, ln, false)
		addr = ln.Addr().String()
	} else {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if addr, err = resolve(ctx, *to, discovery.RoleReceive, cfg); err != nil {
			return err
		}
		if base.Code, err = readCode(*code); err != nil {
			return err
		}
	}

	counts := []int{base.Streams}
	if *sweep {
		counts = []int{1, 2, 4, 8, 16, 32}
	}
	mode := "TLS"
	if base.Raw {
		mode = "raw"
	}
	hash := "on"
	if base.NoHash {
		hash = "off"
	}
	fmt.Printf("Benchmark → %s: %s per run, block %s, %s, checksums %s\n",
		addr, units.Bytes(int64(size)), units.Bytes(int64(base.BlockSize)), mode, hash)

	for _, n := range counts {
		o := base
		o.Streams = n
		s := transfer.NewSender(o)
		var stop func()
		if !*sweep {
			stop = watchSender(s)
		}
		res, err := s.Bench(ctx, addr, int64(size))
		if stop != nil {
			stop()
		}
		if err != nil {
			return err
		}
		fmt.Printf("  %2d streams   %s\n", n, summary(res.Bytes, res.Elapsed))
	}
	return nil
}

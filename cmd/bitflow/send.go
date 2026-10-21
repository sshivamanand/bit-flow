package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"time"

	"github.com/sshivamanand/bit-flow/internal/awake"
	"github.com/sshivamanand/bit-flow/internal/planner"
	"github.com/sshivamanand/bit-flow/internal/transfer"
	"github.com/sshivamanand/bit-flow/internal/units"
)

func cmdSend(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	to := fs.String("to", "", "receiver address, HOST or HOST:PORT (shown when the receiver starts)")
	code := fs.String("code", "", "pairing code shown by the receiver (asked for if omitted)")
	retries := fs.Int("retries", transfer.DefaultRetries, "reconnect attempts after a dropped connection")
	opts := transferFlags(fs)
	paths, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return errors.New("nothing to send: bitflow send PATH... --to HOST")
	}
	if *to == "" {
		return errors.New("--to is required (the receiver prints its address when it starts)")
	}
	addr := withPort(*to)

	m, err := planner.Build(paths)
	if err != nil {
		return err
	}
	for _, s := range m.Skipped {
		fmt.Printf("  skipping %s (not a regular file)\n", s)
	}
	o := opts()
	if o.Code, err = readCode(*code); err != nil {
		return err
	}
	o.Retries = *retries
	if o.Retries == 0 {
		o.Retries = -1
	}
	o.OnRetry = func(attempt int, err error, wait time.Duration) {
		fmt.Printf("\n⟳ %v\n  reconnecting in %s (attempt %d of %d)…\n", err, wait, attempt, *retries)
	}

	fmt.Printf("Sending %d files, %s → %s over %d streams\n", m.FileCount(), units.Bytes(m.Total), addr, o.Streams)
	fmt.Println("Waiting for the receiver to accept…")

	release := awake.Hold()
	defer release()
	s := transfer.NewSender(o)
	stop := watchSender(s)
	res, err := s.Send(ctx, addr, m)
	stop()
	if err != nil {
		return err
	}
	fmt.Printf("✓ Sent %d files: %s%s\n", res.Files, summary(res.Bytes, res.Elapsed), alreadyNote(res.Already))
	return nil
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
		addr = withPort(*to)
		var err error
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

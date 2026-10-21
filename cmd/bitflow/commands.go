package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sshivamanand/bit-flow/internal/netinfo"
	"github.com/sshivamanand/bit-flow/internal/planner"
	"github.com/sshivamanand/bit-flow/internal/proto"
	"github.com/sshivamanand/bit-flow/internal/transfer"
	"github.com/sshivamanand/bit-flow/internal/units"
)

func cmdIfaces() error {
	ifs, err := netinfo.List()
	if err != nil {
		return err
	}
	if len(ifs) == 0 {
		fmt.Println("No active network interfaces with an IPv4 address.")
		return nil
	}
	for _, ifc := range ifs {
		var addrs []string
		for _, ip := range ifc.Addrs {
			addrs = append(addrs, ip.String())
		}
		fmt.Printf("  %-28s %s\n", fmt.Sprintf("%s (%s)", ifc.Label, ifc.Name), strings.Join(addrs, ", "))
	}
	return nil
}

// ---- recv ----

func cmdRecv(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("recv", flag.ContinueOnError)
	to := fs.String("to", ".", "folder to save received files into")
	port := fs.Int("port", proto.DefaultPort, "TCP port to listen on")
	yes := fs.Bool("yes", false, "accept transfers without asking")
	once := fs.Bool("once", false, "exit after one transfer")
	overwrite := fs.Bool("overwrite", false, "replace files that already exist at the destination")
	var sockBuf sizeFlag
	fs.Var(&sockBuf, "sockbuf", "socket buffer size (default: OS autotuning)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected argument %q (use --to DIR for the destination)", pos[0])
	}

	dest, err := filepath.Abs(*to)
	if err != nil {
		return err
	}
	if st, err := os.Stat(dest); err != nil || !st.IsDir() {
		return fmt.Errorf("destination folder %s does not exist", dest)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		return err
	}
	printRecvBanner(dest, *port)

	stdin := bufio.NewReader(os.Stdin)
	r := &transfer.Receiver{Dest: dest, Overwrite: *overwrite, SockBuf: int(sockBuf)}
	r.Confirm = func(o *proto.Offer) bool {
		if o.Bench {
			fmt.Printf("\n▶ Benchmark from %s: %s over %d streams (data is discarded)\n", o.From, units.Bytes(o.Total), o.Streams)
			return true
		}
		describeOffer(o, dest)
		if *yes {
			return true
		}
		fmt.Print("  Accept? [Y/n] ")
		line, _ := stdin.ReadString('\n')
		ans := strings.ToLower(strings.TrimSpace(line))
		return ans == "" || ans == "y" || ans == "yes"
	}

	var mu sync.Mutex
	var stopMeter func()
	r.OnStart = func(o *proto.Offer) {
		mu.Lock()
		defer mu.Unlock()
		stopMeter = runMeter(o.Total, func() int64 { return r.Stats().Received })
	}
	r.OnDone = func(o *proto.Offer, res transfer.Result, err error) {
		mu.Lock()
		if stopMeter != nil {
			stopMeter()
			stopMeter = nil
		}
		mu.Unlock()
		switch {
		case err != nil:
			fmt.Printf("✗ Transfer from %s failed: %v\n", o.From, err)
		case o.Bench:
			fmt.Printf("✓ Benchmark: %s\n", summary(res.Bytes, res.Elapsed))
		default:
			fmt.Printf("✓ Received %d files: %s\n", res.Files, summary(res.Bytes, res.Elapsed))
		}
		if !*once {
			fmt.Println("\nWaiting for the next transfer… (Ctrl+C to stop)")
		}
	}
	return r.Serve(ctx, ln, *once)
}

func printRecvBanner(dest string, port int) {
	fmt.Printf("bit-flow %s — receiving into %s\n", version, dest)
	ifs, _ := netinfo.List()
	if len(ifs) == 0 {
		fmt.Printf("Listening on port %d, but no network interfaces are up.\n", port)
		return
	}
	suffix := ""
	if port != proto.DefaultPort {
		suffix = fmt.Sprintf(":%d", port)
	}
	fmt.Println("On the sending computer, run one of these (pick the fastest link):")
	for _, ifc := range ifs {
		for _, ip := range ifc.Addrs {
			fmt.Printf("  %-26s bitflow send <path> --to %s%s\n", ifc.Label+" ("+ifc.Name+")", ip, suffix)
		}
	}
	fmt.Println("\nWaiting for a transfer… (Ctrl+C to stop)")
}

func describeOffer(o *proto.Offer, dest string) {
	files := 0
	var top []string
	for _, f := range o.Files {
		if !f.Dir {
			files++
		}
		if !strings.Contains(f.Path, "/") {
			name := f.Path
			if f.Dir {
				name += "/"
			}
			top = append(top, name)
		}
	}
	fmt.Printf("\n▶ Incoming from %s: %d files, %s\n", o.From, files, units.Bytes(o.Total))
	for i, name := range top {
		if i == 8 {
			fmt.Printf("    … and %d more\n", len(top)-i)
			break
		}
		fmt.Printf("    %s\n", name)
	}
	fmt.Printf("  → saving to %s\n", dest)
}

// runMeter redraws a progress line every 250 ms until the returned stop func is called.
func runMeter(total int64, current func() int64) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		m := newMeter(os.Stdout, total)
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				m.update(current())
			case <-done:
				m.update(current())
				m.finish()
				return
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}

// ---- send ----

func cmdSend(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	to := fs.String("to", "", "receiver address, HOST or HOST:PORT (shown when the receiver starts)")
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
	o := opts()

	m, err := planner.Build(paths)
	if err != nil {
		return err
	}
	for _, s := range m.Skipped {
		fmt.Printf("  skipping %s (not a regular file)\n", s)
	}
	fmt.Printf("Sending %d files, %s → %s over %d streams\n", m.FileCount(), units.Bytes(m.Total), addr, o.Streams)
	fmt.Println("Waiting for the receiver to accept…")

	s := transfer.NewSender(o)
	stop := watchSender(s)
	res, err := s.Send(ctx, addr, m)
	stop()
	if err != nil {
		return err
	}
	fmt.Printf("✓ Sent %d files: %s\n", res.Files, summary(res.Bytes, res.Elapsed))
	return nil
}

// watchSender shows a progress line once the receiver accepts.
func watchSender(s *transfer.Sender) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			if st := s.Stats(); st.Accepted {
				stopMeter := runMeter(st.Total, func() int64 { return s.Stats().Committed })
				<-done
				stopMeter()
				return
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}

// transferFlags registers the tuning flags shared by send and bench.
func transferFlags(fs *flag.FlagSet) func() transfer.Options {
	streams := fs.Int("streams", transfer.DefaultStreams, "parallel TCP streams")
	chunk := sizeFlag(transfer.DefaultChunkSize)
	block := sizeFlag(transfer.DefaultBlockSize)
	var sockBuf sizeFlag
	fs.Var(&chunk, "chunk", "work unit size (files are split into chunks)")
	fs.Var(&block, "block", "frame size on the wire")
	fs.Var(&sockBuf, "sockbuf", "socket buffer size (default: OS autotuning)")
	noHash := fs.Bool("nohash", false, "skip per-block checksums")
	return func() transfer.Options {
		return transfer.Options{
			Streams:   *streams,
			ChunkSize: int64(chunk),
			BlockSize: int(block),
			NoHash:    *noHash,
			SockBuf:   int(sockBuf),
		}
	}
}

// ---- bench ----

func cmdBench(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	to := fs.String("to", "", "benchmark against a running `bitflow recv` at HOST[:PORT]")
	loopback := fs.Bool("loopback", false, "benchmark against an in-process receiver on 127.0.0.1")
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

	addr := ""
	if *loopback {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		rctx, cancel := context.WithCancel(ctx)
		defer cancel()
		r := &transfer.Receiver{SockBuf: opts().SockBuf}
		go r.Serve(rctx, ln, false)
		addr = ln.Addr().String()
	} else {
		addr = withPort(*to)
	}

	base := opts()
	counts := []int{base.Streams}
	if *sweep {
		counts = []int{1, 2, 4, 8, 16, 32}
	}
	hash := "on"
	if base.NoHash {
		hash = "off"
	}
	fmt.Printf("Benchmark → %s: %s per run, block %s, checksums %s\n",
		addr, units.Bytes(int64(size)), units.Bytes(int64(base.BlockSize)), hash)

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

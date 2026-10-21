package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sshivamanand/bit-flow/internal/advisor"
	"github.com/sshivamanand/bit-flow/internal/netinfo"
	"github.com/sshivamanand/bit-flow/internal/secure"
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

// stdin is shared so prompts never lose buffered input between them.
var stdin = bufio.NewReader(os.Stdin)

func isTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// readCode returns the pairing code from the flag, or asks for it.
func readCode(flagVal string) (string, error) {
	if c := secure.NormalizeCode(flagVal); c != "" {
		return c, nil
	}
	if !isTerminal() {
		return "", errors.New("--code is required (the receiver shows it when it starts)")
	}
	fmt.Print("Pairing code shown on the receiver: ")
	line, _ := stdin.ReadString('\n')
	c := secure.NormalizeCode(line)
	if c == "" {
		return "", errors.New("no pairing code entered")
	}
	return c, nil
}

// defaultStateDir is where resume journals live.
func defaultStateDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "bitflow")
}

// runMeter redraws a progress line every 250 ms until the returned stop func
// is called. extra, if set, adds live details to the line.
func runMeter(total int64, current func() int64, extra func() string) (stop func()) {
	if extra == nil {
		extra = func() string { return "" }
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		m := newMeter(os.Stdout, total)
		m.lastB = current() // bytes already there don't count towards the rate
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				m.update(current(), extra())
			case <-done:
				m.update(current(), extra())
				m.finish()
				return
			}
		}
	}()
	return func() { close(done); wg.Wait() }
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
				printRoute(st)
				stopMeter := runMeter(st.Total, func() int64 { return s.Stats().Committed }, func() string {
					st := s.Stats()
					parts := []string{fmt.Sprintf("%d streams", st.Streams)}
					if st.Saved > 0 {
						parts = append(parts, "zstd saved "+units.Bytes(st.Saved))
					}
					return strings.Join(parts, " · ")
				})
				<-done
				stopMeter()
				return
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}

// printRoute shows which links a send uses and any advice for a faster setup.
func printRoute(st transfer.SendStats) {
	r := st.Route
	if r.LocalLabel != "" || r.PeerLabel != "" {
		fmt.Printf("  link: %s → %s\n", orIP(r.LocalLabel, r.LocalIP), orIP(r.PeerLabel, r.PeerIP))
	}
	links := []string{orIP(r.LocalLabel, r.LocalIP)}
	if len(st.Paths) > 1 {
		links = links[:0]
		for _, p := range st.Paths {
			links = append(links, p.Label)
		}
	}
	for _, tip := range advisor.Tips(advisor.Link{Label: r.LocalLabel, IP: r.LocalIP}, advisor.Link{Label: r.PeerLabel, IP: r.PeerIP}, links) {
		fmt.Printf("  tip: %s\n", tip)
	}
}

func orIP(label, ip string) string {
	if label != "" {
		return label
	}
	return ip
}

// transferFlags registers the tuning flags shared by send and bench.
func transferFlags(fs *flag.FlagSet) func() transfer.Options {
	streams := fs.Int("streams", 0, "parallel TCP streams (default: auto-tune, up to 16)")
	chunk := sizeFlag(transfer.DefaultChunkSize)
	block := sizeFlag(transfer.DefaultBlockSize)
	var sockBuf sizeFlag
	fs.Var(&chunk, "chunk", "work unit size (files are split into chunks)")
	fs.Var(&block, "block", "frame size on the wire")
	fs.Var(&sockBuf, "sockbuf", "socket buffer size (default: OS autotuning)")
	noHash := fs.Bool("nohash", false, "skip per-block checksums")
	raw := fs.Bool("raw", false, "unencrypted data streams for maximum speed (receiver needs --allow-raw)")
	compress := fs.String("compress", transfer.CompressAuto, "zstd compression: auto (on while it pays off), on or off")
	return func() transfer.Options {
		return transfer.Options{
			Streams:   *streams,
			ChunkSize: int64(chunk),
			BlockSize: int(block),
			NoHash:    *noHash,
			Raw:       *raw,
			Compress:  *compress,
			SockBuf:   int(sockBuf),
		}
	}
}

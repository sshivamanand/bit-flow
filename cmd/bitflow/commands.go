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

	"github.com/sshivamanand/bit-flow/internal/netinfo"
	"github.com/sshivamanand/bit-flow/internal/secure"
	"github.com/sshivamanand/bit-flow/internal/transfer"
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

// runMeter redraws a progress line every 250 ms until the returned stop func is called.
func runMeter(total int64, current func() int64) (stop func()) {
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
	raw := fs.Bool("raw", false, "unencrypted data streams for maximum speed (receiver needs --allow-raw)")
	return func() transfer.Options {
		return transfer.Options{
			Streams:   *streams,
			ChunkSize: int64(chunk),
			BlockSize: int(block),
			NoHash:    *noHash,
			Raw:       *raw,
			SockBuf:   int(sockBuf),
		}
	}
}

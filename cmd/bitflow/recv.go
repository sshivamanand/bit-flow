package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sshivamanand/bit-flow/internal/awake"
	"github.com/sshivamanand/bit-flow/internal/config"
	"github.com/sshivamanand/bit-flow/internal/discovery"
	"github.com/sshivamanand/bit-flow/internal/history"
	"github.com/sshivamanand/bit-flow/internal/netinfo"
	"github.com/sshivamanand/bit-flow/internal/proto"
	"github.com/sshivamanand/bit-flow/internal/secure"
	"github.com/sshivamanand/bit-flow/internal/store"
	"github.com/sshivamanand/bit-flow/internal/transfer"
	"github.com/sshivamanand/bit-flow/internal/units"
	"github.com/sshivamanand/bit-flow/internal/web"
)

func cmdRecv(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("recv", flag.ContinueOnError)
	to := fs.String("to", "", "folder to save received files into (default: the configured folder, else the current one)")
	from := fs.String("from", "", "fetch from a sender that is waiting (its NAME or HOST[:PORT])")
	port := fs.Int("port", proto.DefaultPort, "TCP/UDP port to listen on")
	yes := fs.Bool("yes", false, "accept transfers without asking")
	once := fs.Bool("once", false, "exit after one transfer")
	code := fs.String("code", "", "pairing code: a fixed one to use, or the sender's with --from")
	conflict := fs.String("on-conflict", "", "when a different file already exists: error, skip, rename, overwrite or update (default: the configured policy, else error)")
	overwrite := fs.Bool("overwrite", false, "shorthand for --on-conflict=overwrite")
	allowRaw := fs.Bool("allow-raw", false, "let senders use unencrypted data streams (trusted cables only)")
	noResume := fs.Bool("no-resume", false, "delete partial files of interrupted transfers instead of resuming")
	retries := fs.Int("retries", transfer.DefaultRetries, "with --from: reconnect attempts after a dropped connection")
	steamLib := fs.String("steam-library", "", "Steam library for incoming games (default: this computer's main one; \"-\" saves games as plain folders)")
	var sockBuf sizeFlag
	fs.Var(&sockBuf, "sockbuf", "socket buffer size (default: OS autotuning)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected argument %q (use --to DIR for the destination)", pos[0])
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if *conflict == "" {
		*conflict = cfg.Conflict
	}
	policy, err := store.ParsePolicy(*conflict)
	if err != nil {
		return err
	}
	if *overwrite {
		policy = store.PolicyOverwrite
	}
	if *to == "" {
		*to = "."
		if cfg.Dest != "" {
			*to = cfg.Dest
		}
	}

	dest, err := filepath.Abs(*to)
	if err != nil {
		return err
	}
	if st, err := os.Stat(dest); err != nil || !st.IsDir() {
		return fmt.Errorf("destination folder %s does not exist", dest)
	}

	r := &transfer.Receiver{
		Dest: dest, Policy: policy, AllowRaw: *allowRaw, SockBuf: int(sockBuf), Retries: *retries,
		SteamLibrary: *steamLib,
	}
	if !*noResume {
		r.StateDir = defaultStateDir()
	}
	r.Confirm = func(o *proto.Offer, info transfer.OfferInfo) transfer.Decision {
		if o.Bench {
			fmt.Printf("\n▶ Benchmark from %s: %s over %d streams (data is discarded)\n", o.From, units.Bytes(o.Total), o.Streams)
			return transfer.Decision{Accept: true}
		}
		describeOffer(o, info)
		if *yes {
			return transfer.Decision{Accept: true}
		}
		fmt.Print("  Accept? [Y/n] ")
		line, _ := stdin.ReadString('\n')
		ans := strings.ToLower(strings.TrimSpace(line))
		return transfer.Decision{Accept: ans == "" || ans == "y" || ans == "yes"}
	}
	var mu sync.Mutex
	var stopMeter, release func()
	r.OnStart = func(o *proto.Offer, _ transfer.OfferInfo) {
		mu.Lock()
		defer mu.Unlock()
		release = awake.Hold()
		stopMeter = runMeter(o.Total, func() int64 { return r.Stats().Received }, nil)
	}
	r.OnDone = func(o *proto.Offer, res transfer.Result, err error) {
		mu.Lock()
		if stopMeter != nil {
			stopMeter()
			stopMeter = nil
		}
		if release != nil {
			release()
			release = nil
		}
		mu.Unlock()
		if !o.Bench {
			name := topLevelName(o)
			files := 0
			for _, f := range o.Files {
				if !f.Dir {
					files++
				}
			}
			logTransfer("received", o.From, name, files, res, err)
		}
		switch {
		case transfer.IsDeclined(err):
			fmt.Println("✗ Declined.")
		case err != nil:
			fmt.Printf("✗ Transfer from %s stopped: %v\n", o.From, err)
			if r.StateDir != "" && !o.Bench {
				fmt.Println("  Partial files are kept; sending the same files again resumes where this stopped.")
			}
		case o.Bench:
			fmt.Printf("✓ Benchmark: %s\n", summary(res.Bytes, res.Elapsed))
		case o.Steam != nil:
			fmt.Printf("✓ Received %s: %s%s\n  Restart Steam and it shows as installed.\n", o.Steam.Name, summary(res.Bytes, res.Elapsed), alreadyNote(res.Already))
		default:
			fmt.Printf("✓ Received %d files: %s%s\n", res.Files, summary(res.Bytes, res.Elapsed), alreadyNote(res.Already))
		}
		if !*once && *from == "" {
			fmt.Printf("\nWaiting for the next transfer… pairing code %s (Ctrl+C to stop)\n", secure.FormatCode(r.CurrentCode()))
		}
	}

	if *from != "" {
		return fetch(ctx, r, *from, *code, cfg)
	}

	if *code != "" {
		r.Code = secure.NormalizeCode(*code)
		if len(r.Code) < 6 {
			return fmt.Errorf("--code needs at least 6 digits")
		}
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		return err
	}
	// One port for everything: bit-flow peers, and browsers (phones, /get).
	peers, browsers := web.Split(ln)
	// One history entry per uploaded folder (or run of files), not one per file.
	var uploads history.Batches
	defer uploads.Flush()
	go serveHTTP(ctx, browsers, web.NewServer(web.Options{
		Name: cfg.DeviceName, CheckCode: r.CheckCode, Binaries: web.FindBinaries,
		UploadDir: func() string { return dest },
		OnUpload: func(rel string, size int64, from string) {
			fmt.Printf("\n✓ %s sent %s (%s)\n", from, rel, units.Bytes(size))
			uploads.Add(rel, size, from, time.Now())
		},
	}))
	info := selfInfo(cfg, *port, discovery.RoleReceive, "")
	r.Probe = func() any { return info() }
	go discovery.Respond(ctx, proto.DefaultPort, info) // scanners always ask on the default port; info carries the real one
	r.OnCodeChange = func(c string) {
		fmt.Printf("\n⚠ Several wrong pairing codes were tried. New pairing code: %s\n", secure.FormatCode(c))
	}
	printRecvBanner(dest, *port, r.CurrentCode(), cfg.DeviceName())
	return r.Serve(ctx, peers, *once)
}

// fetch receives from a sender that is waiting to be fetched.
func fetch(ctx context.Context, r *transfer.Receiver, from, code string, cfg *config.Config) error {
	addr, err := resolve(ctx, from, discovery.RoleSend, cfg)
	if err != nil {
		return err
	}
	if code, err = readCode(code); err != nil {
		return err
	}
	r.OnRetry = func(attempt int, err error, wait time.Duration) {
		fmt.Printf("\n⟳ %v\n  reconnecting in %s (attempt %d)…\n", err, wait, attempt)
	}
	fmt.Printf("Fetching from %s…\n", addr)
	_, err = r.Fetch(ctx, addr, code)
	return err
}

// topLevelName summarizes an offer for the history log.
func topLevelName(o *proto.Offer) string {
	if o.Steam != nil {
		return o.Steam.Name + " (steam)"
	}
	var top []string
	for _, f := range o.Files {
		if !strings.Contains(f.Path, "/") {
			top = append(top, f.Path)
		}
	}
	switch len(top) {
	case 0:
		return "(nothing)"
	case 1:
		return top[0]
	}
	return fmt.Sprintf("%s and %d more", top[0], len(top)-1)
}

func osName(goos string) string {
	switch goos {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	}
	return goos
}

func alreadyNote(already int64) string {
	if already == 0 {
		return ""
	}
	return fmt.Sprintf("  (%s was already here)", units.Bytes(already))
}

func printRecvBanner(dest string, port int, code, name string) {
	fmt.Printf("bit-flow %s — receiving into %s\n\n", version, dest)
	fmt.Printf("  Pairing code:  %s\n", secure.FormatCode(code))
	fmt.Printf("  Device name:   %s\n\n", name)
	suffix := ""
	if port != proto.DefaultPort {
		suffix = fmt.Sprintf(":%d", port)
	}
	fmt.Println("On the sending computer, run:")
	if suffix == "" && !strings.ContainsAny(name, " .:") {
		fmt.Printf("  bitflow send <path> --to %s --code %s\n", name, code)
	} else {
		fmt.Printf("  bitflow send <path> --code %s\n", code)
	}
	if ifs, _ := netinfo.List(); len(ifs) > 0 {
		fmt.Println("or by address, if it can't find this device (pick the fastest link):")
		for _, ifc := range ifs {
			for _, ip := range ifc.Addrs {
				fmt.Printf("  %-26s bitflow send <path> --to %s%s --code %s\n", ifc.Label+" ("+ifc.Name+")", ip, suffix, code)
			}
		}
	}
	printWebAccess(webURL(port), "Phone, or a computer without bit-flow?")
	fmt.Println("\nWaiting for a transfer… (Ctrl+C to stop)")
}

func describeOffer(o *proto.Offer, info transfer.OfferInfo) {
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
	if o.Steam != nil {
		fmt.Printf("\n▶ %s wants to send the Steam game %s: %d files, %s\n", o.From, o.Steam.Name, files, units.Bytes(o.Total))
		if info.SteamLibrary != "" {
			fmt.Printf("  → installing into your Steam library %s (restart Steam afterwards)\n", info.SteamLibrary)
		} else {
			fmt.Printf("  → no Steam library here, so it's saved as a folder in %s\n", info.Dest)
		}
		if o.Steam.OS != runtime.GOOS {
			fmt.Printf("  ⚠ this is their %s copy; Steam may need to download the %s version\n", osName(o.Steam.OS), osName(runtime.GOOS))
		}
		if info.Already > 0 {
			fmt.Printf("  = %s already here; only the differences are sent\n", units.Bytes(info.Already))
		}
		return
	}
	fmt.Printf("\n▶ Incoming from %s: %d files, %s\n", o.From, files, units.Bytes(o.Total))
	for i, name := range top {
		if i == 8 {
			fmt.Printf("    … and %d more\n", len(top)-i)
			break
		}
		fmt.Printf("    %s\n", name)
	}
	fmt.Printf("  → saving to %s\n", info.Dest)
	if len(info.Renames) > 0 {
		names := make([]string, 0, len(info.Renames))
		for from := range info.Renames {
			names = append(names, from)
		}
		sort.Strings(names)
		for _, from := range names {
			fmt.Printf("    %s already exists → saving as %s\n", from, info.Renames[from])
		}
	}
	switch {
	case info.Resumed:
		fmt.Printf("  ↻ resuming: %s already received\n", units.Bytes(info.Already))
	case info.Already > 0:
		fmt.Printf("  = %d identical files already here (%s), they will be skipped\n", info.Skipped, units.Bytes(info.Already))
	}
	if info.Raw {
		fmt.Println("  ⚠ data streams will NOT be encrypted (--raw)")
	}
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sshivamanand/bit-flow/internal/config"
	"github.com/sshivamanand/bit-flow/internal/history"
	"github.com/sshivamanand/bit-flow/internal/store"
	"github.com/sshivamanand/bit-flow/internal/transfer"
	"github.com/sshivamanand/bit-flow/internal/units"
)

func cmdHistory(args []string) error {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	n := fs.Int("n", 20, "how many transfers to show")
	clear := fs.Bool("clear", false, "delete the whole transfer history (with the app open, use its \"clear history\" instead)")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	if *clear {
		if err := history.Clear(); err != nil {
			return err
		}
		if dir, err := config.Dir(); err == nil {
			_ = os.Remove(filepath.Join(dir, "history.json")) // the app's saved list
		}
		fmt.Println("✓ History deleted.")
		return nil
	}
	rs, err := history.Load(*n)
	if err != nil {
		return err
	}
	if len(rs) == 0 {
		fmt.Println("No transfers yet.")
		return nil
	}
	for _, r := range rs {
		arrow, prep := "↑", "to"
		if r.Dir == "received" {
			arrow, prep = "↓", "from"
		}
		speed := ""
		if r.State == "done" && r.Rate() > 0 {
			speed = units.Rate(r.Rate())
		}
		state := r.State
		if r.Error != "" && r.State != "done" {
			state += ": " + r.Error
		}
		fmt.Printf("  %s  %s %-28s %s %-18s %10s  %-12s %s\n", r.Time.Local().Format("Jan 02 15:04"), arrow,
			trunc(r.Name, 28), prep, trunc(r.Peer, 18), units.Bytes(r.Bytes+r.Already), speed, trunc(state, 60))
	}
	return nil
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// logTransfer records a finished transfer; a failure to log never fails the transfer.
func logTransfer(dir, peer, name string, files int, res transfer.Result, err error) {
	r := history.Record{Time: time.Now(), Dir: dir, Peer: peer, Name: name, Files: files,
		Bytes: res.Bytes, Already: res.Already, Seconds: res.Elapsed.Seconds(), State: "done"}
	switch {
	case err == nil:
	case transfer.IsDeclined(err):
		r.State = "declined"
	case errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "cancelled"):
		r.State, r.Error = "cancelled", err.Error()
	default:
		r.State, r.Error = "failed", err.Error()
	}
	_ = history.Append(r)
}

func cmdConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	dir, _ := config.Dir()
	switch len(pos) {
	case 0:
		fmt.Printf("  name         %s\n", cfg.DeviceName())
		fmt.Printf("  dest         %s\n", cfg.ReceiveDir())
		fmt.Printf("  on_conflict  %s\n", orDefault(cfg.Conflict, "error"))
		fmt.Printf("  id           %s\n", cfg.ID)
		fmt.Printf("\n  stored in %s\n", filepath.Join(dir, "config.json"))
		fmt.Println("  change with: bitflow config name|dest|on_conflict VALUE")
		return nil
	case 2:
	default:
		return errors.New("usage: bitflow config [name|dest|on_conflict VALUE]")
	}
	key, val := pos[0], pos[1]
	switch key {
	case "name":
		if v := strings.TrimSpace(val); v == "" || len(v) > 40 {
			return errors.New("pick a name between 1 and 40 characters")
		}
		cfg.Name = strings.TrimSpace(val)
	case "dest":
		abs, err := filepath.Abs(val)
		if err != nil {
			return err
		}
		if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			return fmt.Errorf("%s is not a folder", abs)
		}
		cfg.Dest = abs
	case "on_conflict", "conflict":
		p, err := store.ParsePolicy(val)
		if err != nil {
			return err
		}
		cfg.Conflict = p.String()
	default:
		return fmt.Errorf("unknown setting %q (name, dest, on_conflict)", key)
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("✓ %s = %s\n", key, val)
	return nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

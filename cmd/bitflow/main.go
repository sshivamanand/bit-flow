// Command bitflow sends files between computers on the same network, fast.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

var version = "0.1.0-dev"

const usageText = `bit-flow — fast local file transfer

Usage:
  bitflow                                        open the app (web interface)
  bitflow recv  [--to DIR] [--yes] [--on-conflict error|skip|rename|overwrite] [--allow-raw]
  bitflow recv  --from NAME|HOST --to DIR        fetch from a sender that is waiting
  bitflow send  PATH... [--to NAME|HOST] [--code CODE] [--streams 8] [--raw]
  bitflow send  PATH... --wait                   wait for a receiver to fetch
  bitflow send  --steam GAME [--to NAME|HOST]     send an installed Steam game into their library
  bitflow games                                  list installed Steam games
  bitflow peers                                  list bit-flow devices on this network
  bitflow bench (--loopback | --to NAME|HOST --code CODE) [--size 10GB] [--sweep] [--raw]
  bitflow ifaces
  bitflow version

Whoever waits shows a pairing code; the other side needs it to connect.
Run "bitflow <command> -h" for a command's flags.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// No command (e.g. double-clicked): open the app.
	cmd, args := "ui", []string{}
	if len(os.Args) > 1 {
		cmd, args = os.Args[1], os.Args[2:]
	}
	var err error
	switch cmd {
	case "ui", "app":
		err = cmdUI(ctx, args)
	case "recv", "receive":
		err = cmdRecv(ctx, args)
	case "send":
		err = cmdSend(ctx, args)
	case "bench":
		err = cmdBench(ctx, args)
	case "games":
		err = cmdGames(args)
	case "peers", "scan":
		err = cmdPeers(ctx, args)
	case "ifaces", "interfaces":
		err = cmdIfaces()
	case "version", "--version":
		fmt.Println("bitflow", version)
	case "help", "-h", "--help":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usageText)
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "\ncancelled")
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "\nerror:", err)
		os.Exit(1)
	}
}

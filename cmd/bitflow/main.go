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
  bitflow recv  [--to DIR] [--yes] [--once] [--on-conflict error|skip|rename|overwrite] [--allow-raw]
  bitflow send  PATH... --to HOST[:PORT] [--code CODE] [--streams 8] [--raw]
  bitflow bench (--loopback | --to HOST[:PORT] --code CODE) [--size 10GB] [--sweep] [--raw]
  bitflow ifaces
  bitflow version

The receiver prints a pairing code; the sender needs it to connect.
Run "bitflow <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "recv", "receive":
		err = cmdRecv(ctx, args)
	case "send":
		err = cmdSend(ctx, args)
	case "bench":
		err = cmdBench(ctx, args)
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

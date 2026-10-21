package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/sshivamanand/bit-flow/internal/proto"
	"github.com/sshivamanand/bit-flow/internal/ui"
)

func cmdUI(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	port := fs.Int("port", proto.DefaultPort, "TCP port for transfers (another free port is used if taken)")
	uiAddr := fs.String("listen", "127.0.0.1:7710", "address for the web interface (keep it on 127.0.0.1)")
	noBrowser := fs.Bool("no-browser", false, "don't open the browser")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	app, err := ui.NewApp(version)
	if err != nil {
		return err
	}
	if err := app.Start(ctx, *port); err != nil {
		return err
	}
	url, err := ui.Serve(ctx, app, *uiAddr)
	if err != nil {
		return err
	}
	fmt.Printf("bit-flow is running. Open %s\n(this browser stays signed in afterwards; Ctrl+C to quit)\n", url)
	if !*noBrowser {
		if err := ui.OpenBrowser(url); err != nil {
			fmt.Println("Couldn't open a browser:", err)
		}
	}
	<-ctx.Done()
	return nil
}

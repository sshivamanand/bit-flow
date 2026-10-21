package main

import (
	"errors"
	"flag"
	"fmt"
	"runtime"
	"strings"

	"github.com/sshivamanand/bit-flow/internal/planner"
	"github.com/sshivamanand/bit-flow/internal/proto"
	"github.com/sshivamanand/bit-flow/internal/steam"
	"github.com/sshivamanand/bit-flow/internal/units"
)

func cmdGames(args []string) error {
	fs := flag.NewFlagSet("games", flag.ContinueOnError)
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	games := steam.Games()
	if len(games) == 0 {
		fmt.Println("No installed Steam games found.")
		return nil
	}
	for _, g := range games {
		fmt.Printf("  %-36s %9s   appid %-8s %s\n", g.Name, units.Bytes(g.Size), g.AppID, g.Library)
	}
	fmt.Println("\nSend one with: bitflow send --steam \"<name or appid>\" [--to NAME]")
	return nil
}

// steamManifest builds the manifest for `send --steam QUERY`.
func steamManifest(query string) (*planner.Manifest, error) {
	games, err := steam.Find(query)
	if err != nil {
		return nil, err
	}
	if len(games) > 1 {
		var names []string
		for _, g := range games {
			names = append(names, fmt.Sprintf("%s (%s)", g.Name, g.AppID))
		}
		return nil, errors.New("several games match: " + strings.Join(names, ", ") + "; use the appid")
	}
	g := games[0]
	fmt.Printf("Steam game: %s (appid %s, %s)\n", g.Name, g.AppID, units.Bytes(g.Size))
	return planner.BuildSteam(proto.SteamGame{AppID: g.AppID, Name: g.Name, InstallDir: g.InstallDir, OS: runtime.GOOS}, g.Dir, g.Manifest)
}

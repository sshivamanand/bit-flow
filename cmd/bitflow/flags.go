package main

import (
	"flag"
	"fmt"
	"net"
	"strconv"

	"github.com/sshivamanand/bit-flow/internal/proto"
	"github.com/sshivamanand/bit-flow/internal/units"
)

// sizeFlag accepts human sizes like 4MiB or 10GB.
type sizeFlag int64

// String shows binary sizes exactly (4MiB, not "4.2 MB").
func (s *sizeFlag) String() string {
	n := int64(*s)
	switch {
	case n == 0:
		return "0"
	case n%(1<<30) == 0:
		return fmt.Sprintf("%dGiB", n>>30)
	case n%(1<<20) == 0:
		return fmt.Sprintf("%dMiB", n>>20)
	case n%(1<<10) == 0:
		return fmt.Sprintf("%dKiB", n>>10)
	}
	return units.Bytes(n)
}

func (s *sizeFlag) Set(v string) error {
	n, err := units.ParseSize(v)
	if err != nil {
		return err
	}
	*s = sizeFlag(n)
	return nil
}

// parseArgs lets flags appear before or after positional arguments
// ("bitflow send ./Game --to 10.0.0.5").
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// withPort adds the default port when addr has none.
func withPort(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(addr, strconv.Itoa(proto.DefaultPort))
}

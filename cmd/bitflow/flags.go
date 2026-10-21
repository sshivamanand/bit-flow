package main

import (
	"flag"
	"net"
	"strconv"

	"github.com/sshivamanand/bit-flow/internal/proto"
	"github.com/sshivamanand/bit-flow/internal/units"
)

// sizeFlag accepts human sizes like 4MiB or 10GB.
type sizeFlag int64

func (s *sizeFlag) String() string { return units.Bytes(int64(*s)) }

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

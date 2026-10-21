package store

import (
	"os"

	"golang.org/x/sys/unix"
)

// preallocate reserves size bytes for f.
func preallocate(f *os.File, size int64) error {
	return unix.Fallocate(int(f.Fd()), 0, 0, size)
}

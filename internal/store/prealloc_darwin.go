package store

import (
	"os"

	"golang.org/x/sys/unix"
)

// preallocate reserves size bytes for f, contiguously if the filesystem can.
func preallocate(f *os.File, size int64) error {
	fst := unix.Fstore_t{Flags: unix.F_ALLOCATECONTIG | unix.F_ALLOCATEALL, Posmode: unix.F_PEOFPOSMODE, Length: size}
	if err := unix.FcntlFstore(f.Fd(), unix.F_PREALLOCATE, &fst); err == nil {
		return nil
	}
	fst.Flags = unix.F_ALLOCATEALL
	return unix.FcntlFstore(f.Fd(), unix.F_PREALLOCATE, &fst)
}

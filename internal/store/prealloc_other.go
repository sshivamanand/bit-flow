//go:build !darwin && !linux

package store

import "os"

// preallocate is a no-op here; Truncate sizes the file. On Windows, NTFS then
// zero-fills gaps written out of order, which is why chunks are scheduled with
// consecutive offsets per file.
func preallocate(*os.File, int64) error { return nil }

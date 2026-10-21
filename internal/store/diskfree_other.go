//go:build !darwin && !linux && !freebsd && !windows

package store

// diskFree is unknown on this platform; the check is skipped.
func diskFree(string) (int64, error) { return -1, nil }

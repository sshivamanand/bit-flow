//go:build !darwin && !linux && !windows

package awake

// Hold is a no-op on this platform.
func Hold() (release func()) { return func() {} }

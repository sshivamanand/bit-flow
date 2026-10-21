//go:build !darwin && !linux && !freebsd && !windows

package discovery

import "syscall"

func reuseControl(string, string, syscall.RawConn) error { return nil }

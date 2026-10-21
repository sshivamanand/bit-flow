//go:build !darwin && !linux && !windows

package ui

func pick(string) ([]string, error) { return nil, errUnsupported }
func openURL(string) error          { return errUnsupported }
func reveal(string) error           { return errUnsupported }

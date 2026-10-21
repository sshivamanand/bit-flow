package ui

import (
	"errors"
	"strings"
)

// Dialog kinds.
const (
	PickFiles  = "files"
	PickFolder = "folder"
	PickDest   = "dest"
)

var (
	errCanceled    = errors.New("canceled")
	errUnsupported = errors.New("no native file dialog available; type the path instead")
)

// splitPaths parses one path per line, dropping blanks and trailing separators.
func splitPaths(out string) []string {
	var paths []string
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if len(line) > 1 {
			line = strings.TrimRight(line, `/\`)
		}
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths
}

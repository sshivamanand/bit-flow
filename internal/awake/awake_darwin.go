package awake

import (
	"os"
	"os/exec"
	"strconv"
)

// Hold prevents idle sleep until release is called (or this process exits).
func Hold() (release func()) {
	// -i: no idle sleep; -w: also stop when this process dies.
	cmd := exec.Command("caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		return func() {}
	}
	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

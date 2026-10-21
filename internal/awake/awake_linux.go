package awake

import (
	"os/exec"
)

// Hold prevents idle sleep via systemd-inhibit, when available, until release is called.
func Hold() (release func()) {
	path, err := exec.LookPath("systemd-inhibit")
	if err != nil {
		return func() {}
	}
	cmd := exec.Command(path, "--what=idle:sleep", "--who=bit-flow", "--why=Transferring files", "--mode=block", "sleep", "infinity")
	if err := cmd.Start(); err != nil {
		return func() {}
	}
	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

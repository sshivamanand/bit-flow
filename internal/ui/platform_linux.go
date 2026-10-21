package ui

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

// pick uses zenity or kdialog, whichever is installed.
func pick(kind string) ([]string, error) {
	var cmd *exec.Cmd
	if p, err := exec.LookPath("zenity"); err == nil {
		args := []string{"--file-selection", "--separator=\n"}
		switch kind {
		case PickFiles:
			args = append(args, "--multiple", "--title=Choose files to send")
		case PickFolder:
			args = append(args, "--multiple", "--directory", "--title=Choose folders to send")
		default:
			args = append(args, "--directory", "--title=Save received files in")
		}
		cmd = exec.Command(p, args...)
	} else if p, err := exec.LookPath("kdialog"); err == nil {
		home, _ := os.UserHomeDir()
		if kind == PickFiles {
			cmd = exec.Command(p, "--getopenfilename", home, "--multiple", "--separate-output")
		} else {
			cmd = exec.Command(p, "--getexistingdirectory", home)
		}
	} else {
		return nil, errUnsupported
	}
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return nil, errCanceled
	}
	if err != nil {
		return nil, err
	}
	paths := splitPaths(string(out))
	if len(paths) == 0 {
		return nil, errCanceled
	}
	return paths, nil
}

func openURL(url string) error { return exec.Command("xdg-open", url).Start() }

// reveal opens the folder containing path.
func reveal(path string) error {
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		path = filepath.Dir(path)
	}
	return exec.Command("xdg-open", path).Start()
}

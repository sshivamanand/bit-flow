package ui

import (
	"os/exec"
	"strings"
)

// pick shows a native macOS chooser. Running it through System Events brings
// the dialog in front of the browser.
func pick(kind string) ([]string, error) {
	choose := map[string]string{
		PickFiles:  `choose file with prompt "Choose files to send" with multiple selections allowed`,
		PickFolder: `choose folder with prompt "Choose folders to send" with multiple selections allowed`,
		PickDest:   `choose folder with prompt "Save received files in"`,
	}[kind]
	script := `tell application "System Events"
	activate
	set picked to (` + choose + `)
end tell
set out to ""
if class of picked is list then
	repeat with f in picked
		set out to out & POSIX path of f & linefeed
	end repeat
else
	set out to POSIX path of picked
end if
return out`
	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && strings.Contains(string(ee.Stderr), "-128") {
			return nil, errCanceled
		}
		return nil, err
	}
	paths := splitPaths(string(out))
	if len(paths) == 0 {
		return nil, errCanceled
	}
	return paths, nil
}

func openURL(url string) error { return exec.Command("open", url).Start() }

// reveal shows path selected in Finder.
func reveal(path string) error { return exec.Command("open", "-R", path).Start() }

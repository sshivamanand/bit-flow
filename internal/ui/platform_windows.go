package ui

import (
	"os/exec"
	"syscall"
)

// pick shows a WinForms chooser via PowerShell. A hidden top-most owner form
// keeps the dialog in front of the browser.
func pick(kind string) ([]string, error) {
	var body string
	switch kind {
	case PickFiles:
		body = `$d = New-Object System.Windows.Forms.OpenFileDialog
$d.Multiselect = $true
$d.Title = 'Choose files to send'
if ($d.ShowDialog($owner) -eq 'OK') { $d.FileNames -join "` + "`n" + `" }`
	default:
		desc := "Choose a folder to send"
		if kind == PickDest {
			desc = "Save received files in"
		}
		body = `$d = New-Object System.Windows.Forms.FolderBrowserDialog
$d.Description = '` + desc + `'
if ($d.ShowDialog($owner) -eq 'OK') { $d.SelectedPath }`
	}
	script := `Add-Type -AssemblyName System.Windows.Forms
$owner = New-Object System.Windows.Forms.Form -Property @{TopMost=$true; ShowInTaskbar=$false}
` + body
	cmd := exec.Command("powershell", "-NoProfile", "-STA", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	paths := splitPaths(string(out))
	if len(paths) == 0 {
		return nil, errCanceled
	}
	return paths, nil
}

func openURL(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// reveal shows path selected in Explorer.
func reveal(path string) error { return exec.Command("explorer", "/select,", path).Start() }

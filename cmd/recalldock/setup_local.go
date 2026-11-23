package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// The reviewed installer is shipped beside the immutable runtime. No upstream
// installer, package manager or knowledge store is opened by this command.
func runLocalSetup(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		script := filepath.Join(filepath.Dir(filepath.Dir(exe)), "tools", "install-windows.ps1")
		info, err := os.Lstat(script)
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("setup requires a packaged RecallDock Windows release; from source use tools\\install-windows.ps1 -Help. No upstream plugin will be installed")
		}
		ps, err := exec.LookPath("powershell.exe")
		if err != nil {
			return errors.New("Windows setup requires PowerShell (powershell.exe); the memory runtime does not require PowerShell")
		}
		cmd := exec.Command(ps, append([]string{"-NoProfile", "-File", script}, args...)...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("local Windows install did not complete: %w", err)
		}
		return nil
	}
	if runtime.GOOS != "linux" {
		return errors.New("local release installation currently supports Linux and Windows only")
	}
	script := filepath.Join(filepath.Dir(filepath.Dir(exe)), "tools", "install-local.py")
	info, err := os.Lstat(script)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("setup requires a packaged RecallDock release; from source use python3 tools/install-local.py --help. No upstream plugin will be installed")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		return errors.New("setup requires Python 3.11+; the memory runtime does not require Python")
	}
	cmd := exec.Command(python, append([]string{script}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("local install did not complete: %w", err)
	}
	return nil
}

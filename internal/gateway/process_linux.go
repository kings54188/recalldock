//go:build linux

package gateway

import (
	"errors"
	"os/exec"
	"syscall"
)

// configureProcess isolates the backend in its own process group so shutdown
// never signals the host or unrelated processes.
func configureProcess(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

func signalGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return errors.New("no owned backend process")
	}
	err := syscall.Kill(-cmd.Process.Pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func terminateProcess(cmd *exec.Cmd) { _ = signalGroup(cmd, syscall.SIGTERM) }
func killProcess(cmd *exec.Cmd)      { _ = signalGroup(cmd, syscall.SIGKILL) }

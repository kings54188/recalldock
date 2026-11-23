//go:build !linux && !windows

package gateway

import (
	"errors"
	"os/exec"
)

func configureProcess(*exec.Cmd) error {
	return errors.New("gateway backend process control currently requires Linux; no unsafe fallback")
}

func terminateProcess(*exec.Cmd) {}
func killProcess(*exec.Cmd)      {}

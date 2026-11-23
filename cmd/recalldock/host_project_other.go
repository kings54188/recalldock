//go:build !linux && !windows

package main

import (
	"errors"
	"recalldock.local/recalldock/internal/project"
	"recalldock.local/recalldock/internal/store"
)

func ensureHostProject(_ *store.Store, _, _ string) (project.DetectionResult, error) {
	return project.DetectionResult{}, errors.New("automatic host projects are supported only on Linux")
}

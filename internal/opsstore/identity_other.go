//go:build !linux && !windows

package opsstore

import "errors"

func directoryIdentity(string) (string, error) {
	return "", errors.New("verified operations workspace identity currently requires Linux; no path-only fallback")
}

func expandOpsShortPath(path string) string {
	return normalizeOpsPath(path)
}

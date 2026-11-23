//go:build !linux && !windows

package store

import "errors"

func directoryIdentity(root string) (string, string, error) {
	return "", "", errors.New("verified directory bindings are currently supported on Linux only; no unsafe path-only fallback")
}

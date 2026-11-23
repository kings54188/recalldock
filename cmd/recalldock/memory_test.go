package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMemoryInvalidInputsDoNotOpenStore(t *testing.T) {
	for _, args := range [][]string{{"show"}, {"show", "bad"}, {"history", "1", "--limit", "21"}, {"version", "1", "0"}, {"update", "1"}, {"history", "1", "--full"}, {"show", "1", "--reason", "unexpected"}} {
		t.Run(args[0], func(t *testing.T) {
			stubExitWithPanic(t)
			cfg := testConfig(t)
			cfg.DataDir = filepath.Join(cfg.DataDir, "absent")
			withArgs(t, append([]string{"recalldock", "memory"}, args...)...)
			_, stderr, p := captureOutputAndRecover(t, func() { cmdMemory(cfg) })
			if p == nil || stderr == "" {
				t.Fatal("invalid command accepted")
			}
			if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
				t.Fatal("invalid memory command opened store")
			}
		})
	}
}

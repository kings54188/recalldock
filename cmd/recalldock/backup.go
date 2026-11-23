package main

import (
	"fmt"
	"os"
	"path/filepath"
	"recalldock.local/recalldock/internal/store"
)

func cmdBackup(cfg store.Config) {
	jsonOutput := false
	for _, arg := range os.Args[2:] {
		if arg != "--json" || jsonOutput {
			fatal(fmt.Errorf("usage: recalldock backup [--json]"))
			return
		}
		jsonOutput = true
	}
	// A backup of a mistyped/empty directory must not silently initialize a store.
	path := filepath.Join(cfg.DataDir, "recalldock.db")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		fatal(fmt.Errorf("backup requires an existing regular recalldock.db in the selected data directory"))
		return
	}
	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()
	backup, err := s.CreateSQLiteBackup()
	if err != nil {
		fatal(err)
		return
	}
	if jsonOutput {
		body, err := jsonMarshalIndent(backup, "", "  ")
		if err != nil {
			fatal(err)
			return
		}
		fmt.Println(string(body))
		return
	}
	fmt.Printf("Online backup verified: %s\nStore: %s\nBytes: %d\nSHA256: %s\n", backup.Path, backup.StoreID, backup.Bytes, backup.SHA256)
	fmt.Println("Contains private data. No automatic restore or deletion was performed.")
}

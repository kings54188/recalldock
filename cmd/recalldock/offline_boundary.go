package main

import (
	"errors"
	"recalldock.local/recalldock/internal/server"
	"strings"
)

// One production entry boundary. Archived cloud/runner implementations remain
// internally testable, but no runtime opt-in or model argument can enable them.
func localOnlyCommandError(args []string) error {
	if len(args) == 0 {
		return nil
	}
	if args[0] == "cloud" {
		return errors.New("RecallDock local-only build: cloud commands are disabled; use local JSON/Markdown interchange")
	}
	for _, arg := range args[1:] {
		if (args[0] == "sync" && (arg == "--cloud" || strings.HasPrefix(arg, "--cloud="))) || (args[0] == "conflicts" && (arg == "--semantic" || strings.HasPrefix(arg, "--semantic="))) {
			return errors.New("RecallDock local-only build: cloud transport and external semantic runners are disabled")
		}
	}
	return nil
}

type localOnlySyncStatus struct{}

func (localOnlySyncStatus) Status(string) server.SyncStatus {
	return server.SyncStatus{Enabled: false, Phase: "disabled", ReasonCode: "local_only", ReasonMessage: "Local-only build; no cloud configuration, polling or upload"}
}

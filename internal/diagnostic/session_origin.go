package diagnostic

import (
	"context"
	"os"
	"strings"

	"recalldock.local/recalldock/internal/store"
)

const (
	CheckSessionOrigin   = "session_origin"
	CheckNetworkBoundary = "network_boundary"
)

// SessionOriginCheck reports which transport registered each session. This is
// provenance, not authentication: a hook, an operator CLI and a model tool are
// different trust surfaces, but none of the labels proves the caller's identity
// to another same-UID process.
type SessionOriginCheck struct{}

func (SessionOriginCheck) Code() string { return CheckSessionOrigin }

func (c SessionOriginCheck) Run(ctx context.Context, scope Scope) (CheckResult, error) {
	_ = ctx
	stats, err := scope.Store.SessionOriginStats(scope.Project)
	if err != nil {
		return CheckResult{}, err
	}
	total := stats[store.SessionOriginHost] + stats[store.SessionOriginOperator] + stats[store.SessionOriginModel] + stats[store.SessionOriginUnknown]
	evidence := map[string]any{
		"total":    total,
		"host":     stats[store.SessionOriginHost],
		"operator": stats[store.SessionOriginOperator],
		"model":    stats[store.SessionOriginModel],
		"unknown":  stats[store.SessionOriginUnknown],
		"note":     "origin is transport provenance, not host handshake authentication",
	}
	// Provenance is informational: it never blocks and must not page the
	// operator. Unknown rows are legacy and cannot be attributed retroactively.
	return okResult(c.Code(), evidence), nil
}


// NetworkBoundaryCheck reports the outbound surfaces that the local-only build
// keeps disabled. It never dials: it only inspects configuration presence.
type NetworkBoundaryCheck struct{}

func (NetworkBoundaryCheck) Code() string { return CheckNetworkBoundary }

func (c NetworkBoundaryCheck) Run(ctx context.Context, scope Scope) (CheckResult, error) {
	_ = ctx
	_ = scope
	cloudEnv := []string{}
	for _, key := range []string{
		"RECALLDOCK_CLOUD_URL", "RECALLDOCK_CLOUD_TOKEN", "ENGRAM_CLOUD_URL", "ENGRAM_CLOUD_TOKEN",
	} {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			cloudEnv = append(cloudEnv, key)
		}
	}
	releaseEnv := strings.TrimSpace(os.Getenv("RECALLDOCK_RELEASE_API_URL")) != ""
	evidence := map[string]any{
		"cloud_commands":       "disabled",
		"sync_cloud_flag":      "disabled",
		"semantic_runner_flag": "disabled",
		"update_check":         map[bool]string{true: "endpoint_env_ignored_by_local_build", false: "disabled"}[releaseEnv],
		"cloud_env_present":    cloudEnv,
		"note":                 "application boundary, not an OS network sandbox; the gateway may connect to operator-registered backends",
	}
	// Presence of cloud env vars does not enable egress in this build; report it
	// as evidence so an operator can clean stale shells without alarm noise.
	return okResult(c.Code(), evidence), nil
}


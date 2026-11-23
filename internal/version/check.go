// Package version checks for newer recalldock releases on GitHub.
package version

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

var (
	checkTimeout           = 2 * time.Second
	githubLatestReleaseURL = "" // No upstream release endpoint for independent builds.
	httpClient             = http.DefaultClient
)

type CheckStatus string

const (
	StatusUpToDate        CheckStatus = "up_to_date"
	StatusUpdateAvailable CheckStatus = "update_available"
	StatusCheckFailed     CheckStatus = "check_failed"
)

type CheckResult struct {
	Status  CheckStatus
	Message string
}

// githubRelease is the subset of the GitHub releases API we care about.
type githubRelease struct {
	TagName string `json:"tag_name"`
}

// CheckLatest compares the running version against the latest GitHub release.
// It distinguishes between up-to-date, update available, and check failures.
func CheckLatest(current string) CheckResult {
	if !ReleaseChecksConfigured() || updateCheckDisabled() {
		return CheckResult{Status: StatusUpToDate}
	}
	switch current {
	case "":
		return checkFailed("Could not check for updates: current version is unknown.")
	case "dev":
		return checkFailed("Could not check for updates: development builds do not map to a release version.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()

	endpoint := strings.TrimSpace(os.Getenv("RECALLDOCK_RELEASE_API_URL"))
	if endpoint == "" {
		endpoint = githubLatestReleaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return checkFailed("Could not check for updates: could not create the GitHub request.")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token := githubToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return checkFailed("Could not check for updates: GitHub took too long to respond.")
		}
		return checkFailed(fmt.Sprintf("Could not check for updates: %v.", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return checkFailed(nonOKStatusMessage(resp.Status))
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return checkFailed("Could not check for updates: could not read the GitHub response.")
	}

	latest := normalizeVersion(release.TagName)
	running := normalizeVersion(current)

	if latest == "" {
		return checkFailed("Could not check for updates: GitHub did not return a release version.")
	}

	if latest == running {
		return CheckResult{Status: StatusUpToDate}
	}

	if !isNewer(latest, running) {
		return CheckResult{Status: StatusUpToDate}
	}

	return CheckResult{
		Status: StatusUpdateAvailable,
		Message: fmt.Sprintf(
			"Update available: %s -> %s\nTo update:\n%s",
			running, latest, updateInstructions(),
		),
	}
}

// normalizeVersion strips a leading "v" prefix.
func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// isNewer returns true if latest > current using simple semver comparison.
func isNewer(latest, current string) bool {
	latestParts := splitVersion(latest)
	currentParts := splitVersion(current)

	for i := 0; i < 3; i++ {
		if latestParts[i] > currentParts[i] {
			return true
		}
		if latestParts[i] < currentParts[i] {
			return false
		}
	}
	return false
}

// splitVersion splits "1.8.1" into [1, 8, 1]. Returns [0,0,0] on parse failure.
func splitVersion(v string) [3]int {
	var parts [3]int
	segments := strings.SplitN(v, ".", 3)
	for i, s := range segments {
		if i >= 3 {
			break
		}
		for _, c := range s {
			if c >= '0' && c <= '9' {
				parts[i] = parts[i]*10 + int(c-'0')
			} else {
				break
			}
		}
	}
	return parts
}

// ReleaseChecksConfigured keeps CLI and TUI offline unless an operator
// explicitly provides a release endpoint. There is no upstream fallback.
func ReleaseChecksConfigured() bool {
	return strings.TrimSpace(os.Getenv("RECALLDOCK_RELEASE_API_URL")) != "" || strings.TrimSpace(githubLatestReleaseURL) != ""
}

// updateCheckDisabled reports whether configured release checks were opted out.
func updateCheckDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("RECALLDOCK_DISABLE_UPDATE_CHECK"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// updateInstructions never points an independent build at the old upstream.
func updateInstructions() string {
	if root := strings.TrimSpace(os.Getenv("RECALLDOCK_SOURCE_ROOT")); root != "" {
		quotedRoot := "'" + strings.ReplaceAll(root, "'", "'\"'\"'") + "'"
		return fmt.Sprintf("  Rebuild RecallDock from the reviewed local checkout:\n    cd %s && go build -o bin/recalldock.bin ./cmd/engram\n  Do not install an unrelated upstream release.", quotedRoot)
	}
	return "  Rebuild RecallDock from the reviewed local checkout:\n    go build -o bin/recalldock.bin ./cmd/engram\n  Set RECALLDOCK_SOURCE_ROOT for the exact local command. Do not install an unrelated upstream release."
}

func githubToken() string {
	// A custom release endpoint must never inherit unrelated account credentials.
	return strings.TrimSpace(os.Getenv("RECALLDOCK_RELEASE_TOKEN"))
}

func nonOKStatusMessage(status string) string {
	msg := fmt.Sprintf("Could not check for updates: GitHub API returned %s.", status)
	if strings.HasPrefix(status, "401") || strings.HasPrefix(status, "403") {
		msg += " Configure RECALLDOCK_RELEASE_TOKEN only if the selected release endpoint requires it."
	}
	return msg
}

func checkFailed(message string) CheckResult {
	return CheckResult{Status: StatusCheckFailed, Message: message}
}

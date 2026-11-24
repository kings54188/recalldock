package version

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeVersion(t *testing.T) {
	tests := []struct{ in, want string }{
		{"v1.8.1", "1.8.1"},
		{"1.8.1", "1.8.1"},
		{" v2.0.0 ", "2.0.0"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := normalizeVersion(tt.in); got != tt.want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSplitVersion(t *testing.T) {
	tests := []struct {
		in   string
		want [3]int
	}{
		{"1.8.1", [3]int{1, 8, 1}},
		{"2.0.0", [3]int{2, 0, 0}},
		{"1.0", [3]int{1, 0, 0}},
		{"", [3]int{0, 0, 0}},
		{"1.8.1-beta", [3]int{1, 8, 1}},
	}
	for _, tt := range tests {
		if got := splitVersion(tt.in); got != tt.want {
			t.Errorf("splitVersion(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestIsNewer(t *testing.T) {
	tests := []struct {
		latest, current string
		want            bool
	}{
		{"1.8.1", "1.8.0", true},
		{"2.0.0", "1.9.9", true},
		{"1.8.1", "1.8.1", false},
		{"1.7.0", "1.8.1", false},
		{"1.8.2", "1.8.1", true},
	}
	for _, tt := range tests {
		if got := isNewer(tt.latest, tt.current); got != tt.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", tt.latest, tt.current, got, tt.want)
		}
	}
}

func TestCheckLatest(t *testing.T) {
	t.Run("dev and empty versions fail honestly", func(t *testing.T) {
		t.Setenv("RECALLDOCK_RELEASE_API_URL", "https://release.invalid")
		result := CheckLatest("dev")
		if result.Status != StatusCheckFailed {
			t.Fatalf("status = %q, want %q", result.Status, StatusCheckFailed)
		}
		if !strings.Contains(result.Message, "do not map to a release version") {
			t.Fatalf("message = %q", result.Message)
		}

		result = CheckLatest("")
		if result.Status != StatusCheckFailed {
			t.Fatalf("status = %q, want %q", result.Status, StatusCheckFailed)
		}
		if !strings.Contains(result.Message, "current version is unknown") {
			t.Fatalf("message = %q", result.Message)
		}
	})

	t.Run("update available", func(t *testing.T) {
		withCheckServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tag_name":"v1.10.8"}`))
		}))

		result := CheckLatest("1.10.7")
		if result.Status != StatusUpdateAvailable {
			t.Fatalf("status = %q, want %q", result.Status, StatusUpdateAvailable)
		}
		if !strings.Contains(result.Message, "Update available: 1.10.7 -> 1.10.8") || !strings.Contains(result.Message, "To update:") {
			t.Fatalf("message = %q", result.Message)
		}
	})

	t.Run("up to date", func(t *testing.T) {
		withCheckServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tag_name":"v1.10.7"}`))
		}))

		result := CheckLatest("1.10.7")
		if result.Status != StatusUpToDate {
			t.Fatalf("status = %q, want %q", result.Status, StatusUpToDate)
		}
		if result.Message != "" {
			t.Fatalf("message = %q, want empty", result.Message)
		}
	})

	t.Run("non-200 becomes check failed", func(t *testing.T) {
		withCheckServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "rate limited", http.StatusForbidden)
		}))

		result := CheckLatest("1.10.7")
		if result.Status != StatusCheckFailed {
			t.Fatalf("status = %q, want %q", result.Status, StatusCheckFailed)
		}
		if !strings.Contains(result.Message, "403 Forbidden") || !strings.Contains(result.Message, "RECALLDOCK_RELEASE_TOKEN") {
			t.Fatalf("message = %q", result.Message)
		}
	})

	t.Run("decode error becomes check failed", func(t *testing.T) {
		withCheckServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tag_name":`))
		}))

		result := CheckLatest("1.10.7")
		if result.Status != StatusCheckFailed {
			t.Fatalf("status = %q, want %q", result.Status, StatusCheckFailed)
		}
		if !strings.Contains(result.Message, "could not read the GitHub response") {
			t.Fatalf("message = %q", result.Message)
		}
	})

	t.Run("missing tag becomes check failed", func(t *testing.T) {
		withCheckServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tag_name":""}`))
		}))

		result := CheckLatest("1.10.7")
		if result.Status != StatusCheckFailed {
			t.Fatalf("status = %q, want %q", result.Status, StatusCheckFailed)
		}
		if !strings.Contains(result.Message, "did not return a release version") {
			t.Fatalf("message = %q", result.Message)
		}
	})

	t.Run("timeout becomes check failed", func(t *testing.T) {
		withCheckTimeout(t, 20*time.Millisecond)
		withCheckServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(50 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tag_name":"v1.10.8"}`))
		}))

		result := CheckLatest("1.10.7")
		if result.Status != StatusCheckFailed {
			t.Fatalf("status = %q, want %q", result.Status, StatusCheckFailed)
		}
		if !strings.Contains(result.Message, "took too long to respond") {
			t.Fatalf("message = %q", result.Message)
		}
	})
}

func TestCheckLatestUsesOnlyExplicitReleaseCredentials(t *testing.T) {
	for _, token := range []string{"", "release-test-token"} {
		t.Run(fmt.Sprintf("explicit=%t", token != ""), func(t *testing.T) {
			t.Setenv("GH_TOKEN", "unrelated-global-token")
			t.Setenv("GITHUB_TOKEN", "unrelated-account-token")
			t.Setenv("RECALLDOCK_RELEASE_TOKEN", token)
			withCheckServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want := ""
				if token != "" {
					want = "Bearer " + token
				}
				if r.Header.Get("Authorization") != want {
					t.Error("release request used unexpected credentials")
				}
				_, _ = w.Write([]byte(`{"tag_name":"v1.10.7"}`))
			}))
			if result := CheckLatest("1.10.7"); result.Status != StatusUpToDate {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestUpdateInstructions(t *testing.T) {
	msg := updateInstructions()
	if msg == "" {
		t.Fatal("expected non-empty update instructions")
	}
	if strings.Contains(msg, "go install recalldock.local/recalldock") {
		t.Fatalf("fork instructions must not point at the upstream module: %q", msg)
	}

	t.Setenv("RECALLDOCK_SOURCE_ROOT", `C:\fork\engram-next`)
	rooted := updateInstructions()
	if !strings.Contains(rooted, `C:\fork\engram-next`) {
		t.Fatalf("expected fork root in instructions: %q", rooted)
	}
}

func TestUpdateCheckDisabledByEnv(t *testing.T) {
	hit := false
	withCheckServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusInternalServerError)
	}))

	t.Setenv("RECALLDOCK_DISABLE_UPDATE_CHECK", "1")
	result := CheckLatest("1.10.7")
	if result.Status != StatusUpToDate || result.Message != "" {
		t.Fatalf("result = %+v, want up_to_date with empty message", result)
	}
	if hit {
		t.Fatal("release check must not contact GitHub when disabled")
	}
}

func withCheckServer(t *testing.T, handler http.Handler) {
	t.Helper()

	srv := httptest.NewServer(handler)
	oldURL := githubLatestReleaseURL
	githubLatestReleaseURL = srv.URL
	t.Cleanup(func() {
		githubLatestReleaseURL = oldURL
		srv.Close()
	})
}

func withCheckTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()

	oldTimeout := checkTimeout
	checkTimeout = timeout
	t.Cleanup(func() { checkTimeout = oldTimeout })
}

func TestNonOKStatusMessage(t *testing.T) {
	if got := nonOKStatusMessage(fmt.Sprintf("%d %s", http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))); !strings.Contains(got, "RECALLDOCK_RELEASE_TOKEN") {
		t.Fatalf("message = %q", got)
	}
}

// TestUpdateInstructionsUseExistingCommandPath pins the rebuild command to a
// directory that exists in this repository.
//
// The guidance previously said ./cmd/engram, a path inherited from the upstream
// baseline. Following it fails outright with "no Go files", which is worse than
// saying nothing when an operator is already recovering from a broken install.
// This asserts the module path rather than the string alone, so renaming the
// command package without updating the guidance fails here.
func TestUpdateInstructionsUseExistingCommandPath(t *testing.T) {
	const want = "./cmd/recalldock"
	for _, root := range []string{"", "/tmp/some root"} {
		if root == "" {
			t.Setenv("RECALLDOCK_SOURCE_ROOT", "")
		} else {
			t.Setenv("RECALLDOCK_SOURCE_ROOT", root)
		}
		got := updateInstructions()
		if !strings.Contains(got, want) {
			t.Fatalf("update instructions with RECALLDOCK_SOURCE_ROOT=%q do not mention %s:\n%s", root, want, got)
		}
		if strings.Contains(got, "./cmd/engram") {
			t.Fatalf("update instructions still point at ./cmd/engram:\n%s", got)
		}
	}
}

// TestUpdateInstructionsCommandPathExists fails when the guidance names a
// directory this checkout does not actually contain.
func TestUpdateInstructionsCommandPathExists(t *testing.T) {
	// internal/version -> repository root
	root := filepath.Join("..", "..")
	for _, dir := range []string{"cmd/recalldock"} {
		info, err := os.Stat(filepath.Join(root, dir))
		if err != nil || !info.IsDir() {
			t.Fatalf("update instructions reference %q, which is not a directory in this checkout: %v", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "cmd", "engram")); err == nil {
		t.Fatal("cmd/engram exists again; re-check which command package is authoritative")
	}
}

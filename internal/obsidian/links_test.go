package obsidian

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"recalldock.local/recalldock/internal/store"
)

func TestExportedWikilinksResolveInsideIndependentNamespace(t *testing.T) {
	for _, tc := range []struct {
		name, session string
		count         int
	}{{"singleton", "session-one", 1}, {"topic_hub", "session-two", 2}, {"unsafe_session", "../../outside", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			vault := t.TempDir()
			project := "proj"
			data := &store.ExportData{}
			for i := 0; i < tc.count; i++ {
				topic := fmt.Sprintf("decision/item-%d", i)
				data.Observations = append(data.Observations, store.Observation{ID: int64(i + 1), SessionID: tc.session, Type: "decision", Title: fmt.Sprintf("Memory %d", i), Content: "Synthetic graph fixture", Project: &project, Scope: "project", TopicKey: &topic})
			}
			exporter := NewExporter(&mockStore{exportData: data, stats: &store.Stats{}}, ExportConfig{VaultPath: vault, GraphConfig: GraphConfigSkip})
			result, err := exporter.Export()
			if err != nil || len(result.Errors) != 0 {
				t.Fatalf("export=%+v err=%v", result, err)
			}
			links := regexp.MustCompile(`\[\[([^\]|]+)`)
			found := 0
			err = filepath.WalkDir(vault, func(path string, entry fs.DirEntry, err error) error {
				if err != nil || entry.IsDir() || filepath.Ext(path) != ".md" {
					return err
				}
				if !strings.HasPrefix(path, filepath.Join(vault, "recalldock")+string(filepath.Separator)) {
					t.Errorf("generated note escaped the managed namespace: %s", path)
				}
				body, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				for _, match := range links.FindAllStringSubmatch(string(body), -1) {
					target := match[1]
					if !strings.HasPrefix(target, "recalldock/") {
						t.Errorf("link must use a vault-relative independent target: %s", target)
					}
					if _, err := os.Stat(filepath.Join(vault, filepath.FromSlash(target)+".md")); err != nil {
						t.Errorf("unresolved generated link %q", target)
					}
					found++
				}
				return nil
			})
			if err != nil || found < 2 {
				t.Fatalf("graph verification: links=%d err=%v", found, err)
			}
		})
	}
}

func TestTopicMembershipReconcilesWithoutObservationRevisionChange(t *testing.T) {
	vault := t.TempDir()
	project, topic := "project", "auth/choice"
	ms := &mockStore{exportData: &store.ExportData{Observations: []store.Observation{{ID: 1, SessionID: "s", Type: "decision", Title: "First", Project: &project, TopicKey: &topic, UpdatedAt: "2020-01-01T00:00:00Z"}}}, stats: &store.Stats{}}
	exporter := NewExporter(ms, ExportConfig{VaultPath: vault})
	assertExport := func() {
		t.Helper()
		r, err := exporter.Export()
		if err != nil || len(r.Errors) != 0 {
			t.Fatalf("export: %+v %v", r, err)
		}
	}
	assertExport()
	note := filepath.Join(vault, "recalldock/project/decision/first-1.md")
	body, _ := os.ReadFile(note)
	if strings.Contains(string(body), "*Topic*:") {
		t.Fatal("singleton must not link to a nonexistent hub")
	}
	ms.exportData.Observations = append(ms.exportData.Observations, store.Observation{ID: 2, SessionID: "s", Type: "decision", Title: "Second", Project: &project, TopicKey: &topic})
	assertExport()
	body, _ = os.ReadFile(note)
	if !strings.Contains(string(body), "[[recalldock/_topics/auth]]") {
		t.Fatal("unchanged observation must pick up newly materialized topic hub")
	}
	deleted := "2026-01-01T00:00:00Z"
	ms.exportData.Observations[1].DeletedAt = &deleted
	assertExport()
	body, _ = os.ReadFile(note)
	if strings.Contains(string(body), "*Topic*:") {
		t.Fatal("removed membership must not leave a topic edge")
	}
}

func TestHubStemPreventsTraversalAndSanitizationCollisions(t *testing.T) {
	seen := make(map[string]string)
	for _, raw := range []string{"../../outside", `..\outside`, "a/b", "a--b", "a_b", "[[injected]]", "", "simple-1"} {
		stem := hubStem(raw)
		if !plainHubStem.MatchString(stem) {
			t.Fatalf("unsafe stem %q", stem)
		}
		if previous, ok := seen[stem]; ok {
			t.Fatalf("collision: %q and %q", previous, raw)
		}
		seen[stem] = raw
	}
}

package store

import (
	"database/sql"
	"strings"
	"testing"
)

func TestMergedProjectRejectsStaleWritersWithoutRedirecting(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("prior", "old-key", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	identity, err := s.Identity()
	if err != nil {
		t.Fatal(err)
	}
	// An already-open connection represents an old runtime that knows no new API.
	old, err := sql.Open("sqlite", identity.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err := old.Ping(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MergeProjects([]string{"old-key"}, "current-key"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyProjectMappings(identity.StoreID, []ProjectMapping{{Source: "old-key", Target: "current-key"}}); err != nil {
		t.Fatal("manifest replay failed", err)
	}
	if _, err := old.Exec(`INSERT INTO sessions(id,project,directory) VALUES('stale','old-key','/synthetic')`); err == nil || !strings.Contains(err.Error(), "project key retired") {
		t.Fatalf("old connection recreated merged key: %v", err)
	}
	if err := s.CreateSession("new-stale", "old-key", "/synthetic"); err == nil {
		t.Fatal("current API recreated source")
	}
	if _, err := s.MergeProjects([]string{"current-key"}, "old-key"); err == nil {
		t.Fatal("retired target silently revived")
	}
	if _, err := s.MergeProjects([]string{"old-key"}, "another-key"); err == nil {
		t.Fatal("retired source silently retargeted")
	}
	if _, err := s.MergeProjects([]string{"old-key"}, "current-key"); err != nil {
		t.Fatal("same mapping replay is not idempotent", err)
	}
	if err := s.CreateSession("current", "current-key", "/synthetic"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("similar", "old_key", "/synthetic"); err != nil {
		t.Fatal("unselected similar key was retired", err)
	}
	got, err := s.GetSession("prior")
	if err != nil || got.Project != "current-key" {
		t.Fatal("original data lost", err)
	}
}

func TestProjectRetirementFailureRollsBackMigration(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("prior", "source", "/synthetic"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER test_retire_failure BEFORE INSERT ON project_key_retirements BEGIN SELECT RAISE(ABORT,'synthetic retirement failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MergeProjects([]string{"source"}, "target"); err == nil {
		t.Fatal("failed retirement committed migration")
	}
	got, err := s.GetSession("prior")
	if err != nil || got.Project != "source" {
		t.Fatal("ownership partially migrated", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM project_key_retirements`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial retirement", err)
	}
}

func TestEmptyRegisteredMergeRetiresIdentityButNotUnknownTypo(t *testing.T) {
	s := newTestStore(t)
	p, err := s.CreateRegisteredProject("Empty retired source")
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.MergeProjects([]string{p.ID}, "target")
	if err != nil || result.KeysRetired != 1 {
		t.Fatal(result, err)
	}
	if _, err := s.BindProjectRoot(p.ID, t.TempDir()); err == nil {
		t.Fatal("retired ID rebound")
	}
	if _, err := s.MergeProjects([]string{"nonexistent-typo"}, "target"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("unrelated", "nonexistent-typo", "/synthetic"); err != nil {
		t.Fatal("unknown key reserved", err)
	}
}

func TestMappingPreservesExactUnicodeSource(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.db.Exec(`INSERT INTO sessions(id,project,directory) VALUES('unicode','Équipe Studio','/synthetic')`); err != nil {
		t.Fatal(err)
	}
	identity, err := s.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyProjectMappings(identity.StoreID, []ProjectMapping{{Source: "Équipe Studio", Target: "current"}}); err != nil {
		t.Fatal("exact source not found", err)
	}
	got, err := s.GetSession("unicode")
	if err != nil || got.Project != "current" {
		t.Fatal("Unicode source stranded", err)
	}
}

func TestLegacyMigrationUsesProjectMergeGuards(t *testing.T) {
	t.Run("active registered source", func(t *testing.T) {
		s := newTestStore(t)
		p, err := s.CreateRegisteredProject("Bound source")
		if err != nil {
			t.Fatal(err)
		}
		root := t.TempDir()
		if _, err := s.BindProjectRoot(p.ID, root); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateSession("bound", p.ID, root); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MigrateProject(p.ID, "target"); err == nil {
			t.Fatal("legacy migration bypassed active binding guard")
		}
		got, err := s.GetSession("bound")
		if err != nil || got.Project != p.ID {
			t.Fatalf("rejected migration changed ownership: %+v %v", got, err)
		}
	})
	t.Run("unknown registered target", func(t *testing.T) {
		s := newTestStore(t)
		if err := s.CreateSession("prior", "source", "/synthetic"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MigrateProject("source", "prj-"+strings.Repeat("0", 32)); err == nil {
			t.Fatal("legacy migration accepted an unknown registered target")
		}
	})
	t.Run("ownership and retirement", func(t *testing.T) {
		s := newTestStore(t)
		if err := s.CreateSession("prior", "source", "/synthetic"); err != nil {
			t.Fatal(err)
		}
		if err := s.EnrollProject("source"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MigrateProject("source", "target"); err != nil {
			t.Fatal(err)
		}
		if enrolled, err := s.IsProjectEnrolled("target"); err != nil || !enrolled {
			t.Fatalf("legacy migration stranded enrollment: %v %v", enrolled, err)
		}
		if err := s.CreateSession("stale", "source", "/synthetic"); err == nil {
			t.Fatal("legacy migration did not retire the old key")
		}
	})
	t.Run("late failure rolls back sync ownership", func(t *testing.T) {
		s := newTestStore(t)
		if err := s.CreateSession("prior", "source", "/synthetic"); err != nil {
			t.Fatal(err)
		}
		if err := s.EnrollProject("source"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`CREATE TRIGGER test_legacy_retire_failure BEFORE INSERT ON project_key_retirements BEGIN SELECT RAISE(ABORT,'synthetic retirement failure'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MigrateProject("source", "target"); err == nil {
			t.Fatal("legacy migration ignored retirement failure")
		}
		got, err := s.GetSession("prior")
		if err != nil || got.Project != "source" {
			t.Fatalf("failed migration changed session: %+v %v", got, err)
		}
		if enrolled, err := s.IsProjectEnrolled("source"); err != nil || !enrolled {
			t.Fatalf("failed migration lost enrollment: %v %v", enrolled, err)
		}
		if enrolled, err := s.IsProjectEnrolled("target"); err != nil || enrolled {
			t.Fatalf("failed migration partially enrolled target: %v %v", enrolled, err)
		}
	})
}

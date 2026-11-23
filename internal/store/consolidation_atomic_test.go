package store

import (
	"fmt"
	"testing"
)

func TestConsolidationLineageFailureRollsBackArchive(t *testing.T) {
	s, target, source, _ := refineFixture(t)
	before, err := s.GetObservation(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_consolidation_lineage BEFORE INSERT ON memory_consolidations BEGIN SELECT RAISE(ABORT, 'lineage failed'); END`); err != nil {
		t.Fatal(err)
	}
	absorbed, err := s.ConsolidateObservations(target, []int64{source}, "reviewed consolidation", "")
	if err == nil || len(absorbed) != 0 {
		t.Fatalf("absorbed=%v err=%v", absorbed, err)
	}
	after, err := s.GetObservation(source)
	if err != nil || after.ArchivedAt != nil || after.RevisionCount != before.RevisionCount {
		t.Fatalf("archive must roll back: %+v %v", after, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM observation_edit_audits WHERE observation_id=?`, source).Scan(&count); err != nil || count != 0 {
		t.Fatalf("audit count=%d err=%v", count, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_consolidation_lineage`); err != nil {
		t.Fatal(err)
	}
	absorbed, err = s.ConsolidateObservations(target, []int64{source}, "retry consolidation", "")
	if err != nil || len(absorbed) != 1 {
		t.Fatalf("retry absorbed=%v err=%v", absorbed, err)
	}
	sources, err := s.ConsolidatedSources(target)
	if err != nil || len(sources) != 1 || sources[0].ID != source {
		t.Fatalf("lineage=%+v err=%v", sources, err)
	}
}

func TestConsolidationRejectsTargetChangedBeforeWrite(t *testing.T) {
	s, target, source, _ := refineFixture(t)
	trigger := fmt.Sprintf(`CREATE TRIGGER change_consolidation_target AFTER UPDATE OF id ON observations WHEN NEW.id=%d BEGIN UPDATE observations SET content='changed after review' WHERE id=%d; END`, target, target)
	if _, err := s.db.Exec(trigger); err != nil {
		t.Fatal(err)
	}
	absorbed, err := s.ConsolidateObservations(target, []int64{source}, "reviewed consolidation", "")
	if err == nil || len(absorbed) != 0 {
		t.Fatalf("target change absorbed=%v err=%v", absorbed, err)
	}
	after, err := s.GetObservation(source)
	if err != nil || after.ArchivedAt != nil {
		t.Fatalf("source archived: %+v %v", after, err)
	}
}

func TestConsolidationRejectsSourceChangedBeforeWrite(t *testing.T) {
	s, target, source, _ := refineFixture(t)
	trigger := fmt.Sprintf(`CREATE TRIGGER change_consolidation_source AFTER UPDATE OF id ON observations WHEN NEW.id=%d BEGIN UPDATE observations SET content='new source detail' WHERE id=%d; END`, target, source)
	if _, err := s.db.Exec(trigger); err != nil {
		t.Fatal(err)
	}
	absorbed, err := s.ConsolidateObservations(target, []int64{source}, "reviewed consolidation", "")
	if err == nil || len(absorbed) != 0 {
		t.Fatalf("source change absorbed=%v err=%v", absorbed, err)
	}
	after, err := s.GetObservation(source)
	if err != nil || after.ArchivedAt != nil {
		t.Fatalf("source archived: %+v %v", after, err)
	}
}

func TestConsolidationChecksScopeAfterWriterReservation(t *testing.T) {
	s, target, source, _ := refineFixture(t)
	// Simulate a change after the preliminary read and before the transactional
	// snapshot. The target write-lock statement invokes this trigger.
	trigger := fmt.Sprintf(`CREATE TRIGGER change_consolidation_scope AFTER UPDATE OF id ON observations WHEN NEW.id=%d BEGIN UPDATE observations SET scope='personal' WHERE id=%d; END`, target, source)
	if _, err := s.db.Exec(trigger); err != nil {
		t.Fatal(err)
	}
	absorbed, err := s.ConsolidateObservations(target, []int64{source}, "reviewed consolidation", "")
	if err == nil || len(absorbed) != 0 {
		t.Fatalf("scope change absorbed=%v err=%v", absorbed, err)
	}
	after, err := s.GetObservation(source)
	if err != nil || after.ArchivedAt != nil {
		t.Fatalf("source archived: %+v %v", after, err)
	}
}

func TestConsolidationRejectsTargetArchivedAtWriterReservation(t *testing.T) {
	s, target, source, _ := refineFixture(t)
	trigger := fmt.Sprintf(`CREATE TRIGGER archive_consolidation_target AFTER UPDATE OF id ON observations WHEN NEW.id=%d BEGIN UPDATE observations SET archived_at='2026-01-01 00:00:00' WHERE id=%d; END`, target, target)
	if _, err := s.db.Exec(trigger); err != nil {
		t.Fatal(err)
	}
	absorbed, err := s.ConsolidateObservations(target, []int64{source}, "reviewed consolidation", "")
	if err == nil || len(absorbed) != 0 {
		t.Fatalf("archived target absorbed=%v err=%v", absorbed, err)
	}
	after, err := s.GetObservation(source)
	if err != nil || after.ArchivedAt != nil {
		t.Fatalf("source archived: %+v %v", after, err)
	}
}

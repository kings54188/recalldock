package diagnostic

import (
	"context"
	"encoding/json"
	"testing"

	"recalldock.local/recalldock/internal/store"
)

func TestMemoryClassificationCheckReportsCountsAndAmbiguousTypesReadOnly(t *testing.T) {
	s := newDiagnosticTestStore(t)
	if err := s.CreateSession("memory-class-doctor", "engram", "/tmp/engram"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	for _, params := range []store.AddObservationParams{
		{SessionID: "memory-class-doctor", Type: "checkpoint", Title: "checkpoint", Content: "classification doctor", Project: "engram"},
		{SessionID: "memory-class-doctor", Type: "decision", Title: "decision", Content: "classification doctor", Project: "engram"},
		{SessionID: "memory-class-doctor", Type: "custom_unknown_type", Title: "unknown", Content: "classification doctor", Project: "engram"},
	} {
		if _, err := s.AddObservation(params); err != nil {
			t.Fatalf("AddObservation(%s): %v", params.Type, err)
		}
	}

	report, err := NewRunner().RunOne(context.Background(), Scope{Store: s, Project: "engram"}, CheckMemoryClassification)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if report.Status != StatusWarning || len(report.Checks) != 1 || len(report.Checks[0].Findings) != 1 {
		t.Fatalf("report=%+v", report)
	}
	var evidence store.MemoryClassificationStats
	if err := json.Unmarshal(report.Checks[0].Evidence, &evidence); err != nil {
		t.Fatalf("decode evidence: %v", err)
	}
	if evidence.Total != 3 || evidence.ClassCounts[store.MemoryClassOperational] != 1 || evidence.ClassCounts[store.MemoryClassProject] != 2 {
		t.Fatalf("classification evidence=%+v", evidence)
	}
	if len(evidence.AmbiguousTypes) != 1 || evidence.AmbiguousTypes[0].Type != "custom_unknown_type" || evidence.AmbiguousTypes[0].Count != 1 {
		t.Fatalf("ambiguous types=%+v", evidence.AmbiguousTypes)
	}

	statsAfter, err := s.MemoryClassificationStats("engram")
	if err != nil {
		t.Fatalf("MemoryClassificationStats after doctor: %v", err)
	}
	if statsAfter.Total != evidence.Total {
		t.Fatalf("doctor changed observation count: before=%d after=%d", evidence.Total, statsAfter.Total)
	}
}

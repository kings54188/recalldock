package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionedCandidatePolicyPreservesCurrentLogicalModes(t *testing.T) {
	for mode, wire := range map[string]string{"manual": "manual", "duplicates": "duplicates-v2", "checked": "checked-v2", "tiered": "tiered-v4"} {
		t.Run(mode, func(t *testing.T) {
			s := newCandidateTestStore(t)
			if err := s.SetCandidateAdmissionMode(mode); err != nil {
				t.Fatal(err)
			}
			if got := s.CandidateAdmissionMode(); got != mode {
				t.Fatalf("effective mode=%s want=%s", got, mode)
			}
			raw, err := os.ReadFile(filepath.Join(s.cfg.DataDir, "candidate-admission.json"))
			if err != nil {
				t.Fatal(err)
			}
			var persisted struct {
				Version int    `json:"version"`
				Mode    string `json:"mode"`
			}
			wantVersion := 2
			if strings.HasSuffix(wire, "-v4") {
				wantVersion = 4
			}
			if err := json.Unmarshal(raw, &persisted); err != nil || persisted.Version != wantVersion || persisted.Mode != wire {
				t.Fatalf("policy does not exclude legacy automatic readers: %+v %v", persisted, err)
			}
		})
	}
}

func TestCandidatePolicyVersionParsingFailsClosed(t *testing.T) {
	for _, tc := range []struct{ raw, expected string }{
		{`{"mode":"checked"}`, "checked"},
		{`{"mode":"duplicates"}`, "duplicates"},
		{`{"version":0,"mode":"checked"}`, "manual"},
		{`{"version":null,"mode":"checked"}`, "manual"},
		{`{"version":"2","mode":"checked-v2"}`, "manual"},
		{`{"version":2,"mode":"checked-v2"}`, "checked"},
		{`{"version":2,"mode":"duplicates-v2"}`, "duplicates"},
		{`{"version":2,"mode":"checked"}`, "manual"},
		{`{"version":3,"mode":"checked-v2"}`, "manual"},
		{`{"version":3,"mode":"tiered-v3"}`, "tiered"},
		{`{"version":4,"mode":"tiered-v4"}`, "tiered"},
		{`{"version":3,"mode":"tiered-v4"}`, "manual"},
		{`{"version":4,"mode":"tiered-v3"}`, "manual"},
		{`{"version":4,"mode":"tiered-v4","approval":true}`, "manual"},
		{`{"mode":"checked-v2"}`, "manual"},
		{`{"version":2,"mode":"checked-v2","unknown":true}`, "manual"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			s := newCandidateTestStore(t)
			if err := os.WriteFile(filepath.Join(s.cfg.DataDir, "candidate-admission.json"), []byte(tc.raw), 0600); err != nil {
				t.Fatal(err)
			}
			if got := s.CandidateAdmissionMode(); got != tc.expected {
				t.Fatalf("effective mode=%s want=%s", got, tc.expected)
			}
		})
	}
}

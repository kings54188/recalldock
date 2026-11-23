package store

import (
	"math"
	"strings"
	"testing"
)

func TestSimilarMemoryGroupsFindsNearDuplicates(t *testing.T) {
	cfg, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = t.TempDir()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateSession("sim-session", "sim-proj", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	add := func(title, content string) int64 {
		id, err := s.AddObservation(AddObservationParams{SessionID: "sim-session", Type: "experience", Title: title, Content: content, Project: "sim-proj"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := add("验证要区分已证实未证实", "诊断结论必须区分已证实与未证实，并以当前磁盘为准。")
	second := add("结论区分已证实未证实", "诊断结论要区分已证实/未证实，并以当前磁盘、进程为准，不要拿记忆当事实。")
	unrelated := add("CMake rpath 绝对路径", "CMake 产物的 rpath 是绝对路径，移动 build 目录后需要重新设置。")
	archived := add("已归档的近似条目", "诊断结论必须区分已证实与未证实，并以当前磁盘为准。")
	selected, err := s.SelectObservationForEdit(archived)
	if err != nil {
		t.Fatal(err)
	}
	flag := true
	if _, err := s.UpdateObservationCAS(archived, selected.SnapshotSHA256, UpdateObservationParams{Archived: &flag}, "similar test", ""); err != nil {
		t.Fatal(err)
	}

	groups, err := s.SimilarMemoryGroups("sim-proj", 0.5, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1: %+v", len(groups), groups)
	}
	group := groups[0]
	if len(group.Observations) != 2 {
		t.Fatalf("group members = %d, want 2", len(group.Observations))
	}
	if group.SuggestedTarget != second {
		t.Fatalf("suggested target = %d, want longest #%d", group.SuggestedTarget, second)
	}
	for _, observation := range group.Observations {
		if observation.ID == archived || observation.ID == unrelated {
			t.Fatalf("group leaked archived/unrelated #%d: %+v", observation.ID, group)
		}
	}
	if !strings.Contains(group.Observations[0].Title+group.Observations[1].Title, "证实") {
		t.Fatalf("unexpected group: %+v", group)
	}

	high, err := s.SimilarMemoryGroups("sim-proj", 0.95, 10)
	if err != nil || len(high) != 0 {
		t.Fatalf("high threshold should not group: %+v %v", high, err)
	}
	if _, err := s.SimilarMemoryGroups("", 0.6, 10); err == nil {
		t.Fatal("empty project should be refused")
	}
	nanGroups, err := s.SimilarMemoryGroups("sim-proj", math.NaN(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range nanGroups {
		for _, obs := range group.Observations {
			if obs.ID == unrelated {
				t.Fatal("NaN threshold grouped unrelated memory")
			}
		}
	}
	_ = first
}

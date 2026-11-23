package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProgressiveSearchJSONContainsOnlySelectedContent(t *testing.T) {
	r := SearchResult{Observation: Observation{Title: "Chosen title", Content: "UNSELECTED_FULL_BODY", Abstract: "UNSELECTED_RAW_ABSTRACT", Overview: "UNSELECTED_RAW_OVERVIEW"}, SelectedContent: "bounded chosen text"}
	for _, detail := range []string{SearchDetailL0, SearchDetailL1, searchDetailL2} {
		r.SelectedDetail = detail
		body, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		for _, hidden := range []string{"UNSELECTED_FULL_BODY", "UNSELECTED_RAW_ABSTRACT", "UNSELECTED_RAW_OVERVIEW"} {
			if strings.Contains(string(body), hidden) {
				t.Fatalf("%s serialized unselected material: %s", detail, body)
			}
		}
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded["selected_content"] != "bounded chosen text" {
			t.Fatal("selected content missing")
		}
		if detail == searchDetailL2 && decoded["content"] != "bounded chosen text" {
			t.Fatal("L2 ignored bounded selection")
		}
		if detail != searchDetailL2 {
			if _, ok := decoded["content"]; ok {
				t.Fatal("index exposed body field")
			}
		}
	}
	r.SelectedDetail = ""
	body, _ := json.Marshal(r)
	if !strings.Contains(string(body), "UNSELECTED_FULL_BODY") {
		t.Fatal("explicit legacy operator serialization changed")
	}
}

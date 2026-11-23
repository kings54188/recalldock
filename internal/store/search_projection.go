package store

import "encoding/json"

// MarshalJSON keeps progressive search responses progressive. The internal
// Observation is retained for ranking, but its unselected body/summaries must
// not leak through embedded JSON fields alongside a bounded selected_content.
func (r SearchResult) MarshalJSON() ([]byte, error) {
	type alias SearchResult
	if r.SelectedDetail != SearchDetailL0 && r.SelectedDetail != SearchDetailL1 && r.SelectedDetail != searchDetailL2 {
		return json.Marshal(alias(r))
	}
	var content *string
	if r.SelectedDetail == searchDetailL2 {
		selected := r.SelectedContent
		content = &selected
	}
	return json.Marshal(struct {
		alias
		Content  *string `json:"content,omitempty"`
		Abstract *string `json:"abstract,omitempty"`
		Overview *string `json:"overview,omitempty"`
	}{alias: alias(r), Content: content})
}

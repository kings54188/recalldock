package store

import (
	"errors"
	"fmt"
	"testing"
)

func TestSearchModeErrorsHaveTypedClassification(t *testing.T) {
	s := newTestStore(t)
	for _, opts := range []SearchOptions{{MatchMode: "bogus"}, {Profile: "bogus"}} {
		if _, err := s.Search("marker", opts); !errors.Is(err, ErrInvalidSearchOptions) {
			t.Errorf("flat search lost parameter classification: %v", err)
		}
		if _, err := s.SearchDetailed("marker", opts); !errors.Is(err, ErrInvalidSearchOptions) {
			t.Errorf("detailed search lost parameter classification: %v", err)
		}
	}
}

func TestCLIErrorDescriptionsOmitMachinePrefix(t *testing.T) {
	for _, code := range []string{"invalid_arguments", "invalid_query", "query_has_no_terms", "budget_too_small"} {
		err := fmt.Errorf("%s: useful description", code)
		if got := ErrorMessage(err); got != "useful description" {
			t.Errorf("machine prefix in human message: %q", got)
		}
	}
}

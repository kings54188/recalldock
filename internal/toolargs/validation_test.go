package toolargs

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestValidationMatchesBoundedToolSchemas(t *testing.T) {
	schema := mcp.ToolInputSchema{Properties: map[string]any{
		"generation": map[string]any{"type": "integer", "minimum": 0, "maximum": 10},
		"mode":       map[string]any{"type": "string", "enum": []string{"l0", "l1"}, "maxLength": 2},
		"refresh":    map[string]any{"type": "boolean"},
		"arguments":  map[string]any{"type": "object"},
		"checkpoint": map[string]any{"type": "object", "additionalProperties": false, "required": []any{"goal"}, "properties": map[string]any{
			"goal": map[string]any{"type": "string", "minLength": 1, "maxLength": 3},
			"next": map[string]any{"type": "array", "maxItems": 1, "items": map[string]any{"type": "string"}},
		}},
	}, Required: []string{"generation"}}
	for _, value := range []any{0, int64(1), 2.0, json.Number("3")} {
		args := map[string]any{"generation": value, "mode": "l0", "refresh": true, "arguments": map[string]any{"native": []any{1, true}}, "checkpoint": map[string]any{"goal": "中文", "next": []any{"one"}}}
		if err := Validate(args, schema); err != nil {
			t.Fatalf("valid arguments: %v", err)
		}
		if _, ok := args["generation"].(float64); !ok {
			t.Fatal("numeric arguments not normalized for SDK getters")
		}
	}
	cases := []struct {
		args    map[string]any
		message string
	}{
		{map[string]any{}, "required"},
		{map[string]any{"generation": 0, "approval": true}, "unknown argument"},
		{map[string]any{"generation": "0"}, "finite number"},
		{map[string]any{"generation": 0.9}, "representable integer"},
		{map[string]any{"generation": math.Inf(1)}, "finite number"},
		{map[string]any{"generation": math.NaN()}, "finite number"},
		{map[string]any{"generation": 9007199254740992.0}, "representable integer"},
		{map[string]any{"generation": -1}, "minimum"},
		{map[string]any{"generation": 11}, "maximum"},
		{map[string]any{"generation": 0, "mode": false}, "must be a string"},
		{map[string]any{"generation": 0, "mode": "long"}, "maxLength"},
		{map[string]any{"generation": 0, "mode": "xx"}, "one of"},
		{map[string]any{"generation": 0, "refresh": "true"}, "boolean"},
		{map[string]any{"generation": 0, "arguments": []any{}}, "object"},
		{map[string]any{"generation": 0, "checkpoint": map[string]any{}}, "required"},
		{map[string]any{"generation": 0, "checkpoint": map[string]any{"goal": ""}}, "minLength"},
		{map[string]any{"generation": 0, "checkpoint": map[string]any{"goal": "ok", "unknown": true}}, "checkpoint.unknown"},
		{map[string]any{"generation": 0, "checkpoint": map[string]any{"goal": "ok", "next": "one"}}, "array"},
		{map[string]any{"generation": 0, "checkpoint": map[string]any{"goal": "ok", "next": []any{"one", "two"}}}, "maxItems"},
		{map[string]any{"generation": 0, "checkpoint": map[string]any{"goal": "ok", "next": []any{false}}}, "next[0]"},
	}
	for _, tc := range cases {
		if err := Validate(tc.args, schema); err == nil || !strings.Contains(err.Error(), tc.message) {
			t.Errorf("%+v: want %q, got %v", tc.args, tc.message, err)
		}
	}
}

package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"recalldock.local/recalldock/internal/opsstore"
)

// Built-in tools are part of the gateway, not a registered backend. Their
// schemas are declared here so tools/list and tools_describe agree, and so a
// model never has to guess progress-ledger field names.
var (
	checkpointSchema = map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"goal"},
		"properties": map[string]any{
			"goal": map[string]any{
				"type": "string", "maxLength": 300,
				"description": "Single-line task goal (required, <=300 bytes).",
			},
			"summary": map[string]any{
				"type":        "string",
				"description": "Bounded verified progress; reference paths/commands instead of pasting logs.",
			},
			"next": map[string]any{
				"type": "array", "maxItems": 3,
				"items":       map[string]any{"type": "string", "maxLength": 300},
				"description": "Up to 3 single-line next steps.",
			},
			"details": map[string]any{
				"type":        "string",
				"description": "Optional selected details; keep it small (L2 is capped at 12 KiB).",
			},
			"status": map[string]any{
				"type": "string", "enum": []string{"active", "blocked", "completed", "archived"},
				"description": "Default is active. Use completed only after verification.",
			},
			"blockers": map[string]any{
				"type": "array", "maxItems": 5,
				"items":       map[string]any{"type": "string", "maxLength": 240},
				"description": "Up to 5 short blocker summaries.",
			},
			"operations": map[string]any{
				"type": "array", "maxItems": 12,
				"description": "Optional reported operations; verified requires evidence.",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"id", "input_fingerprint", "status"},
					"properties": map[string]any{
						"id":                map[string]any{"type": "string", "maxLength": 80},
						"input_fingerprint": map[string]any{"type": "string", "maxLength": 200},
						"status": map[string]any{
							"type": "string",
							"enum": []string{"reported", "started", "applied", "failed", "needs_check", "verified"},
						},
						"evidence": map[string]any{"type": "string", "maxLength": 400},
					},
				},
			},
		},
	}

	eventSchema = map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"id", "input_fingerprint", "status", "summary"},
		"properties": map[string]any{
			"id":                map[string]any{"type": "string", "maxLength": 80},
			"input_fingerprint": map[string]any{"type": "string", "maxLength": 200},
			"kind": map[string]any{
				"type": "string", "enum": []string{"progress", "attempt", "hypothesis", "blocker", "validation"},
			},
			"status": map[string]any{
				"type": "string",
				"enum": []string{"reported", "started", "applied", "failed", "needs_check", "verified"},
			},
			"summary":  map[string]any{"type": "string", "maxLength": 600},
			"evidence": map[string]any{"type": "string", "maxLength": 400},
			"details":  map[string]any{"type": "string", "maxLength": 6000},
		},
	}

	patchSchema = map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"summary": map[string]any{"type": "string"},
			"next": map[string]any{
				"type": "array", "maxItems": 3,
				"items": map[string]any{"type": "string", "maxLength": 300},
			},
			"status": map[string]any{
				"type": "string", "enum": []string{"active", "blocked", "completed", "archived"},
			},
			"blockers": map[string]any{
				"type": "array", "maxItems": 5,
				"items": map[string]any{"type": "string", "maxLength": 240},
			},
		},
	}
)

// withRawObject installs a full JSON Schema for one input property. mcp-go's
// WithObject leaves nested properties empty unless every field is repeated
// through helpers; a raw schema keeps the checkpoint contract exact.
func withRawObject(name string, schema map[string]any, required bool) mcp.ToolOption {
	return func(t *mcp.Tool) {
		if t.InputSchema.Properties == nil {
			t.InputSchema.Properties = map[string]any{}
		}
		t.InputSchema.Properties[name] = schema
		if required {
			t.InputSchema.Required = append(t.InputSchema.Required, name)
		}
	}
}

type serverSummary struct {
	ID           string `json:"id"`
	Description  string `json:"description"`
	Enabled      bool   `json:"enabled"`
	StartAllowed bool   `json:"start_allowed"`
}

type discoveryPage struct {
	Servers            []serverSummary        `json:"servers"`
	Tools              []opsstore.ToolSummary `json:"tools"`
	BuiltinTools       []builtinToolRecord    `json:"builtin_tools"`
	After              string                 `json:"after,omitempty"`
	ToolAfter          string                 `json:"tool_after,omitempty"`
	BuiltinAfter       string                 `json:"builtin_after,omitempty"`
	HasMore            bool                   `json:"has_more"`
	ToolHasMore        bool                   `json:"tool_has_more"`
	BuiltinHasMore     bool                   `json:"builtin_has_more"`
	Session            string                 `json:"session"`
	Next               string                 `json:"next,omitempty"`
	SummariesTruncated bool                   `json:"summaries_truncated,omitempty"`
}

func discoveryResult(servers []serverSummary, tools []opsstore.ToolSummary, builtin []builtinToolRecord, session string) (*mcp.CallToolResult, error) {
	ns, nt, nb := min(len(servers), 5), min(len(tools), 3), min(len(builtin), 3)
	summariesTruncated := false
	for {
		page := discoveryPage{Servers: servers[:ns], Tools: tools[:nt], BuiltinTools: builtin[:nb], HasMore: ns < len(servers), ToolHasMore: nt < len(tools), BuiltinHasMore: nb < len(builtin), Session: session, SummariesTruncated: summariesTruncated}
		if nt > 0 {
			page.Next = "Backend tools are not added to the host tool list. Use tools_describe(server,tool=name), then tools_call(server,tool=name,schema_hash,root,arguments)."
		}
		if page.HasMore && ns > 0 {
			page.After = servers[ns-1].ID
		}
		if page.ToolHasMore && nt > 0 {
			page.ToolAfter = tools[nt-1].Server + "\x00" + tools[nt-1].Name
		}
		if page.BuiltinHasMore && nb > 0 {
			page.BuiltinAfter = builtin[nb-1].Name
		}
		result, err := boundedResult(page, opsstore.SummaryLimit)
		if !errors.Is(err, opsstore.ErrBudget) {
			return result, err
		}
		switch {
		case ns > 1 && ns >= nt && ns >= nb:
			ns--
		case nt > 1 && nt >= nb:
			nt--
		case nb > 1:
			nb--
		case !summariesTruncated:
			summariesTruncated = true
			for i := range servers {
				servers[i].Description = clipText(servers[i].Description, 20)
			}
			for i := range tools {
				tools[i].Summary = clipText(tools[i].Summary, 20)
			}
			for i := range builtin {
				builtin[i].Summary = clipText(builtin[i].Summary, 20)
			}
		default:
			return nil, err
		}
	}
}

type builtinToolRecord struct {
	Name       string `json:"name"`
	Summary    string `json:"summary"`
	SchemaHash string `json:"schema_hash"`
	Schema     any    `json:"schema,omitempty"`
}

func builtinRecord(tool mcp.Tool, includeSchema bool) (builtinToolRecord, error) {
	body, err := json.Marshal(tool.InputSchema)
	if err != nil {
		return builtinToolRecord{}, err
	}
	sum := sha256.Sum256(body)
	record := builtinToolRecord{
		Name: tool.Name, Summary: clipText(tool.Description, 160),
		SchemaHash: hex.EncodeToString(sum[:]),
	}
	if includeSchema {
		record.Schema = tool.InputSchema
	}
	return record, nil
}

// describeBuiltin serves tools_describe for server=recalldock_gateway. These
// tools are called by name directly; tools_call is only for registered backends.
func describeBuiltin(tools map[string]mcp.Tool, name, after string) (*mcp.CallToolResult, error) {
	names := make([]string, 0, len(tools))
	for value := range tools {
		names = append(names, value)
	}
	sort.Strings(names)
	if name == "" {
		items := []builtinToolRecord{}
		for _, value := range names {
			if value <= after {
				continue
			}
			record, err := builtinRecord(tools[value], false)
			if err != nil {
				return nil, err
			}
			record.Summary = clipText(tools[value].Description, 80)
			items = append(items, record)
		}
		return boundedResult(map[string]any{
			"server": "recalldock_gateway", "transport": "builtin",
			"tools": items, "after": "", "has_more": false,
			"note": "Built-in gateway tools: call them directly by name; tools_call only calls registered backends.",
		}, opsstore.SummaryLimit)
	}
	tool, ok := tools[name]
	if !ok {
		return nil, fmt.Errorf("unknown built-in tool %q; available: %s", name, strings.Join(names, ", "))
	}
	record, err := builtinRecord(tool, true)
	if err != nil {
		return nil, err
	}
	return boundedResult(map[string]any{
		"server": "recalldock_gateway", "transport": "builtin",
		"name": record.Name, "summary": record.Summary,
		"schema_hash": record.SchemaHash, "schema": record.Schema,
		"note": "Call this tool directly by name; tools_call only calls registered backends.",
	}, 128<<10)
}

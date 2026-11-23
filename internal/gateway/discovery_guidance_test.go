package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"recalldock.local/recalldock/internal/opsstore"
)

func TestDiscoveryExplainsBackendInvocation(t *testing.T) {
	r, err := discoveryResult(nil, []opsstore.ToolSummary{{Server: "memory", Name: "mem_search", SchemaHash: "cached-schema"}}, nil, "gateway-session")
	if err != nil {
		t.Fatal(err)
	}
	body := schemaText(t, r)
	var envelope struct {
		Data struct {
			Next  string                 `json:"next"`
			Tools []opsstore.ToolSummary `json:"tools"`
		} `json:"data"`
	}
	if err = json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"tools_describe", "tools_call", "server", "name", "host"} {
		if !strings.Contains(envelope.Data.Next, part) {
			t.Fatalf("missing backend invocation guidance %q: %s", part, body)
		}
	}
	if len(envelope.Data.Tools) != 1 || envelope.Data.Tools[0].SchemaHash != "cached-schema" {
		t.Fatalf("lost stable identifiers: %s", body)
	}
}

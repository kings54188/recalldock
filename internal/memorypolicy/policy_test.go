package memorypolicy

import "testing"

func TestAgentPolicyFailsClosed(t *testing.T) {
	for _, tool := range []string{"mem_save", "mem_update", "mem_delete", "mem_capture_passive", "mem_save_prompt", "mem_session_summary", "mem_judge", "mem_compare", "mem_pin", "future_write_tool"} {
		if MCP(tool, "") != Deny {
			t.Fatal(tool)
		}
	}
	if MCP("mem_review", "list") != Read || MCP("mem_review", "mark") != Deny || MCP("mem_commit_session", "") != Propose {
		t.Fatal("mixed action policy")
	}
	for _, path := range []string{"/observations", "/observations/passive", "/prompts", "/import", "/projects/migrate", "/conflicts/judge", "/future"} {
		if HTTP("POST", path) != Deny {
			t.Fatal(path)
		}
	}
	if HTTP("POST", "/sessions/id/candidates") != Propose || HTTP("POST", "/sessions/id/end") != Lifecycle || HTTP("GET", "/search") != Read {
		t.Fatal("safe routes")
	}
}

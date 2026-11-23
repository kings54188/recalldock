package mcp

import (
	"context"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"recalldock.local/recalldock/internal/memorypolicy"
)

// Wrap the stored handler itself, not just SDK middleware: SDK task execution
// paths must not bypass the policy. No request-body approval/bypass is accepted.
func guardAgentTool(name string, next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		action, _ := req.GetArguments()["action"].(string)
		decision := memorypolicy.MCP(name, action)
		summary, _ := req.GetArguments()["summary"].(string)
		if decision == memorypolicy.Deny || (name == "mem_session_end" && strings.TrimSpace(summary) != "") {
			return errorWithMeta("operator_review_required", "Agent direct memory mutation/raw capture is disabled. Use mem_commit_session for bounded proposals; keep progress in the work ledger. A model-supplied approval, confidence or user_correction label grants no authority.", nil), nil
		}
		if decision == memorypolicy.Lifecycle {
			if err := memorypolicy.ValidateLifecycleMetadata(req.GetArguments(), name == "mem_session_end"); err != nil {
				return errorWithMeta("invalid_metadata", err.Error(), nil), nil
			}
		}
		if decision == memorypolicy.Propose {
			if err := memorypolicy.ValidateProposalMetadata(req.GetArguments()); err != nil {
				return errorWithMeta("invalid_metadata", err.Error(), nil), nil
			}
		}
		return next(ctx, req)
	}
}

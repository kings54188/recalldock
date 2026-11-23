package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"recalldock.local/recalldock/internal/store"
	"recalldock.local/recalldock/internal/toolargs"
)

func newSearchTool() mcp.Tool {
	tool := mcp.NewTool("mem_search",
		mcp.WithDescription("Search persistent memory in the selected project. all requires every meaningful term; any matches complete term alternatives, not arbitrary Chinese fragments. score is query-relative; raw_bm25 exposes native negative-better BM25 for direct FTS hits. Budget downgrades, cutoff ties and spelling suggestions are explicit."),
		mcp.WithTitleAnnotation("Search Memory"),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithSchemaAdditionalProperties(false),
		mcp.WithString("query", mcp.Required(), mcp.Description("Natural language or keywords. Contiguous Chinese uses bigram phrases; separate independent terms with spaces. Standalone 的/了/是/在/不/和 are ignored and reported; embedded negation is preserved.")),
		mcp.WithString("type", mcp.Description("Filter by a built-in observation type or a stored custom type in the selected scope; unknown values are errors.")),
		mcp.WithString("project", mcp.Description("Select one exact project; all_projects=true ignores this argument.")),
		mcp.WithBoolean("all_projects", mcp.Description("Explicitly search every project instead of the selected project.")),
		mcp.WithString("scope", mcp.Description("Filter by project, personal or historical global scope; invalid values are errors and do not broaden project selection.")),
		mcp.WithString("match_mode", mcp.Enum("", "all", "any"), mcp.Description("all (default): AND over meaningful terms, including hierarchy results. any: OR over complete whitespace-delimited groups; Chinese/ASCII parts within a mixed group remain AND. Raw FTS syntax controls its own boolean expression.")),
		mcp.WithBoolean("raw_fts", mcp.Description("Allow raw FTS5 syntax. Raw expressions use the flat path; a requested hierarchy fallback is reported.")),
		mcp.WithString("profile", mcp.Enum("", "all", "semantic", "operational"), mcp.Description("all (default), semantic, or operational.")),
		mcp.WithString("strategy", mcp.Enum("", "flat", "hierarchical", "auto"), mcp.Description("flat, hierarchical (default), or auto. all never admits hierarchy-only results missing a query term; use any for lexical-gap recall.")),
		mcp.WithString("detail", mcp.Enum("", "l0", "l1"), mcp.Description("l0 (default, abstract) or l1 (abstract plus overview). Budget-driven changes return detail_downgraded.")),
		mcp.WithNumber("token_budget", mcp.Description("Non-negative integer estimated-token budget for packed results. Zero means no packed-result budget.")),
		mcp.WithString("root_path", mcp.Description("Optional class-rooted virtual path that bounds recall, such as project/architecture.")),
		mcp.WithNumber("max_depth", mcp.Description("Non-negative integer hierarchy depth below root_path (default: 4).")),
		mcp.WithBoolean("trace", mcp.Description("Return content-free retrieval decisions.")),
		mcp.WithBoolean("explain_empty", mcp.Description("For empty results, add empty_explanation with bounded stage counts and verified relaxation suggestions; empty_reason is always available.")),
		mcp.WithBoolean("include_stale", mcp.Description("Include stale observations; default false.")),
		mcp.WithNumber("limit", mcp.Description("Positive integer result limit (default 10, clamped to 20 with an explicit notice).")),
	)
	for _, name := range []string{"token_budget", "max_depth", "limit"} {
		tool.InputSchema.Properties[name].(map[string]any)["type"] = "integer"
	}
	tool.InputSchema.Properties["scope"].(map[string]any)["enum"] = []string{"", "project", "personal", "global"}
	return tool
}

func guardToolArguments(schema mcp.ToolInputSchema, next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		if err := validateToolArguments(args, schema); err != nil {
			return errorWithMeta("invalid_arguments", err.Error(), nil), nil
		}
		req.Params.Arguments = args
		return next(ctx, req)
	}
}

func validateToolArguments(args map[string]any, schema mcp.ToolInputSchema) error {
	if _, known := schema.Properties["scope"]; known {
		if value, ok := args["scope"].(string); ok {
			scope, err := store.NormalizeReadScope(value)
			if err != nil {
				return err
			}
			args["scope"] = scope
		}
	}
	return toolargs.Validate(args, schema)
}

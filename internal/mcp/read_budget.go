package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"recalldock.local/recalldock/internal/memorypolicy"
	"recalldock.local/recalldock/internal/store"
)

const allProjectsBudgetKey = "all-projects"

type mcpReadGate struct {
	s        *store.Store
	state    store.ReadBudgetState
	err      error
	explicit bool
	tokens   int
	mu       sync.Mutex
	budgets  map[string]store.ReadBudgetState
}

func newMCPReadGate(s *store.Store, cfg MCPConfig) *mcpReadGate {
	cfg.DefaultProject, _ = store.NormalizeProject(cfg.DefaultProject)
	g := &mcpReadGate{s: s, tokens: cfg.ReadBudgetTokens, budgets: map[string]store.ReadBudgetState{}}
	if cfg.ReadBudgetID != "" {
		g.explicit = true
		g.state, g.err = s.ReadBudget(cfg.ReadBudgetID)
		if g.err == nil && g.state.Project == "" {
			g.err = store.ErrReadBudget // shared budgets must be project-scoped
		}
	} else if cfg.DefaultProject != "" {
		g.state, g.err = s.CreateReadBudget(cfg.DefaultProject, cfg.ReadBudgetTokens)
	}
	if g.err == nil && cfg.DefaultProject != "" && g.state.Project != "" && cfg.DefaultProject != g.state.Project {
		g.err = store.ErrReadBudget
	}
	return g
}

// budgetFor returns one project-isolated budget. A no-project read is not
// charged to another project's bucket, and a shared explicit budget can never
// be borrowed by a different project.
func (g *mcpReadGate) budgetFor(project string) (store.ReadBudgetState, error) {
	project, _ = store.NormalizeProject(project)
	if project == "" {
		// all_projects is a real read scope; keep it in its own bucket rather
		// than charging an arbitrary project or a shared empty bucket.
		project = allProjectsBudgetKey
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.explicit {
		// A host-supplied reference is an explicit global/selected allowance;
		// cross-project reads still use that same operator-owned envelope.
		if project != allProjectsBudgetKey && g.state.Project != project {
			return store.ReadBudgetState{}, store.ErrReadBudget
		}
		return g.state, nil
	}
	if g.state.ID != "" && g.state.Project == project {
		return g.state, nil
	}
	if state, ok := g.budgets[project]; ok {
		return state, nil
	}
	state, err := g.s.CreateReadBudget(project, g.tokens)
	if err != nil {
		return store.ReadBudgetState{}, err
	}
	g.budgets[project] = state
	return state, nil
}

func (g *mcpReadGate) catalog(srv *server.MCPServer) {
	if g.err != nil || g.state.ID == "" {
		return
	}
	tools := map[string]mcp.Tool{}
	for name, tool := range srv.ListTools() {
		tools[name] = tool.Tool
	}
	body, err := json.Marshal(map[string]any{"tools": tools, "instructions": serverInstructions})
	if err != nil {
		g.err = err
		return
	}
	// Reserve this server instance's initial catalog/instructions conservatively.
	g.state, g.err = g.s.ConsumeReadBudget(g.state.ID, g.state.Project, store.EstimateReadTokens(string(body))+128, "")
}

func (g *mcpReadGate) denial() *mcp.CallToolResult {
	// Always return the full explanatory envelope. After exhaustion this gate
	// short-circuits before touching any memory data, so repeating this fixed
	// control message carries no unbounded-payload risk; an empty error would
	// leave the agent with nothing actionable to diagnose the refusal.
	return errorWithMeta("read_budget_exhausted", "Memory read budget is unavailable/expired/out of scope/exhausted. Do not retry the same/broader payload. Use a smaller selected index only if allowance remains; otherwise stop and let the host inspect the activation. Changing tool/session parameters cannot reset it.", nil)
}

func (g *mcpReadGate) wrap(name string, next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		action, _ := req.GetArguments()["action"].(string)
		if memorypolicy.MCP(name, action) != memorypolicy.Read {
			return next(ctx, req)
		}
		if g.err != nil {
			return g.denial(), nil
		}
		if g.explicit {
			state, err := g.s.ReadBudget(g.state.ID)
			if err != nil || state.Used >= state.Limit {
				return g.denial(), nil
			}
		}
		// Set progressive defaults only on the public boundary; direct legacy
		// formatter APIs remain available to explicit local operator callers.
		args := map[string]any{}
		for k, v := range req.GetArguments() {
			args[k] = v
		}
		if name == "mem_context" {
			if _, ok := args["mode"]; !ok {
				args["mode"] = "index"
			}
		}
		if name == "mem_search" {
			if _, ok := args["detail"]; !ok {
				args["detail"] = "l0"
			}
			if _, ok := args["strategy"]; !ok {
				args["strategy"] = "hierarchical"
			}
		}
		req.Params.Arguments = args
		result, err := next(ctx, req)
		if err != nil || result == nil {
			return result, err
		}
		if result.IsError {
			// Never repackage an error as a successful result with budget
			// metadata; callers need IsError for a single error path.
			return result, nil
		}
		var envelope map[string]any
		if len(result.Content) > 0 {
			if text, ok := mcp.AsTextContent(result.Content[0]); ok {
				_ = json.Unmarshal([]byte(text.Text), &envelope)
			}
		}
		if envelope == nil {
			envelope = map[string]any{"result": result.Content}
		} else if len(result.Content) > 1 {
			envelope["additional_content"] = result.Content[1:]
		}
		project, _ := envelope["project"].(string)
		if name == "mem_current_project" {
			project = g.state.Project
		} // workspace metadata, not another project's memory body
		state, stateErr := g.budgetFor(project)
		if stateErr != nil {
			return g.denial(), nil
		}
		project = state.Project
		receipt := ""
		if name == "mem_get_observation" && envelope["detail"] == "l2" && !result.IsError {
			if syncID, ok := envelope["observation_sync_id"].(string); ok {
				receipt = fmt.Sprintf("obs:%s@%.0f", syncID, envelope["revision_count"])
			}
		}
		packed, cost, marshalErr := packReadEnvelope(result, envelope, state.Limit)
		if marshalErr != nil {
			return g.denial(), nil
		}

		state, reserveErr := g.s.ConsumeReadBudget(state.ID, project, cost, receipt)
		if reserveErr != nil {
			return g.denial(), nil
		}
		if state.Duplicate {
			envelope = map[string]any{"already_loaded": true, "observation_sync_id": envelope["observation_sync_id"], "revision_count": envelope["revision_count"], "content_omitted": true}
			// Structured content may duplicate the same body; never retain it in a receipt.
			packed, cost, marshalErr = packReadEnvelope(mcp.NewToolResultText(""), envelope, state.Limit)
			if marshalErr != nil {
				return g.denial(), nil
			}
			state, reserveErr = g.s.ConsumeReadBudget(state.ID, project, cost, "")
			if reserveErr != nil {
				return g.denial(), nil
			}
		}
		if name == "mem_search" {
			if entries, ok := envelope["results"].([]any); ok {
				recalled := make([]store.SearchResult, 0, len(entries))
				for _, entry := range entries {
					if row, ok := entry.(map[string]any); ok {
						if id, ok := row["id"].(float64); ok {
							recalled = append(recalled, store.SearchResult{Observation: store.Observation{ID: int64(id)}})
						}
					}
				}
				g.s.RecordSearchRecalls(recalled)
			}
		}
		envelope["read_budget"] = map[string]any{"limit": state.Limit, "used": state.Used, "remaining": state.Limit - state.Used, "estimator": store.ReadBudgetEstimator}
		text, _ := json.Marshal(envelope)
		packed.Content = []mcp.Content{mcp.NewTextContent(string(text))}
		return packed, nil
	}
}

func packReadEnvelope(base *mcp.CallToolResult, envelope map[string]any, limit int) (*mcp.CallToolResult, int, error) {
	envelope["read_budget"] = map[string]any{"limit": limit, "used": limit, "remaining": limit, "estimator": store.ReadBudgetEstimator}
	text, err := json.Marshal(envelope)
	if err != nil {
		return nil, 0, err
	}
	packed := *base
	packed.Content = []mcp.Content{mcp.NewTextContent(string(text))}
	wire, err := json.Marshal(packed)
	if err != nil {
		return nil, 0, err
	}
	return &packed, store.EstimateReadTokens(string(wire)) + 128, nil
}

package store

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/text/unicode/norm"
)

var ErrInvalidType = errors.New("invalid_type")

func NormalizeReadScope(scope string) (string, error) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	switch scope {
	case "", "project", "personal", "global":
		return scope, nil
	default:
		return "", fmt.Errorf("invalid_scope: scope %q must be project, personal or global", QueryPreview(scope))
	}
}

func IsBuiltinObservationType(typ string) bool {
	switch typ {
	case "bugfix", "decision", "discovery", "experience", "learning", "pattern", "preference", "procedure", "session_summary", "checkpoint", "user_fact", "tool_use", "file_change", "command", "file_read", "search", "manual", "architecture":
		return true
	}
	return false
}

func (s *Store) ValidateObservationType(typ, project string) error {
	if typ == "" || IsBuiltinObservationType(typ) {
		return nil
	}
	query := "SELECT EXISTS(SELECT 1 FROM observations WHERE type=?"
	args := []any{typ}
	if project != "" {
		query += " AND lower(project)=?"
		args = append(args, s.canonicalProjectOrRaw(project))
	}
	query += ")"
	var exists bool
	if err := s.db.QueryRow(query, args...).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: type %q is neither a built-in nor a stored custom type in the selected scope", ErrInvalidType, QueryPreview(typ))
	}
	return nil
}

func NormalizeSearchQuery(query string) string { return norm.NFKC.String(query) }

func NormalizeSearchOptions(opts *SearchOptions) error {
	switch opts.MatchMode {
	case "", "all", "any":
	default:
		return fmt.Errorf("%w: invalid match_mode %q: must be \"all\" or \"any\"", ErrInvalidSearchOptions, opts.MatchMode)
	}
	opts.Profile = strings.ToLower(strings.TrimSpace(opts.Profile))
	switch opts.Profile {
	case "", SearchProfileAll, SearchProfileSemantic, SearchProfileOperational:
	default:
		return fmt.Errorf("%w: invalid profile %q: must be \"all\", \"semantic\", or \"operational\"", ErrInvalidSearchOptions, opts.Profile)
	}
	opts.Strategy = strings.ToLower(strings.TrimSpace(opts.Strategy))
	switch opts.Strategy {
	case "", SearchStrategyFlat, SearchStrategyHierarchical, SearchStrategyAuto:
	default:
		return fmt.Errorf("%w: invalid strategy %q: must be \"flat\", \"hierarchical\", or \"auto\"", ErrInvalidSearchOptions, opts.Strategy)
	}
	opts.Detail = strings.ToLower(strings.TrimSpace(opts.Detail))
	if opts.Detail != "" && opts.Detail != SearchDetailL0 && opts.Detail != SearchDetailL1 {
		return fmt.Errorf("%w: invalid detail %q: must be \"l0\" or \"l1\"", ErrInvalidSearchOptions, opts.Detail)
	}
	var err error
	opts.Scope, err = NormalizeReadScope(opts.Scope)
	if err != nil {
		return err
	}
	if opts.TokenBudget < 0 {
		return fmt.Errorf("%w: token_budget must be non-negative", ErrInvalidSearchOptions)
	}
	if opts.MaxDepth < 0 {
		return fmt.Errorf("%w: max_depth must be non-negative", ErrInvalidSearchOptions)
	}
	opts.RootPath, err = normalizeSearchRootPath(opts.RootPath)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidSearchOptions, err)
	}
	if err := normalizeSearchTimeBounds(opts); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidSearchOptions, err)
	}
	return nil
}

func ErrorDescription(message, code string) string {
	for strings.HasPrefix(message, code+": ") {
		message = strings.TrimPrefix(message, code+": ")
	}
	if message == code {
		return strings.ReplaceAll(code, "_", " ")
	}
	return message
}

func ErrorMessage(err error) string {
	if isRetryableSQLiteLockError(err) {
		return "database lock timeout: another process may be writing; retry after the writer finishes"
	}
	message := err.Error()
	for strings.HasPrefix(message, "recalldock: ") {
		message = strings.TrimPrefix(message, "recalldock: ")
	}
	for _, code := range []string{"invalid_arguments", "invalid_query", "query_has_no_terms", "budget_too_small"} {
		message = ErrorDescription(message, code)
	}
	return message
}

func QueryPreview(query string) string {
	runes := []rune(query)
	if len(runes) <= 100 {
		return query
	}
	return string(runes[:100]) + "…"
}

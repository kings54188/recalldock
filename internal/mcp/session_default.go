package mcp

import (
	"strings"
	"sync"

	"recalldock.local/recalldock/internal/store"
)

// sessionProjectDefault remembers the project established by
// mem_session_start(directory=...) for this MCP process. It is intentionally
// process-local: the DSH stdio child often runs with cwd=$HOME, so cwd alone
// cannot identify the workspace the model actually started.
type sessionProjectDefault struct {
	project string
	path    string
}

var sessionDefaults sync.Map // map[*store.Store]sessionProjectDefault

func setSessionProjectDefault(s *store.Store, project, path string) {
	project, _ = store.NormalizeProject(project)
	if s == nil || project == "" {
		return
	}
	sessionDefaults.Store(s, sessionProjectDefault{project: project, path: strings.TrimSpace(path)})
}

func sessionProjectDefaultFor(s *store.Store) (sessionProjectDefault, bool) {
	if s == nil {
		return sessionProjectDefault{}, false
	}
	value, ok := sessionDefaults.Load(s)
	if !ok {
		return sessionProjectDefault{}, false
	}
	def, ok := value.(sessionProjectDefault)
	return def, ok && def.project != ""
}

func clearSessionProjectDefault(s *store.Store) {
	if s != nil {
		sessionDefaults.Delete(s)
	}
}

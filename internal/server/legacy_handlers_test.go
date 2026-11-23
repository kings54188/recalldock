package server

import (
	"net/http"
	"recalldock.local/recalldock/internal/store"
)

// Only legacy parser/business tests use the raw mux. Production Handler/Start
// always use the fail-closed agent policy. There is no runtime bypass flag.
type legacyHandlerHarness struct{ *Server }

func (h *legacyHandlerHarness) Handler() http.Handler { return h.mux }
func newLegacyHandlerTestServer(s *store.Store, port int) *legacyHandlerHarness {
	return &legacyHandlerHarness{New(s, port)}
}

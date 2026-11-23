package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"recalldock.local/recalldock/internal/memorypolicy"
	"strings"
)

func (s *Server) agentPolicy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decision := memorypolicy.HTTP(r.Method, r.URL.Path)
		if decision == memorypolicy.Deny {
			jsonErrorWithFields(w, http.StatusForbidden, "agent direct mutation/raw capture requires local operator review", map[string]any{"code": "operator_review_required"})
			return
		}
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		}
		if decision == memorypolicy.Lifecycle || decision == memorypolicy.Propose {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				jsonError(w, http.StatusBadRequest, "invalid or oversize metadata body")
				return
			}
			if len(body) == 0 {
				body = []byte("{}")
			}
			var fields map[string]any
			if json.Unmarshal(body, &fields) != nil || fields == nil {
				jsonError(w, http.StatusBadRequest, "invalid metadata body")
				return
			}
			if decision == memorypolicy.Lifecycle {
				end := strings.HasSuffix(r.URL.Path, "/end")
				if end {
					if summary, ok := fields["summary"].(string); ok && strings.TrimSpace(summary) != "" {
						jsonErrorWithFields(w, http.StatusForbidden, "session summary belongs in the work ledger", map[string]any{"code": "operator_review_required"})
						return
					}
					if _, exists := fields["id"]; exists {
						jsonError(w, http.StatusBadRequest, "session ID belongs in the URL")
						return
					}
					parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
					fields["id"] = parts[1]
				}
				err = memorypolicy.ValidateLifecycleMetadata(fields, end)
			} else {
				if _, exists := fields["session_id"]; exists {
					jsonError(w, http.StatusBadRequest, "session ID belongs in the URL")
					return
				}
				parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
				fields["session_id"] = parts[1]
				err = memorypolicy.ValidateProposalMetadata(fields)
			}
			if err != nil {
				jsonError(w, http.StatusBadRequest, err.Error())
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}

		if decision == memorypolicy.Read && r.URL.Path != "/health" {
			s.boundedRead(next, w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

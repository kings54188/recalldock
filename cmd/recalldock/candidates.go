package main

import (
	"errors"
	"fmt"
	"os"

	"recalldock.local/recalldock/internal/store"
)

// candidateLookupError keeps an unknown candidate id actionable instead of
// surfacing the driver's "no rows" text.
func candidateLookupError(err error, id string) error {
	if errors.Is(err, store.ErrCandidateNotFound) {
		return fmt.Errorf("candidate %q not found; list pending candidates with: recalldock candidates list --all", id)
	}
	return err
}

// Local operator CLI, deliberately not exposed as an MCP/HTTP approval route.
// Host integrations must still restrict which operator capabilities an agent has.
func cmdCandidates(cfg store.Config) {
	if len(os.Args) >= 3 && os.Args[2] == "policy" {
		options, err := parseCLIOptions(os.Args[3:], []string{"--mode"}, []string{"--all"})
		if err != nil {
			fatal(err)
			return
		}
		mode := options.Values["--mode"]
		if !options.Flags["--all"] || (mode != "manual" && mode != "duplicates" && mode != "checked" && mode != "tiered") {
			fatal(errors.New("use candidates policy --all --mode manual|duplicates|checked|tiered; policy applies store-wide"))
			return
		}
		s, err := storeNew(cfg)
		if err != nil {
			fatal(err)
			return
		}
		defer s.Close()
		if err = s.SetCandidateAdmissionMode(mode); err != nil {
			fatal(err)
			return
		}
		fmt.Println("Candidate admission policy:", mode)
		return
	}
	if len(os.Args) < 3 {
		fatal(errors.New("candidates requires list, show, approve, reject, revive, sweep or policy"))
		return
	}
	sub := os.Args[2]
	optionArgs, positionals, err := splitCLIPositionals(os.Args[3:], []string{"--project", "--expected", "--reason", "--limit", "--status"})
	if err != nil {
		fatal(err)
		return
	}
	options, err := parseCLIOptions(optionArgs, []string{"--project", "--expected", "--reason", "--limit", "--status"}, []string{"--json", "--all"})
	if err != nil {
		fatal(err)
		return
	}
	if len(positionals) > 1 {
		fatal(errors.New("action accepts at most one candidate ID"))
		return
	}
	id := ""
	if len(positionals) == 1 {
		id = positionals[0]
	}
	project, expected := options.Values["--project"], options.Values["--expected"]
	reason, status := options.Values["--reason"], options.Values["--status"]
	all := options.Flags["--all"]
	limit, err := options.integer("--limit", 5, 1, 20)
	if err != nil {
		fatal(err)
		return
	}
	switch {
	case sub == "list":
		if (project == "" && !all) || (project != "" && all) || id != "" || expected != "" || reason != "" {
			fatal(errors.New("list requires --project PROJECT or --all (not both), and no candidate ID"))
			return
		}
		if status != "" && status != "review" && status != "expired" && status != "promoted" && status != "rejected" && status != "all" {
			fatal(errors.New("--status must be review, expired, promoted, rejected or all"))
			return
		}
	case sub == "sweep":
		if id != "" || project != "" || !all || expected != "" || reason != "" {
			fatal(errors.New("sweep requires --all; it applies the configured policy store-wide"))
			return
		}
	case sub == "show" || sub == "approve" || sub == "reject" || sub == "revive":
		if id == "" || project != "" || all {
			fatal(errors.New("action requires exact candidate ID and no project/all"))
			return
		}
	default:
		fatal(errors.New("unknown candidates subcommand"))
		return
	}
	if (sub == "approve" || sub == "reject" || sub == "revive") && (expected == "" || reason == "") {
		fatal(errors.New("operator decision requires --expected SNAPSHOT_SHA256 --reason TEXT"))
		return
	}
	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()
	if sub == "approve" || sub == "reject" || sub == "revive" {
		candidate, err := s.GetMemoryCandidate(id)
		if err != nil {
			fatal(candidateLookupError(err, id))
			return
		}
		if _, err := resolveCLIWriteOwner(s, candidate.Project); err != nil {
			fatal(err)
			return
		}
	}
	var result any
	switch sub {
	case "list":
		queryStatus := status
		if queryStatus == "" {
			queryStatus = store.CandidateStatusReview
		}
		if queryStatus == "all" {
			queryStatus = ""
		}
		candidates, err := s.ListMemoryCandidates(project, queryStatus, limit)
		if err != nil {
			fatal(err)
			return
		}
		// Operator visibility: every inbox view reports the active admission
		// policy so silent mode drift cannot hide behind list output.
		fmt.Fprintf(os.Stderr, "admission policy: %s\n", s.CandidateAdmissionMode())
		index := make([]map[string]any, 0, len(candidates))
		for _, c := range candidates {
			digest, err := store.MemoryCandidateDigest(&c)
			if err != nil {
				fatal(err)
				return
			}
			preview := []rune(c.Statement)
			if len(preview) > 64 {
				preview = preview[:64]
			}
			index = append(index, map[string]any{"id": c.SyncID, "category": c.Category, "project": c.Project, "session_origin": c.SessionOrigin, "preview": string(preview), "expires_at": c.ExpiresAt, "snapshot_sha256": digest})
		}
		result = index
	case "show":
		c, err := s.GetMemoryCandidate(id)
		if err != nil {
			fatal(candidateLookupError(err, id))
			return
		}
		digest, err := store.MemoryCandidateDigest(c)
		if err != nil {
			fatal(err)
			return
		}
		result = map[string]any{"candidate": c, "snapshot_sha256": digest, "evidence_status": "unverified_claims_until_operator_review"}
	case "approve":
		obs, err := s.ApproveMemoryCandidate(id, expected, reason)
		if err != nil {
			fatal(candidateLookupError(err, id))
			return
		}
		result = map[string]any{"candidate_id": id, "observation_id": obs.ID, "observation_sync_id": obs.SyncID, "status": "approved"}
	case "reject":
		if err := s.RejectMemoryCandidateChecked(id, expected, reason); err != nil {
			fatal(candidateLookupError(err, id))
			return
		}
		result = map[string]any{"candidate_id": id, "status": "rejected"}
	case "revive":
		if _, err := s.ReviveMemoryCandidateChecked(id, expected, reason); err != nil {
			fatal(candidateLookupError(err, id))
			return
		}
		result = map[string]any{"candidate_id": id, "status": "review", "expires_at": "extended"}
	case "sweep":
		swept, err := s.AdmitPendingMemoryCandidates()
		if err != nil {
			fatal(err)
			return
		}
		result = swept
	default:
		fatal(errors.New("unknown candidates subcommand"))
		return
	}
	out, err := jsonMarshalIndent(result, "", "  ")
	if err != nil {
		fatal(err)
		return
	}
	fmt.Println(string(out))
}

package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestContextRejectsInvalidScopeInEveryMode(t *testing.T) {
	s := newTestStore(t)
	for _, mode := range []string{ContextModeFull, ContextModeIndex, ContextModeL0, ContextModeL1} {
		if _, err := s.FormatContextBounded(ContextOptions{Scope: "bogus_scope", Mode: mode}); err == nil {
			t.Errorf("mode %s ignored invalid scope", mode)
		}
	}
}

func TestFormatContextBoundedIndexOmitsBodiesAndHonorsBudget(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("bounded-context", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(AddObservationParams{
		SessionID: "bounded-context", Type: "decision", Title: "Bounded decision",
		Content:  "FULL-CONTEXT-MARKER must never be returned by index mode.",
		Abstract: "Compact decision abstract", Overview: "Short overview",
		Project: "engram", TopicKey: "context/bounded",
	}); err != nil {
		t.Fatal(err)
	}

	response, err := s.FormatContextBounded(ContextOptions{Project: "engram", Mode: ContextModeIndex, TokenBudget: 180})
	if err != nil {
		t.Fatal(err)
	}
	if response.Mode != ContextModeIndex || response.Budget.Limit != 180 {
		t.Fatalf("response metadata=%+v", response)
	}
	if response.Budget.Used > response.Budget.Limit {
		t.Fatalf("budget exceeded: %+v", response.Budget)
	}
	if !strings.Contains(response.Text, "observation_id=") || !strings.Contains(response.Text, "Compact decision abstract") {
		t.Fatalf("index omitted metadata: %s", response.Text)
	}
	if strings.Contains(response.Text, "FULL-CONTEXT-MARKER") {
		t.Fatal("index exposed full observation content")
	}
}

func TestFormatContextBoundedL0AndL1AreProgressive(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("bounded-levels", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(AddObservationParams{
		SessionID: "bounded-levels", Type: "decision", Title: "Level decision",
		Content:  "FULL-CONTEXT-MARKER is only available through exact retrieval.",
		Abstract: "L0 abstract", Overview: "L1 overview", Project: "engram",
	}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{ContextModeL0, ContextModeL1} {
		response, err := s.FormatContextBounded(ContextOptions{Project: "engram", Mode: mode, TokenBudget: 500})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(response.Text, "L0 abstract") || strings.Contains(response.Text, "FULL-CONTEXT-MARKER") {
			t.Fatalf("mode=%s returned unsafe content: %s", mode, response.Text)
		}
		if mode == ContextModeL1 && !strings.Contains(response.Text, "L1 overview") {
			t.Fatalf("l1 omitted overview: %s", response.Text)
		}
	}
}

func TestFormatContextBoundedFullRejectsBudget(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.FormatContextBounded(ContextOptions{Mode: ContextModeFull, TokenBudget: 1}); err == nil {
		t.Fatal("full mode should reject a token budget")
	}
}

func TestFormatContextBoundedAccountsForCompleteReturnedText(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("budget-accounting", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(AddObservationParams{
		SessionID: "budget-accounting", Type: "decision", Project: "engram",
		Title: "中文预算与 returned text", Content: strings.Repeat("long content ", 50),
		Abstract: "中文摘要", Overview: strings.Repeat("长概览 overview ", 30),
	}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{ContextModeIndex, ContextModeL0, ContextModeL1} {
		for _, budget := range []int{1, 12, 20, 25, 40, 60, 90, 180, 500} {
			t.Run(fmt.Sprintf("%s/%d", mode, budget), func(t *testing.T) {
				response, err := s.FormatContextBounded(ContextOptions{Project: "engram", Mode: mode, TokenBudget: budget})
				if err != nil {
					t.Fatal(err)
				}
				cost := 0
				if response.Text != "" {
					cost = estimateSearchTokens(response.Text)
				}
				if response.Budget.Used != cost || (cost > budget && !response.Budget.BudgetLimited) {
					t.Fatalf("actual cost=%d budget=%+v text=%q", cost, response.Budget, response.Text)
				}
				if response.Text == "" && (!response.Budget.Truncated || response.Budget.Entries != 0) {
					t.Fatalf("empty output must report truncation with no entries: %+v", response.Budget)
				}
				if response.Budget.BudgetLimited && response.Budget.Entries == 0 {
					t.Fatalf("budget-limited output must include one entry: %+v", response.Budget)
				}
			})
		}
	}
}

func TestFormatContextBoundedRendersLocalTimezone(t *testing.T) {
	s := newTestStore(t)
	t.Setenv("RECALLDOCK_TIMEZONE", "Asia/Shanghai")
	if err := s.CreateSession("bounded-tz", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(AddObservationParams{
		SessionID: "bounded-tz", Type: "decision", Title: "TZ decision",
		Content: "timezone rendering contract", Project: "engram",
	}); err != nil {
		t.Fatal(err)
	}

	var utcCreated, utcStarted string
	if err := s.DB().QueryRow(`SELECT created_at FROM observations ORDER BY id DESC LIMIT 1`).Scan(&utcCreated); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT started_at FROM sessions WHERE id='bounded-tz'`).Scan(&utcStarted); err != nil {
		t.Fatal(err)
	}
	shift := func(utc string) string {
		parsed, err := time.Parse("2006-01-02 15:04:05", utc)
		if err != nil {
			t.Fatal(err)
		}
		return parsed.Add(8 * time.Hour).Format("2006-01-02 15:04:05")
	}
	expectedCreated, expectedStarted := shift(utcCreated), shift(utcStarted)

	response, err := s.FormatContextBounded(ContextOptions{Project: "engram", Mode: ContextModeIndex, TokenBudget: 1200})
	if err != nil {
		t.Fatal(err)
	}
	// 0.1.7 bug: bounded context rendered raw UTC while search rendered local,
	// so the same record showed two different wall clocks 8h apart.
	if !strings.Contains(response.Text, "created="+expectedCreated) {
		t.Fatalf("observation created should render %s (Asia/Shanghai), got:\n%s", expectedCreated, response.Text)
	}
	if !strings.Contains(response.Text, "started="+expectedStarted) {
		t.Fatalf("session started should render %s (Asia/Shanghai), got:\n%s", expectedStarted, response.Text)
	}
	if expectedCreated != utcCreated && strings.Contains(response.Text, "created="+utcCreated) {
		t.Fatalf("index leaked raw UTC timestamp %s", utcCreated)
	}
}

func TestFormatContextFullKeepsSessionIDAndDirectory(t *testing.T) {
	s := newTestStore(t)
	root := t.TempDir()
	if err := s.CreateSession("session-full-identity", "engram", root); err != nil {
		t.Fatal(err)
	}
	text, err := s.FormatContext("engram", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "session-full-identity") || !strings.Contains(text, root) {
		t.Fatalf("full context lost session identity/directory: %s", text)
	}
	if strings.Contains(text, "- **engram**") {
		t.Fatalf("full context still prints project instead of session: %s", text)
	}
}

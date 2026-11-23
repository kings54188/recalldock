package store

import (
	"sync"
	"testing"
)

func TestReadBudgetSharedAcrossStoresAndReceipts(t *testing.T) {
	s := newTestStore(t)
	budget, err := s.CreateReadBudget("", 1000)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	first, err := s.ConsumeReadBudget(budget.ID, "", 600, "obs:one@1")
	if err != nil || first.Duplicate {
		t.Fatalf("first: %+v %v", first, err)
	}
	duplicate, err := second.ConsumeReadBudget(budget.ID, "", 600, "obs:one@1")
	if err != nil || !duplicate.Duplicate || duplicate.Used != 600 {
		t.Fatalf("duplicate across process/store: %+v %v", duplicate, err)
	}
	if _, err := second.ConsumeReadBudget(budget.ID, "", 500, "obs:one@2"); err == nil {
		t.Fatal("new revision bypassed shared limit")
	}
	state, _ := s.ReadBudget(budget.ID)
	if state.Used != 600 {
		t.Fatal("failed read charged or reset budget")
	}
	if _, err := s.ReadBudget("unknown-budget"); err == nil {
		t.Fatal("unknown host activation must not auto-reset")
	}
}

func TestConcurrentReadBudgetReservationsDoNotOverspend(t *testing.T) {
	s := newTestStore(t)
	budget, _ := s.CreateReadBudget("", 1000)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.ConsumeReadBudget(budget.ID, "", 300, "") }()
	}
	wg.Wait()
	state, err := s.ReadBudget(budget.ID)
	if err != nil || state.Used != 900 {
		t.Fatalf("used=%d err=%v", state.Used, err)
	}
}

func TestExpiredReadBudgetAndWrongScopeCannotBeConsumed(t *testing.T) {
	s := newTestStore(t)
	budget, err := s.CreateReadBudget("one", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeReadBudget(budget.ID, "two", 1, "receipt"); err == nil {
		t.Fatal("project scope widened")
	}
	if _, err := s.DB().Exec(`UPDATE read_budgets SET expires_at='2000-01-01 00:00:00' WHERE id=?`, budget.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeReadBudget(budget.ID, "one", 1, "receipt"); err == nil {
		t.Fatal("expired budget accepted")
	}
	if _, err := s.ReadBudget(budget.ID); err == nil {
		t.Fatal("expired activation returned as live")
	}
	if _, err := s.CreateReadBudget("", MaxReadBudget+1); err == nil {
		t.Fatal("unbounded host allowance accepted")
	}
}

func TestDefaultReadBudgetMatchesMax(t *testing.T) {
	s := newTestStore(t)
	budget, err := s.CreateReadBudget("", 0)
	if err != nil {
		t.Fatal(err)
	}
	// The 0.1.7 default (8000) exhausted after 5-7 bounded reads with the
	// startup catalog reserve; the default now covers a normal 24h window.
	if budget.Limit != MaxReadBudget {
		t.Fatalf("default read budget = %d, want %d", budget.Limit, MaxReadBudget)
	}
}

func TestConsumeReadBudgetGarbageCollectsExpiredRows(t *testing.T) {
	s := newTestStore(t)
	live, err := s.CreateReadBudget("", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO read_budgets(id,project,token_limit,used,expires_at) VALUES('read-expired-one','',1000,0,'2000-01-01 00:00:00'),('read-expired-two','',1000,0,'2000-01-02 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeReadBudget(live.ID, "", 10, ""); err != nil {
		t.Fatalf("consume live budget: %v", err)
	}
	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM read_budgets`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expired rows not garbage-collected on consume: %d rows remain", count)
	}
}

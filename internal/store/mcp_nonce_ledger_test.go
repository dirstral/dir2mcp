package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newNonceTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	st := NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("Init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestMCPNonceLedger_ConsumptionIsMonotonic proves a durably-consumed nonce is
// never downgraded to a reservation by a later write for the same nonce, and its
// request/execution binding + expiry are preserved — so a replay that reaches
// storage cannot weaken the single-use ledger.
func TestMCPNonceLedger_ConsumptionIsMonotonic(t *testing.T) {
	st := newNonceTestStore(t)
	ctx := context.Background()
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)

	// Reserve, then durably consume for request A.
	if err := st.UpsertMCPNonceLedger(ctx, MCPNonceLedgerRecord{
		Nonce: "n1", RequestKey: "reqA", ExecutionKey: "n1:reqA", Consumed: false, ExpiresAt: exp, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("reserve upsert: %v", err)
	}
	if err := st.UpsertMCPNonceLedger(ctx, MCPNonceLedgerRecord{
		Nonce: "n1", RequestKey: "reqA", ExecutionKey: "n1:reqA", Consumed: true, ExpiresAt: exp, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("consume upsert: %v", err)
	}

	// Replay attempt: same nonce, DIFFERENT request, consumed=false. Must NOT
	// downgrade the consumed record or rebind it.
	if err := st.UpsertMCPNonceLedger(ctx, MCPNonceLedgerRecord{
		Nonce: "n1", RequestKey: "reqB", ExecutionKey: "n1:reqB", Consumed: false, ExpiresAt: exp.Add(-time.Hour), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("replay upsert: %v", err)
	}

	rec, ok, err := st.GetMCPNonceLedger(ctx, "n1")
	if err != nil || !ok {
		t.Fatalf("GetMCPNonceLedger ok=%v err=%v", ok, err)
	}
	if !rec.Consumed {
		t.Fatalf("consumed was downgraded to false by a later reservation")
	}
	if rec.RequestKey != "reqA" || rec.ExecutionKey != "n1:reqA" {
		t.Fatalf("consumed binding was clobbered: request=%q exec=%q", rec.RequestKey, rec.ExecutionKey)
	}
	if rec.ExpiresAt.Before(exp) {
		t.Fatalf("consumed expiry was shortened: got %v want >= %v", rec.ExpiresAt, exp)
	}
}

// TestMCPNonceLedger_GetMissing verifies a genuinely-unseen nonce reports not
// found (so the enforcement layer treats it as proceed, not a replay).
func TestMCPNonceLedger_GetMissing(t *testing.T) {
	st := newNonceTestStore(t)
	if _, ok, err := st.GetMCPNonceLedger(context.Background(), "never-seen"); err != nil || ok {
		t.Fatalf("GetMCPNonceLedger(missing) ok=%v err=%v want ok=false", ok, err)
	}
}

// TestMCPNonceLedger_DeleteExpired reclaims only rows at/before the cutoff,
// directly in the DB — the sweep path that reclaims cap-evicted persisted rows.
func TestMCPNonceLedger_DeleteExpired(t *testing.T) {
	st := newNonceTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	must := func(rec MCPNonceLedgerRecord) {
		if err := st.UpsertMCPNonceLedger(ctx, rec); err != nil {
			t.Fatalf("upsert %s: %v", rec.Nonce, err)
		}
	}
	must(MCPNonceLedgerRecord{Nonce: "old", RequestKey: "r", Consumed: true, ExpiresAt: now.Add(-time.Minute), UpdatedAt: now})
	must(MCPNonceLedgerRecord{Nonce: "fresh", RequestKey: "r", Consumed: true, ExpiresAt: now.Add(time.Hour), UpdatedAt: now})

	if err := st.DeleteExpiredMCPNonceLedger(ctx, now.Unix()); err != nil {
		t.Fatalf("DeleteExpiredMCPNonceLedger: %v", err)
	}

	if _, ok, _ := st.GetMCPNonceLedger(ctx, "old"); ok {
		t.Fatalf("expired nonce was not swept")
	}
	if _, ok, _ := st.GetMCPNonceLedger(ctx, "fresh"); !ok {
		t.Fatalf("unexpired nonce was incorrectly swept")
	}
}

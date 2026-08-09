package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestMCPPaymentOutcomeRoundTripsExpiry pins the store half of issue #697. The
// payment outcome carries a nonce-aligned expiry, and the row must keep it, so a
// restart restores the real expiry instead of falling back to a fixed TTL on
// UpdatedAt. A row written without an expiry must still read back with a zero
// time, which is what an existing database holds after the migration.
func TestMCPPaymentOutcomeRoundTripsExpiry(t *testing.T) {
	ctx := context.Background()
	st := NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = st.Close() }()

	updatedAt := time.Now().UTC().Truncate(time.Second)
	expiresAt := updatedAt.Add(15 * time.Minute)

	withExpiry := MCPPaymentOutcomeRecord{
		ExecutionKey: "nonce-a:request-a",
		StatusCode:   200,
		ResultJSON:   `{"content":[]}`,
		Settled:      true,
		UpdatedAt:    updatedAt,
		ExpiresAt:    expiresAt,
	}
	withoutExpiry := MCPPaymentOutcomeRecord{
		ExecutionKey: "nonce-b:request-b",
		StatusCode:   200,
		ResultJSON:   `{"content":[]}`,
		Settled:      true,
		UpdatedAt:    updatedAt,
	}
	for _, rec := range []MCPPaymentOutcomeRecord{withExpiry, withoutExpiry} {
		if err := st.UpsertMCPPaymentOutcome(ctx, rec); err != nil {
			t.Fatalf("upsert %s: %v", rec.ExecutionKey, err)
		}
	}

	byKey := listPaymentOutcomesByKey(t, st)
	got, ok := byKey[withExpiry.ExecutionKey]
	if !ok {
		t.Fatalf("row %s is missing", withExpiry.ExecutionKey)
	}
	if !got.ExpiresAt.Equal(expiresAt) {
		t.Errorf("ExpiresAt=%v, want %v", got.ExpiresAt, expiresAt)
	}

	got, ok = byKey[withoutExpiry.ExecutionKey]
	if !ok {
		t.Fatalf("row %s is missing", withoutExpiry.ExecutionKey)
	}
	if !got.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt=%v, want a zero time for a row written without one", got.ExpiresAt)
	}

	// An update must move the expiry too, because settlement rewrites the row.
	moved := withExpiry
	moved.ExpiresAt = expiresAt.Add(10 * time.Minute)
	if err := st.UpsertMCPPaymentOutcome(ctx, moved); err != nil {
		t.Fatalf("re-upsert %s: %v", moved.ExecutionKey, err)
	}
	byKey = listPaymentOutcomesByKey(t, st)
	if got := byKey[moved.ExecutionKey]; !got.ExpiresAt.Equal(moved.ExpiresAt) {
		t.Errorf("ExpiresAt after update=%v, want %v", got.ExpiresAt, moved.ExpiresAt)
	}
}

// TestMCPPaymentOutcomeMigratesLegacyRow proves the additive migration path:
// a database whose mcp_payment_outcomes table predates expires_unix gets the
// column, and the rows it already held read back with a zero expiry.
func TestMCPPaymentOutcomeMigratesLegacyRow(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "meta.sqlite")

	legacy := NewSQLiteStore(path)
	if err := legacy.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	db, err := legacy.ensureDB(ctx)
	if err != nil {
		t.Fatalf("ensureDB: %v", err)
	}
	// Recreate the pre-#697 table shape and put one row in it.
	stmts := []string{
		`DROP TABLE mcp_payment_outcomes`,
		`CREATE TABLE mcp_payment_outcomes (
		  execution_key TEXT PRIMARY KEY,
		  status_code INTEGER NOT NULL,
		  result_json TEXT NOT NULL DEFAULT '',
		  rpc_error_json TEXT NOT NULL DEFAULT '',
		  requires_settle INTEGER NOT NULL DEFAULT 0,
		  settled INTEGER NOT NULL DEFAULT 0,
		  payment_response TEXT NOT NULL DEFAULT '',
		  updated_unix INTEGER NOT NULL
		)`,
		`INSERT INTO mcp_payment_outcomes(execution_key, status_code, result_json, settled, updated_unix)
		 VALUES('legacy-key', 200, '{"content":[]}', 1, 1700000000)`,
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("prepare legacy schema: %v", err)
		}
	}
	legacy.ReleaseDB()
	if err := legacy.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-open. Init runs the additive migration on the legacy table.
	upgraded := NewSQLiteStore(path)
	if err := upgraded.Init(ctx); err != nil {
		t.Fatalf("re-Init: %v", err)
	}
	defer func() { _ = upgraded.Close() }()

	byKey := listPaymentOutcomesByKey(t, upgraded)
	got, ok := byKey["legacy-key"]
	if !ok {
		t.Fatalf("the legacy row did not survive the migration")
	}
	if !got.ExpiresAt.IsZero() {
		t.Errorf("legacy ExpiresAt=%v, want a zero time", got.ExpiresAt)
	}
	if !got.Settled || got.StatusCode != 200 {
		t.Errorf("legacy row changed: settled=%v status=%d", got.Settled, got.StatusCode)
	}
}

func listPaymentOutcomesByKey(t *testing.T, st *SQLiteStore) map[string]MCPPaymentOutcomeRecord {
	t.Helper()
	records, err := st.ListMCPPaymentOutcomes(context.Background())
	if err != nil {
		t.Fatalf("list payment outcomes: %v", err)
	}
	byKey := make(map[string]MCPPaymentOutcomeRecord, len(records))
	for _, rec := range records {
		byKey[rec.ExecutionKey] = rec
	}
	return byKey
}

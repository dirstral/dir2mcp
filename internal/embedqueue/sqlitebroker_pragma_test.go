package embedqueue

import (
	"context"
	"path/filepath"
	"testing"
)

// TestSQLiteBroker_PragmasConfigured pins F5 (issue #433): when the broker opens
// its own DB it configures the connection with busy_timeout then journal_mode=WAL
// (same order/values as internal/store/sqlite_store.go) so the reclaim-write-first
// Lease waits on contention under the multi-worker pool instead of failing
// immediately with "database is locked". This is a white-box test because
// busy_timeout is connection-local and only observable on the broker's own handle.
func TestSQLiteBroker_PragmasConfigured(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "queue.db")

	b, err := NewSQLiteBroker(ctx, path, 5)
	if err != nil {
		t.Fatalf("NewSQLiteBroker: %v", err)
	}
	defer func() { _ = b.Close() }()

	var busyTimeout int
	if err := b.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busyTimeout)
	}

	var journalMode string
	if err := b.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want \"wal\"", journalMode)
	}
}

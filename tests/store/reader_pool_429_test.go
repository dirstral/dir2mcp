package tests

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// #429 F11: the store served every query through a single-connection pool, so a
// read queued behind any in-flight write. During ingest that collapsed query
// throughput: `ask`/`search` waited on the embedding worker's writes.
//
// These tests pin the two properties that make the read pool safe rather than
// merely faster: reads proceed while a write transaction is open, and the
// per-connection pragmas that used to "stick" only because there was one
// connection are now in effect on every pooled connection.

func newTestStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.db"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestReadPool_ReadProceedsDuringOpenWrite is the regression that matters: hold
// a write transaction open and require a read to finish anyway. On the
// single-connection pool this blocks until the write commits, so the read only
// completes inside the deadline because it runs on a different connection.
func TestReadPool_ReadProceedsDuringOpenWrite(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	if err := st.UpsertDocument(ctx, model.Document{RelPath: "a.txt", DocType: "text"}); err != nil {
		t.Fatalf("seed document: %v", err)
	}

	db, _, _, err := st.HandlesForTest(ctx)
	if err != nil {
		t.Fatalf("HandlesForTest: %v", err)
	}

	// Occupy the single writer connection with an open transaction.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin write tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE documents SET title = ? WHERE rel_path = ?`, "held", "a.txt"); err != nil {
		t.Fatalf("write inside tx: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_, rerr := st.GetDocumentByPath(readCtx, "a.txt")
		done <- rerr
	}()

	select {
	case rerr := <-done:
		if rerr != nil {
			t.Fatalf("read during open write tx failed: %v", rerr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read blocked behind an open write transaction; the read pool is not in use")
	}
}

// TestReadPool_PragmasApplyToEveryConnection guards the subtle half of the
// change. busy_timeout and foreign_keys are per-connection and were previously
// applied with one ExecContext each, which was only correct because
// SetMaxOpenConns(1) guaranteed a single connection. They now ride in the DSN;
// if that regressed, pooled connections would silently run with foreign_keys
// OFF, leaving the #405 ON DELETE CASCADE constraints inert.
func TestReadPool_PragmasApplyToEveryConnection(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	_, rdb, poolSize, err := st.HandlesForTest(ctx)
	if err != nil {
		t.Fatalf("HandlesForTest: %v", err)
	}
	if rdb == nil {
		t.Fatal("read pool was not opened")
	}

	// Hold several connections open simultaneously so each is a distinct one.
	conns := make([]interface{ Close() error }, 0, poolSize)
	for i := 0; i < poolSize; i++ {
		c, err := rdb.Conn(ctx)
		if err != nil {
			t.Fatalf("open pooled conn %d: %v", i, err)
		}
		conns = append(conns, c)

		var foreignKeys, busyTimeout int
		if err := c.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
			t.Fatalf("conn %d: read foreign_keys: %v", i, err)
		}
		if err := c.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
			t.Fatalf("conn %d: read busy_timeout: %v", i, err)
		}
		if foreignKeys != 1 {
			t.Errorf("conn %d: foreign_keys = %d, want 1 (#405 cascades would be inert)", i, foreignKeys)
		}
		if busyTimeout != 5000 {
			t.Errorf("conn %d: busy_timeout = %d, want 5000", i, busyTimeout)
		}
	}
	for _, c := range conns {
		_ = c.Close()
	}
}

// TestReadPool_ClosedStoreClosesBothHandles pins that Close tears down the read
// pool too, so a leaked pool cannot keep file descriptors (or the -wal file)
// alive after the store is closed.
func TestReadPool_ClosedStoreClosesBothHandles(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.db"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	_, rdb, _, err := st.HandlesForTest(ctx)
	if err != nil {
		t.Fatalf("HandlesForTest: %v", err)
	}
	if rdb == nil {
		t.Fatal("read pool was not opened")
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := rdb.PingContext(ctx); err == nil {
		t.Error("read pool still usable after Close; it was not closed")
	}
}

package embedqueue_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"

	"github.com/dirstral/dir2mcp/internal/embedqueue"
)

// holdSharedLock opens a second handle on path and keeps a read transaction
// open on it. In rollback-journal mode that read holds a SHARED lock, so a
// COMMIT on another connection fails at once with SQLITE_BUSY when that
// connection has no busy timeout. The returned func ends the read.
func holdSharedLock(t *testing.T, path string) func() {
	t.Helper()
	ctx := context.Background()
	reader, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	tx, err := reader.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("reader begin: %v", err)
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM embed_jobs`).Scan(&n); err != nil {
		t.Fatalf("reader select: %v", err)
	}
	return func() { _ = tx.Rollback() }
}

// rollbackJournalBroker builds a SQLite broker over a rollback-journal database
// with no busy timeout and one pooled connection, so a lock that another
// handle holds makes COMMIT fail at once, and every broker call uses the
// connection that the failed COMMIT used.
func rollbackJournalBroker(t *testing.T) (*embedqueue.SQLiteBroker, string) {
	t.Helper()
	path := filepath.Join(storeTempDir(t), "queue.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	broker, err := embedqueue.NewSQLiteBrokerWithDB(context.Background(), db, 3)
	if err != nil {
		t.Fatalf("NewSQLiteBrokerWithDB: %v", err)
	}
	return broker, path
}

// TestSQLiteBroker_FailedLeaseCommitDoesNotStrandTheConnection is the #1188
// regression. A lease COMMIT that fails leaves the SQLite transaction open, and
// database/sql gives the connection back to the pool. Before the fix, every
// later lease on that connection failed with "cannot start a transaction
// within a transaction", and the worker stalled.
func TestSQLiteBroker_FailedLeaseCommitDoesNotStrandTheConnection(t *testing.T) {
	ctx := context.Background()
	broker, path := rollbackJournalBroker(t)
	for _, id := range []uint64{1, 2} {
		if err := broker.Enqueue(ctx, sampleJob(id)); err != nil {
			t.Fatalf("Enqueue %d: %v", id, err)
		}
	}

	release := holdSharedLock(t, path)
	_, err := broker.Lease(ctx, time.Minute)
	release()
	if err == nil || !strings.Contains(err.Error(), "lease commit") {
		t.Fatalf("precondition: the lease under a held SHARED lock must fail at COMMIT, got %v", err)
	}

	lease, err := broker.Lease(ctx, time.Minute)
	if err != nil {
		t.Fatalf("the lease after a failed COMMIT must succeed: %v", err)
	}
	// The failed lease rolled back, so its claim did not stick: the oldest job
	// is still the next one.
	if lease.Job.ChunkID != 1 || lease.Attempts != 1 {
		t.Fatalf("lease = chunk %d attempt %d, want chunk 1 attempt 1", lease.Job.ChunkID, lease.Attempts)
	}
}

// TestSQLiteBroker_NackWaitsForAWriteLock is the Nack half of #1188, on the
// broker that production opens (WAL, busy timeout). Nack used to read the row
// in a transaction and then write it. SQLite does not run the busy handler for
// a reader that asks to write, so a Nack that met another connection's write
// failed at once, the worker dropped the error, and the job stayed in flight
// until its lease expired. A Nack must wait for the lock like any other write.
func TestSQLiteBroker_NackWaitsForAWriteLock(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(storeTempDir(t), "queue.sqlite")
	broker, err := embedqueue.NewSQLiteBroker(ctx, path, 3)
	if err != nil {
		t.Fatalf("NewSQLiteBroker: %v", err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	if err := broker.Enqueue(ctx, sampleJob(1)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	lease, err := broker.Lease(ctx, time.Minute)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}

	// Another handle, as a second worker on the shared queue file, holds the
	// write lock for a short time.
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open other: %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })
	conn, err := other.Conn(ctx)
	if err != nil {
		t.Fatalf("other conn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("other BEGIN IMMEDIATE: %v", err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		_, err := conn.ExecContext(ctx, `COMMIT`)
		released <- err
	}()

	nackErr := broker.Nack(ctx, lease.Token, 0)
	if err := <-released; err != nil {
		t.Fatalf("other COMMIT: %v", err)
	}
	if nackErr != nil {
		t.Fatalf("a Nack that meets another writer must wait for the lock, got %v", nackErr)
	}
	again, err := broker.Lease(ctx, time.Minute)
	if err != nil {
		t.Fatalf("the requeued job must lease again: %v", err)
	}
	if again.Job.ChunkID != 1 || again.Attempts != 2 {
		t.Fatalf("lease = chunk %d attempt %d, want chunk 1 attempt 2", again.Job.ChunkID, again.Attempts)
	}
}

// TestSQLiteBroker_NackDeadLettersAtMaxAttempts pins that the single-statement
// Nack keeps both outcomes: a requeue below the attempt budget, and a dead
// letter on the last delivery.
func TestSQLiteBroker_NackDeadLettersAtMaxAttempts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(storeTempDir(t), "queue.sqlite")
	broker, err := embedqueue.NewSQLiteBroker(ctx, path, 2)
	if err != nil {
		t.Fatalf("NewSQLiteBroker: %v", err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	if err := broker.Enqueue(ctx, sampleJob(1)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		lease, err := broker.Lease(ctx, time.Minute)
		if err != nil {
			t.Fatalf("Lease %d: %v", attempt, err)
		}
		if err := broker.Nack(ctx, lease.Token, 0); err != nil {
			t.Fatalf("Nack %d: %v", attempt, err)
		}
	}
	st, err := broker.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Pending != 0 || st.InFlight != 0 || st.DeadLettered != 1 {
		t.Fatalf("after 2 of 2 deliveries the job must be dead-lettered: %+v", st)
	}
	// A Nack with a token that no longer leases anything changes nothing.
	if err := broker.Nack(ctx, "sql-0-unknown", 0); err != nil {
		t.Fatalf("Nack with an unknown token: %v", err)
	}
}

// cancelMidStep hands a cancel func to the zz1188_cancel_mid_step SQL function.
var cancelMidStep = make(chan context.CancelFunc, 1)

func init() {
	// The function ends the query's context while the query's first step runs,
	// then waits so that the driver's watcher sees the cancel before the step
	// returns its row.
	sqlite.MustRegisterScalarFunction("zz1188_cancel_mid_step", 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		select {
		case cancel := <-cancelMidStep:
			cancel()
			time.Sleep(200 * time.Millisecond)
		default:
		}
		return int64(1), nil
	})
}

// TestSQLiteDriver_CancelledQueryReleasesTheDatabaseOnClose pins the driver
// half of #1188. modernc.org/sqlite before v1.40.1 dropped the rows of a query
// whose context ended during its first step without finalizing the statement.
// The connection then could not close: sqlite3_close_v2 kept it open as a
// zombie that held its file handle and its lock. That is the meta.db that
// Windows CI could not remove at cleanup, and the SQLITE_BUSY that a broker
// saw after a worker's lease was cancelled. The broker and the metadata store
// share the driver, so this test guards both.
func TestSQLiteDriver_CancelledQueryReleasesTheDatabaseOnClose(t *testing.T) {
	path := filepath.Join(storeTempDir(t), "meta.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE t(x)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelMidStep <- cancel
	var v int64
	err = db.QueryRowContext(ctx, `SELECT zz1188_cancel_mid_step()`).Scan(&v)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("precondition: the query must end with its cancelled context, got %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A switch out of WAL needs exclusive access, so it fails with
	// SQLITE_BUSY while any connection to the file stays open.
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = other.Close() }()
	if _, err := other.Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		t.Fatalf("a connection stayed open after Close: %v", err)
	}
}

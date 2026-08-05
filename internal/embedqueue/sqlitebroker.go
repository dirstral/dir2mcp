package embedqueue

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (CGO_ENABLED=0, SPEC §6.5)
)

// SQLiteBroker is the default persistent Broker (SPEC §8.7.4): a dependency-free,
// pure-Go (CGO_ENABLED=0, §6.5) broker backed by a SQLite table reusing the same
// modernc.org/sqlite driver the metadata store already uses. It survives a
// coordinator restart (jobs are durable) and, when pointed at a shared SQLite
// file on an NFS-reachable path, lets a single-host-plus-NFS worker pool drain a
// queue without an external broker. A cross-machine pool over a real network
// broker plugs in behind the Broker interface instead.
//
// Lease/visibility, redelivery, and dead-lettering are implemented with the
// notBefore/deadline/attempts columns; an expired lease is reclaimed lazily on
// the next Lease (SPEC §8.7.3 lease expiry).
type SQLiteBroker struct {
	db          *sql.DB
	ownsDB      bool
	maxAttempts int
	now         func() time.Time
}

var _ CorpusScopedBroker = (*SQLiteBroker)(nil)

// NewSQLiteBroker opens (or creates) a SQLite-backed queue at path. maxAttempts
// bounds redelivery before dead-lettering (non-positive defaults to 5).
func NewSQLiteBroker(ctx context.Context, path string, maxAttempts int) (*SQLiteBroker, error) {
	// busy_timeout is a PER-CONNECTION setting, so it travels in the DSN and is
	// in effect the moment database/sql opens any connection. Setting it with a
	// single ExecContext (as this did) lands on whichever pooled connection
	// served that one call: with several workers leasing concurrently, the pool
	// opens further connections that never got it, and their reclaim-write-first
	// Lease fails IMMEDIATELY with "database is locked" instead of waiting. That
	// is the same defect #429 F11 fixed in internal/store, and the broker is
	// where it bites hardest — a failed Ack under contention leaves the job
	// in-flight until its lease expires. Surfaced by the multi-worker shared-queue
	// test added for #708.
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("embedqueue: open sqlite broker: %w", err)
	}
	// Every broker operation (Enqueue/Lease/Ack/Nack) writes, so concurrent
	// connections to one SQLite file can only contend. Serializing them through a
	// single connection is what internal/store does for the same reason.
	db.SetMaxOpenConns(1)
	// journal_mode is persistent and database-level, so one connection setting it
	// is enough; it must come AFTER busy_timeout, because the switch itself can
	// return SQLITE_BUSY when another process holds the lock.
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL;`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("embedqueue: configure sqlite broker: %w", err)
	}
	b, err := newSQLiteBroker(ctx, db, true, maxAttempts)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return b, nil
}

// NewSQLiteBrokerWithDB wraps an already-open *sql.DB (e.g. shared with the
// metadata store). The caller retains ownership of db; Close does not close it.
func NewSQLiteBrokerWithDB(ctx context.Context, db *sql.DB, maxAttempts int) (*SQLiteBroker, error) {
	return newSQLiteBroker(ctx, db, false, maxAttempts)
}

func newSQLiteBroker(ctx context.Context, db *sql.DB, ownsDB bool, maxAttempts int) (*SQLiteBroker, error) {
	if db == nil {
		return nil, errors.New("embedqueue: sqlite broker requires a non-nil *sql.DB")
	}
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	b := &SQLiteBroker{db: db, ownsDB: ownsDB, maxAttempts: maxAttempts, now: time.Now}
	if err := b.migrate(ctx); err != nil {
		return nil, err
	}
	return b, nil
}

// SetClock overrides the broker's clock for deterministic lease-expiry tests.
func (b *SQLiteBroker) SetClock(now func() time.Time) {
	if now != nil {
		b.now = now
	}
}

func (b *SQLiteBroker) migrate(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS embed_jobs (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  corpus_id     TEXT NOT NULL,
  source        TEXT NOT NULL,
  chunk_id      INTEGER NOT NULL,
  index_kind    TEXT NOT NULL,
  text_hash     TEXT NOT NULL,
  modality      TEXT NOT NULL,
  rel_path      TEXT NOT NULL,
  span_kind     TEXT NOT NULL,
  span_page     INTEGER NOT NULL,
  span_start_ms INTEGER NOT NULL,
  span_end_ms   INTEGER NOT NULL,
  embed_identity TEXT NOT NULL,
  attempts      INTEGER NOT NULL DEFAULT 0,
  state         TEXT NOT NULL DEFAULT 'pending',  -- pending | inflight | dead
  token         TEXT,
  deadline_ns   INTEGER NOT NULL DEFAULT 0,
  not_before_ns INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_embed_jobs_state ON embed_jobs(state, not_before_ns);
CREATE INDEX IF NOT EXISTS idx_embed_jobs_token ON embed_jobs(token);
-- Serves both corpus-scoped claiming (LeaseForCorpus) and the corpus-scoped
-- dedup probe Enqueue runs on every coordinator tick (#708).
CREATE INDEX IF NOT EXISTS idx_embed_jobs_corpus ON embed_jobs(corpus_id, state, not_before_ns);
CREATE INDEX IF NOT EXISTS idx_embed_jobs_dedup ON embed_jobs(corpus_id, chunk_id, index_kind, state);
`
	if _, err := b.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("embedqueue: migrate broker schema: %w", err)
	}
	return nil
}

// newLeaseToken builds a globally unique lease token: a random 128-bit suffix so
// tokens never collide across SQLiteBroker instances sharing one DB file. Without
// this, two instances reusing a row id + a process-local counter could mint the
// same token, letting a stale Ack/Nack from one instance's expired lease match a
// different instance's current lease for the same job (SPEC §8.7.3). The row id
// is included only for readability. crypto/rand failure is surfaced, never masked.
func newLeaseToken(id int64) (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("embedqueue: generate lease token: %w", err)
	}
	return fmt.Sprintf("sql-%d-%s", id, hex.EncodeToString(buf[:])), nil
}

// Enqueue inserts a job in the pending state.
func (b *SQLiteBroker) Enqueue(ctx context.Context, job Job) error {
	if err := job.Validate(); err != nil {
		return err
	}
	// Dedup: do not insert when a LIVE (pending or in-flight) job already exists
	// for this corpus_id+chunk_id+index_kind, so re-enqueuing the same
	// still-pending head across coordinator ticks cannot pile up duplicate jobs
	// (SPEC §8.7.3). A dead-lettered job does NOT block a fresh enqueue (a later
	// retry is allowed).
	//
	// corpus_id belongs in the key because chunk ids are per-corpus SQLite
	// rowids: two corpora pointed at one broker file both have a chunk 1, and
	// without the corpus term the second corpus's enqueue was silently swallowed
	// by the first corpus's live job (#708).
	_, err := b.db.ExecContext(ctx, `
INSERT INTO embed_jobs(corpus_id, source, chunk_id, index_kind, text_hash, modality,
  rel_path, span_kind, span_page, span_start_ms, span_end_ms, embed_identity, state)
SELECT ?,?,?,?,?,?,?,?,?,?,?,?, 'pending'
WHERE NOT EXISTS (
  SELECT 1 FROM embed_jobs
   WHERE corpus_id = ? AND chunk_id = ? AND index_kind = ? AND state IN ('pending','inflight')
)`,
		job.CorpusID, job.Source, int64(job.ChunkID), job.IndexKind, job.TextHash, job.Modality,
		job.RelPath, job.Span.Kind, job.Span.Page, job.Span.StartMS, job.Span.EndMS, job.EmbedIdentity,
		job.CorpusID, int64(job.ChunkID), job.IndexKind)
	if err != nil {
		return fmt.Errorf("embedqueue: enqueue: %w", err)
	}
	return nil
}

// execer is the subset of *sql.DB / *sql.Tx that reclaimExpired needs, so the
// same reclaim runs both inside Lease's transaction and on Stats's bare handle.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// reclaimExpired moves in-flight jobs whose lease deadline has passed back to
// pending so a crashed/abandoned worker cannot strand a chunk (SPEC §8.7.3 lease
// expiry). A job whose lease expires after it has already exhausted maxAttempts
// is dead-lettered instead of redelivered, mirroring Nack's gate, so a chunk that
// reliably kills/hangs the worker before it can Ack/Nack cannot be re-leased
// forever. The dead-letter UPDATE runs first so attempts-exhausted rows are not
// caught by the requeue UPDATE.
func (b *SQLiteBroker) reclaimExpired(ctx context.Context, q execer, nowNS int64) error {
	if _, err := q.ExecContext(ctx,
		`UPDATE embed_jobs SET state='dead', token=NULL, deadline_ns=0
		   WHERE state='inflight' AND deadline_ns < ? AND attempts >= ?`,
		nowNS, b.maxAttempts); err != nil {
		return fmt.Errorf("embedqueue: reclaim dead-letter: %w", err)
	}
	if _, err := q.ExecContext(ctx,
		`UPDATE embed_jobs SET state='pending', token=NULL, deadline_ns=0
		   WHERE state='inflight' AND deadline_ns < ?`, nowNS); err != nil {
		return fmt.Errorf("embedqueue: reclaim: %w", err)
	}
	return nil
}

// Lease reclaims expired leases, then atomically claims the oldest claimable
// pending job, from any corpus.
func (b *SQLiteBroker) Lease(ctx context.Context, visibility time.Duration) (Lease, error) {
	return b.LeaseForCorpus(ctx, "", visibility)
}

// LeaseForCorpus claims the oldest claimable pending job belonging to corpusID,
// leaving every other corpus's jobs for the workers that serve them (SPEC
// §8.7.2, §8.7.4). An empty corpusID claims from any corpus, which is what Lease
// does.
func (b *SQLiteBroker) LeaseForCorpus(ctx context.Context, corpusID string, visibility time.Duration) (Lease, error) {
	if visibility <= 0 {
		visibility = 30 * time.Second
	}
	nowNS := b.now().UnixNano()

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, fmt.Errorf("embedqueue: lease begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Reclaim expired in-flight leases (SPEC §8.7.3 lease expiry). Reclaim stays
	// queue-wide rather than corpus-scoped: it is maintenance on rows this
	// transaction is about to read past, and a corpus whose only worker died must
	// not depend on another corpus's worker never running to get its jobs back.
	if err := b.reclaimExpired(ctx, tx, nowNS); err != nil {
		return Lease{}, err
	}

	id, job, priorAttempts, err := b.claimableJob(ctx, tx, corpusID, nowNS)
	if err != nil {
		return Lease{}, err
	}

	token, err := newLeaseToken(id)
	if err != nil {
		return Lease{}, err
	}
	deadline := b.now().Add(visibility)
	if _, err := tx.ExecContext(ctx,
		`UPDATE embed_jobs SET state='inflight', token=?, deadline_ns=?, attempts=attempts+1
		   WHERE id=?`, token, deadline.UnixNano(), id); err != nil {
		return Lease{}, fmt.Errorf("embedqueue: lease claim: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Lease{}, fmt.Errorf("embedqueue: lease commit: %w", err)
	}
	return Lease{
		Job:      job,
		Token:    token,
		Deadline: deadline,
		// The row read and the increment share one transaction, so the delivery
		// count this lease represents is exactly one more than what was on disk.
		Attempts:    priorAttempts + 1,
		MaxAttempts: b.maxAttempts,
	}, nil
}

// claimableJob selects the oldest claimable pending row, optionally restricted
// to one corpus. Split out of LeaseForCorpus so the claim path stays within the
// cyclomatic budget and the two SELECT variants sit next to each other.
func (b *SQLiteBroker) claimableJob(ctx context.Context, tx *sql.Tx, corpusID string, nowNS int64) (int64, Job, int, error) {
	const columns = `id, attempts, corpus_id, source, chunk_id, index_kind, text_hash, modality, rel_path,
       span_kind, span_page, span_start_ms, span_end_ms, embed_identity`
	query := `SELECT ` + columns + `
  FROM embed_jobs
 WHERE state='pending' AND not_before_ns <= ?
 ORDER BY id LIMIT 1`
	args := []any{nowNS}
	if corpusID != "" {
		query = `SELECT ` + columns + `
  FROM embed_jobs
 WHERE corpus_id = ? AND state='pending' AND not_before_ns <= ?
 ORDER BY id LIMIT 1`
		args = []any{corpusID, nowNS}
	}

	var (
		id       int64
		attempts int
		job      Job
		chunkID  int64
	)
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&id, &attempts, &job.CorpusID, &job.Source, &chunkID,
		&job.IndexKind, &job.TextHash, &job.Modality, &job.RelPath, &job.Span.Kind, &job.Span.Page,
		&job.Span.StartMS, &job.Span.EndMS, &job.EmbedIdentity); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, Job{}, 0, ErrNoJob
		}
		return 0, Job{}, 0, fmt.Errorf("embedqueue: lease select: %w", err)
	}
	job.ChunkID = uint64(chunkID)
	return id, job, attempts, nil
}

// Ack deletes the leased job. An unknown/expired token deletes nothing.
func (b *SQLiteBroker) Ack(ctx context.Context, token string) error {
	if _, err := b.db.ExecContext(ctx,
		`DELETE FROM embed_jobs WHERE token=? AND state='inflight'`, token); err != nil {
		return fmt.Errorf("embedqueue: ack: %w", err)
	}
	return nil
}

// Nack requeues the leased job, or dead-letters it once attempts reach
// maxAttempts (SPEC §8.7.3). An unknown/expired token is a no-op.
func (b *SQLiteBroker) Nack(ctx context.Context, token string, retryAfter time.Duration) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("embedqueue: nack begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var id, attempts int64
	row := tx.QueryRowContext(ctx,
		`SELECT id, attempts FROM embed_jobs WHERE token=? AND state='inflight'`, token)
	if err := row.Scan(&id, &attempts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("embedqueue: nack select: %w", err)
	}

	if int(attempts) >= b.maxAttempts {
		if _, err := tx.ExecContext(ctx,
			`UPDATE embed_jobs SET state='dead', token=NULL, deadline_ns=0 WHERE id=?`, id); err != nil {
			return fmt.Errorf("embedqueue: nack dead-letter: %w", err)
		}
		return tx.Commit()
	}

	notBefore := int64(0)
	if retryAfter > 0 {
		notBefore = b.now().Add(retryAfter).UnixNano()
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE embed_jobs SET state='pending', token=NULL, deadline_ns=0, not_before_ns=? WHERE id=?`,
		notBefore, id); err != nil {
		return fmt.Errorf("embedqueue: nack requeue: %w", err)
	}
	return tx.Commit()
}

// Stats reports queue depth, reclaiming expired leases first so in-flight counts
// are accurate.
func (b *SQLiteBroker) Stats(ctx context.Context) (Stats, error) {
	nowNS := b.now().UnixNano()
	if err := b.reclaimExpired(ctx, b.db, nowNS); err != nil {
		return Stats{}, err
	}
	var s Stats
	row := b.db.QueryRowContext(ctx, `
SELECT
  COALESCE(SUM(CASE WHEN state='pending'  THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN state='inflight' THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN state='dead'     THEN 1 ELSE 0 END), 0)
FROM embed_jobs`)
	if err := row.Scan(&s.Pending, &s.InFlight, &s.DeadLettered); err != nil {
		return Stats{}, fmt.Errorf("embedqueue: stats: %w", err)
	}
	return s, nil
}

// Close closes the underlying DB only when the broker owns it.
func (b *SQLiteBroker) Close() error {
	if b.ownsDB && b.db != nil {
		return b.db.Close()
	}
	return nil
}

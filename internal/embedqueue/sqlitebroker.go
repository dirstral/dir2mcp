package embedqueue

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
  rep_id        INTEGER NOT NULL DEFAULT 0,
  chunk_ids     TEXT NOT NULL DEFAULT '',
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
	// Additive columns for document jobs (SPEC §8.1.9); a queue file written by
	// an earlier build gains them with their defaults, and its rows read back as
	// per-chunk jobs.
	for _, stmt := range []string{
		`ALTER TABLE embed_jobs ADD COLUMN rep_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE embed_jobs ADD COLUMN chunk_ids TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := b.db.ExecContext(ctx, stmt); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return fmt.Errorf("embedqueue: migrate broker schema: %w", err)
		}
	}
	// Serves the per-representation dedup probe document jobs run (below); it
	// must come after the ALTERs because rep_id does not exist on a legacy file
	// until they ran.
	if _, err := b.db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_embed_jobs_rep_dedup ON embed_jobs(corpus_id, rep_id, index_kind, state)`); err != nil {
		return fmt.Errorf("embedqueue: migrate broker schema: %w", err)
	}
	return nil
}

// encodeChunkIDs renders a document job's chunk ids for the chunk_ids column;
// empty for a per-chunk job.
func encodeChunkIDs(ids []uint64) string {
	if len(ids) == 0 {
		return ""
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return ""
	}
	return string(raw)
}

// decodeChunkIDs parses the chunk_ids column; empty or malformed reads as a
// per-chunk job (nil).
func decodeChunkIDs(raw string) []uint64 {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var ids []uint64
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil
	}
	return ids
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
	//
	// A DOCUMENT job (SPEC §8.1.9) dedups per REPRESENTATION, not per first
	// chunk id: the document-ownership rule says no two workers pool chunks of
	// one representation concurrently, and a job's first chunk can leave pending
	// while the job is in flight (tombstoned, or failed by a fallback path), so a
	// second document job for the same representation with a different first
	// chunk id would otherwise insert and a second worker would token-embed the
	// same document. The remaining pending chunks are picked up by the next tick
	// once the live job completes.
	dedup := `corpus_id = ? AND chunk_id = ? AND index_kind = ?`
	dedupArgs := []any{job.CorpusID, int64(job.ChunkID), job.IndexKind}
	if job.IsDocument() {
		dedup = `corpus_id = ? AND rep_id = ? AND index_kind = ?`
		dedupArgs = []any{job.CorpusID, job.RepID, job.IndexKind}
	}
	args := []any{
		job.CorpusID, job.Source, int64(job.ChunkID), job.IndexKind, job.TextHash, job.Modality,
		job.RelPath, job.Span.Kind, job.Span.Page, job.Span.StartMS, job.Span.EndMS, job.EmbedIdentity,
		job.RepID, encodeChunkIDs(job.ChunkIDs),
	}
	args = append(args, dedupArgs...)
	_, err := b.db.ExecContext(ctx, `
INSERT INTO embed_jobs(corpus_id, source, chunk_id, index_kind, text_hash, modality,
  rel_path, span_kind, span_page, span_start_ms, span_end_ms, embed_identity, rep_id, chunk_ids, state)
SELECT ?,?,?,?,?,?,?,?,?,?,?,?,?,?, 'pending'
WHERE NOT EXISTS (
  SELECT 1 FROM embed_jobs
   WHERE `+dedup+` AND state IN ('pending','inflight')
)`, args...)
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

// brokerTx is a broker transaction on one connection that it holds from BEGIN
// to release. It exists because database/sql does not know the SQLite state of
// a connection (#1188). A COMMIT that fails, for example with SQLITE_BUSY in
// rollback-journal mode, leaves the SQLite transaction open, but database/sql
// still marks the Tx done and puts the connection back in the pool. Every later
// BEGIN on that connection then fails with "cannot start a transaction within a
// transaction", so the broker stops serving leases. brokerTx keeps the
// connection, so end can roll back the open transaction on that same
// connection before the pool gets it back.
type brokerTx struct {
	tx   *sql.Tx
	conn *sql.Conn
	// committed is set when COMMIT succeeded, so end does not roll back.
	committed bool
	// stranded is set when a COMMIT or ROLLBACK failed, so the SQLite
	// transaction can still be open on conn.
	stranded bool
}

// begin starts a transaction on a connection that the returned brokerTx holds.
// The caller must defer end.
func (b *SQLiteBroker) begin(ctx context.Context) (*brokerTx, error) {
	conn, err := b.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		// A connection that came from the pool inside a transaction refuses
		// BEGIN. This can occur on a *sql.DB that other code shares
		// (NewSQLiteBrokerWithDB). Clear it, so that the next call can begin.
		endStrandedTx(conn)
		_ = conn.Close()
		return nil, err
	}
	return &brokerTx{tx: tx, conn: conn}, nil
}

// commit commits the transaction. When COMMIT fails, end must clear the
// transaction that it can leave open.
func (t *brokerTx) commit() error {
	if err := t.tx.Commit(); err != nil {
		t.stranded = true
		return err
	}
	t.committed = true
	return nil
}

// end rolls back a transaction that did not commit, clears a transaction that
// a failed COMMIT or ROLLBACK left open, and gives the connection back to the
// pool.
func (t *brokerTx) end() {
	if !t.committed {
		if err := t.tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.stranded = true
		}
	}
	if t.stranded {
		endStrandedTx(t.conn)
	}
	_ = t.conn.Close()
}

// endStrandedTx rolls back a SQLite transaction that is open on conn although
// database/sql thinks that no transaction is in progress. When that ROLLBACK
// also fails, the state of the connection is not known, so the pool discards
// the connection and opens a new one. The ROLLBACK does not use the caller's
// context: the cleanup must run also when that context is done.
func endStrandedTx(conn *sql.Conn) {
	_, err := conn.ExecContext(context.Background(), "ROLLBACK")
	if err == nil || strings.Contains(err.Error(), "no transaction is active") {
		return
	}
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
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

	btx, err := b.begin(ctx)
	if err != nil {
		return Lease{}, fmt.Errorf("embedqueue: lease begin: %w", err)
	}
	defer btx.end()
	tx := btx.tx

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
	if err := btx.commit(); err != nil {
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
       span_kind, span_page, span_start_ms, span_end_ms, embed_identity, rep_id, chunk_ids`
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
		chunkIDs string
	)
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&id, &attempts, &job.CorpusID, &job.Source, &chunkID,
		&job.IndexKind, &job.TextHash, &job.Modality, &job.RelPath, &job.Span.Kind, &job.Span.Page,
		&job.Span.StartMS, &job.Span.EndMS, &job.EmbedIdentity, &job.RepID, &chunkIDs); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, Job{}, 0, ErrNoJob
		}
		return 0, Job{}, 0, fmt.Errorf("embedqueue: lease select: %w", err)
	}
	job.ChunkID = uint64(chunkID)
	job.ChunkIDs = decodeChunkIDs(chunkIDs)
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
//
// It is one UPDATE that picks the outcome from the row's own attempts column.
// A transaction that reads the row first and then writes it starts as a
// reader. SQLite cannot upgrade a reader to a writer while another connection
// writes: it returns SQLITE_BUSY at once (in WAL mode SQLITE_BUSY_SNAPSHOT when
// a write committed after the read), and the busy timeout does not apply. The
// worker drops a Nack error, so the job then stayed in flight until its lease
// expired (#1188). A statement that writes first waits for the lock instead.
func (b *SQLiteBroker) Nack(ctx context.Context, token string, retryAfter time.Duration) error {
	notBefore := int64(0)
	if retryAfter > 0 {
		notBefore = b.now().Add(retryAfter).UnixNano()
	}
	if _, err := b.db.ExecContext(ctx, `
UPDATE embed_jobs
   SET state         = CASE WHEN attempts >= ? THEN 'dead' ELSE 'pending' END,
       not_before_ns = CASE WHEN attempts >= ? THEN not_before_ns ELSE ? END,
       token         = NULL,
       deadline_ns   = 0
 WHERE token=? AND state='inflight'`,
		b.maxAttempts, b.maxAttempts, notBefore, token); err != nil {
		return fmt.Errorf("embedqueue: nack: %w", err)
	}
	return nil
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

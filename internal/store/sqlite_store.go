package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dirstral/dir2mcp/internal/mistral"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/relpath"
)

const relPathErrorMessage = "rel_path must be a non-empty relative path without parent-traversal or absolute paths"

type SQLiteStore struct {
	path string

	mu sync.Mutex
	db *sql.DB
	// rdb is a multi-connection READ-ONLY pool over the same file (#429 F11).
	// db stays single-connection so writes remain serialized; rdb lets queries
	// run concurrently with an in-flight write instead of queueing behind it.
	// Nil until initLocked succeeds, and nil-safe: readDB falls back to db.
	rdb *sql.DB
	// vdb is a one-connection READ-ONLY handle used for nothing but the
	// `PRAGMA data_version` probe that guards the ListFiles memo (#429 F10). It is
	// deliberately separate from rdb: the probe pins its connection for the
	// store's lifetime (data_version readings are only comparable within one
	// connection), and pinning a slot of the shared read pool would both shrink
	// read concurrency and put the probe behind whatever scans are in flight.
	// Nil when it failed to open, which simply disables the memo.
	vdb *sql.DB

	// listCache memoizes the ListFiles total (and the glob resume index) for as
	// long as no writer commits (#429 F10). It has its own lock and must never be
	// held while mu is held.
	listCache listFilesCache
	// Counters behind ListFilesQueryStatsForTest; see that method.
	listCountQueries  atomic.Int64
	listGlobFullScans atomic.Int64
	listGlobPageScans atomic.Int64

	activeOps int
	closing   bool
	cond      *sync.Cond
}

// ListFilesQueryStats counts the database work ListFiles has done since the
// store was opened. See ListFilesQueryStatsForTest.
type ListFilesQueryStats struct {
	// CountQueries is the number of `SELECT COUNT(*)` statements executed for a
	// ListFiles total.
	CountQueries int64
	// GlobFullScans is the number of times the glob path scanned and re-globbed
	// every prefix-matched row.
	GlobFullScans int64
	// GlobPageScans is the number of times the glob path materialized a page by
	// resuming from a recorded boundary instead of rescanning from the start.
	GlobPageScans int64
}

// ListFilesQueryStatsForTest reports the ListFiles query counters.
//
// Exported solely so the #429 F10 regression test can live under tests/ as
// AGENTS.md requires: the property worth pinning is that paging through a corpus
// stops re-running the COUNT and the full glob rescan per page, and the number
// of statements a query path issues is not observable through the ordinary store
// API. Production code never calls this.
func (s *SQLiteStore) ListFilesQueryStatsForTest() ListFilesQueryStats {
	return ListFilesQueryStats{
		CountQueries:  s.listCountQueries.Load(),
		GlobFullScans: s.listGlobFullScans.Load(),
		GlobPageScans: s.listGlobPageScans.Load(),
	}
}

// versionProbeDB returns the dedicated data_version handle, or nil when it
// failed to open (in which case the ListFiles memo is bypassed and every call
// recounts). The probe must never run on the writer handle: data_version by
// definition does NOT change for commits made on the same connection, so a
// probe there would miss this store's own ingest writes.
//
// Callers must already hold an activeOps reference (i.e. have called ensureDB or
// ensureReadDB) so the returned handle cannot be closed underneath them.
func (s *SQLiteStore) versionProbeDB() *sql.DB {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vdb
}

type MCPSessionRecord struct {
	ID        string
	Created   time.Time
	LastSeen  time.Time
	AuthScope string
}

type MCPPaymentOutcomeRecord struct {
	ExecutionKey    string
	StatusCode      int
	ResultJSON      string
	RPCErrorJSON    string
	RequiresSettle  bool
	Settled         bool
	PaymentResponse string
	UpdatedAt       time.Time
	// ExpiresAt is the nonce-aligned lifetime of the outcome. The adapter keeps
	// the outcome until this time, so an idempotent retry of the same
	// (nonce, request) pair can get the recorded result for as long as the
	// nonce ledger keeps the nonce consumed. A zero value means the row is from
	// an older schema; the caller then uses the fixed TTL fallback (#697).
	ExpiresAt time.Time
}

// MCPNonceLedgerRecord is the durable single-use replay ledger entry for an x402
// client authorization nonce (x402 adapter spec / bs-010). It records only which
// nonce was spent for which logical request — it holds no funds and no custodial
// balance. A reserved (Consumed=false) entry blocks concurrent replays while a
// settle call is in flight; it becomes durably consumed on settlement success.
type MCPNonceLedgerRecord struct {
	Nonce        string
	RequestKey   string
	ExecutionKey string
	Consumed     bool
	ExpiresAt    time.Time
	UpdatedAt    time.Time
}

// dbExecutor abstracts the methods needed to run SQL statements in either a
// *sql.DB or *sql.Tx.  Upserts on representations share the same logic and the
// two store types can both supply an executor implementing this interface.
// The helper below uses it to avoid duplicating validation and SQL.
type dbExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ActiveDocCountsByStatus returns per-doc_type counts of non-deleted documents
// filtered to the given status (e.g. "ok"). Diagnostics that must reflect only
// the documents the indexer will actually attempt to process use this so they
// do not count skipped/errored rows. An empty status counts all non-deleted
// documents (equivalent to the corpus-wide DocCounts).
func (s *SQLiteStore) ActiveDocCountsByStatus(ctx context.Context, status string) (map[string]int64, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	query := `SELECT doc_type, COUNT(*) FROM documents WHERE deleted = 0 GROUP BY doc_type`
	args := []any{}
	// Trim once and use the trimmed value for both the conditional and the SQL
	// arg, so a whitespace-padded status doesn't silently match nothing.
	if trimmed := strings.TrimSpace(status); trimmed != "" {
		query = `SELECT doc_type, COUNT(*) FROM documents WHERE deleted = 0 AND status = ? GROUP BY doc_type`
		args = append(args, trimmed)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	counts := make(map[string]int64)
	for rows.Next() {
		var (
			docType string
			count   int64
		)
		if err := rows.Scan(&docType, &count); err != nil {
			return nil, err
		}
		docType = strings.TrimSpace(docType)
		if docType == "" {
			docType = "unknown"
		}
		counts[docType] += count
	}
	return counts, rows.Err()
}

// ExtractableExtensionCounts returns per-lowercased-extension counts of
// non-deleted, index-eligible documents whose doc_type requires a document
// extractor to become searchable (the pdf/image/document buckets). It powers the
// doctor extraction-coverage diagnostic, which names the SPECIFIC corpus
// extensions the active extractor cannot read (#395).
//
// The doc_type filter mirrors ingest.ShouldGenerateExtractedMarkdown's
// extractable set (pdf/image/document); store sits below ingest in the import
// graph and cannot import it, so the list is duplicated here. Rows with an empty
// extension are bucketed under "" so the caller can still see uncovered
// extension-less assets. When status is non-empty the count is restricted to
// documents in that status (e.g. "ok" for index-eligible rows), matching
// ActiveDocCountsByStatus.
func (s *SQLiteStore) ExtractableExtensionCounts(ctx context.Context, status string) (map[string]int64, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	query := `SELECT rel_path FROM documents WHERE deleted = 0 AND doc_type IN ('pdf','image','document')`
	args := []any{}
	if trimmed := strings.TrimSpace(status); trimmed != "" {
		query = `SELECT rel_path FROM documents WHERE deleted = 0 AND doc_type IN ('pdf','image','document') AND status = ?`
		args = append(args, trimmed)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	counts := make(map[string]int64)
	for rows.Next() {
		var relPath string
		if err := rows.Scan(&relPath); err != nil {
			return nil, err
		}
		ext := strings.ToLower(filepath.Ext(strings.TrimSpace(relPath)))
		counts[ext]++
	}
	return counts, rows.Err()
}

// activeDocCountsWith encapsulates the query logic previously found in
// SQLiteStore.ActiveDocCounts.  It accepts any dbExecutor so callers can reuse
// an existing *sql.DB or a *sql.Tx without having to open a handle again.
func activeDocCountsWith(ctx context.Context, exec dbExecutor) (map[string]int64, int64, error) {
	rows, err := exec.QueryContext(ctx, `SELECT doc_type, COUNT(*) FROM documents WHERE deleted = 0 GROUP BY doc_type`)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		_ = rows.Close()
	}()

	counts := make(map[string]int64)
	var total int64
	for rows.Next() {
		var (
			docType string
			count   int64
		)
		if err := rows.Scan(&docType, &count); err != nil {
			return nil, 0, err
		}
		docType = strings.TrimSpace(docType)
		if docType == "" {
			docType = "unknown"
		}
		counts[docType] += count
		total += count
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return counts, total, nil
}

func upsertRepresentationWith(ctx context.Context, exec dbExecutor, rep model.Representation) (int64, error) {
	if rep.DocID <= 0 {
		return 0, errors.New("doc_id must be > 0")
	}
	repType := strings.TrimSpace(rep.RepType)
	if repType == "" {
		repType = "raw_text"
	}
	repHash := strings.TrimSpace(rep.RepHash)
	if repHash == "" {
		return 0, errors.New("rep_hash must be non-empty")
	}
	createdUnix := rep.CreatedUnix
	if createdUnix <= 0 {
		createdUnix = time.Now().Unix()
	}

	_, err := exec.ExecContext(
		ctx,
		`INSERT INTO representations(doc_id, rep_type, rep_hash, meta_json, created_unix, deleted)
		 VALUES(?, ?, ?, ?, ?, ?)
		 ON CONFLICT(doc_id, rep_type) DO UPDATE SET
		   rep_hash=excluded.rep_hash,
		   meta_json=excluded.meta_json,
		   created_unix=excluded.created_unix,
		   deleted=excluded.deleted`,
		rep.DocID,
		repType,
		repHash,
		strings.TrimSpace(rep.MetaJSON),
		createdUnix,
		boolToInt(rep.Deleted),
	)
	if err != nil {
		return 0, err
	}

	var repID int64
	if err := exec.QueryRowContext(
		ctx,
		`SELECT rep_id FROM representations WHERE doc_id = ? AND rep_type = ? LIMIT 1`,
		rep.DocID,
		repType,
	).Scan(&repID); err != nil {
		return 0, err
	}
	if repID <= 0 {
		return 0, errors.New("representation upsert did not return a row")
	}
	return repID, nil
}

func lookupChunkDocContext(ctx context.Context, exec dbExecutor, repID int64) (relPath, docType, repType, language string, err error) {
	var metaJSON string
	err = exec.QueryRowContext(
		ctx,
		`SELECT d.rel_path, d.doc_type, r.rep_type, COALESCE(r.meta_json, '')
		 FROM representations r
		 JOIN documents d ON d.doc_id = r.doc_id
		 WHERE r.rep_id = ?
		 LIMIT 1`,
		repID,
	).Scan(&relPath, &docType, &repType, &metaJSON)
	if err != nil {
		return relPath, docType, repType, "", err
	}
	// Denormalize the representation's recorded effective language (SPEC §5.2/§8.8)
	// onto the chunk so the per-language retrieval filter (§9.5) can predicate at
	// candidate selection. The recorded `language` is already the resolved
	// effective value (precedence applied at the ingest write per §8.8), so the
	// store merely reads it; absent ⇒ unknown (empty), never an error.
	return relPath, docType, repType, languageFromRepMeta(metaJSON), err
}

// languageFromRepMeta extracts the effective BCP-47 language a representation
// recorded in its meta_json (SPEC §5.2 `language`). It is intentionally tolerant:
// a representation with no meta, unparseable meta, or no `language` field is
// "unknown language" (returns ""), which never matches a specific per-language
// filter (§9.5). The recorded value is returned verbatim (trimmed); §9.5
// matching normalizes to the primary subtag, so both full-tag and primary-subtag
// recordings filter correctly.
func languageFromRepMeta(metaJSON string) string {
	metaJSON = strings.TrimSpace(metaJSON)
	if metaJSON == "" {
		return ""
	}
	var meta struct {
		Language string `json:"language"`
	}
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		return ""
	}
	return strings.TrimSpace(meta.Language)
}

func insertChunkWithSpansWith(ctx context.Context, exec dbExecutor, chunk model.Chunk, spans []model.Span, relPath, docType, repType, language string) (int64, error) {
	_, err := exec.ExecContext(
		ctx,
		`INSERT INTO chunks(rep_id, ordinal, rel_path, doc_type, rep_type, text, text_hash, tokens_est, index_kind, modality, media_ref, language, embedding_status, embedding_error, error_category, embedding_failed_unix, chunk_context, embedding_mode, deleted)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(rep_id, ordinal) DO UPDATE SET
		   rel_path=excluded.rel_path,
		   doc_type=excluded.doc_type,
		   rep_type=excluded.rep_type,
		   text=excluded.text,
		   text_hash=excluded.text_hash,
		   tokens_est=excluded.tokens_est,
		   index_kind=excluded.index_kind,
		   modality=excluded.modality,
		   media_ref=excluded.media_ref,
		   language=excluded.language,
		   embedding_status=excluded.embedding_status,
		   embedding_error=excluded.embedding_error,
		   error_category=excluded.error_category,
		   embedding_failed_unix=excluded.embedding_failed_unix,
		   chunk_context=excluded.chunk_context,
		   embedding_mode=excluded.embedding_mode,
		   deleted=excluded.deleted`,
		chunk.RepID,
		chunk.Ordinal,
		relPath,
		docType,
		defaultIfEmpty(repType, "raw_text"),
		chunk.Text,
		strings.TrimSpace(chunk.TextHash),
		0,
		normalizeIndexKind(chunk.IndexKind),
		normalizeModality(chunk.Modality),
		strings.TrimSpace(chunk.MediaRef),
		strings.TrimSpace(language),
		normalizeEmbeddingStatus(chunk.EmbeddingStatus),
		strings.TrimSpace(chunk.EmbeddingError),
		strings.TrimSpace(chunk.ErrorCategory),
		// A chunk can be born failed: the output quality gate quarantines a
		// degenerate transcript/OCR chunk at insert time. Stamp that the same
		// way a mid-run failure is stamped so the failure aggregates can age it
		// (issue #783).
		embeddingFailureStamp(normalizeEmbeddingStatus(chunk.EmbeddingStatus), time.Now()),
		chunk.Context,
		model.NormalizeEmbeddingMode(chunk.EmbeddingMode),
		boolToInt(chunk.Deleted),
	)
	if err != nil {
		return 0, err
	}

	var chunkID int64
	if err := exec.QueryRowContext(
		ctx,
		`SELECT chunk_id FROM chunks WHERE rep_id = ? AND ordinal = ? LIMIT 1`,
		chunk.RepID,
		chunk.Ordinal,
	).Scan(&chunkID); err != nil {
		return 0, err
	}

	if _, err := exec.ExecContext(ctx, `DELETE FROM spans WHERE chunk_id = ?`, chunkID); err != nil {
		return 0, err
	}

	for _, span := range spans {
		spanKind, startValue, endValue, extraJSON, spanErr := spanToRow(span)
		if spanErr != nil {
			return 0, spanErr
		}
		var extra any // NULL for scalar kinds, JSON text for region spans
		if extraJSON != "" {
			extra = extraJSON
		}
		if _, err := exec.ExecContext(
			ctx,
			`INSERT INTO spans (chunk_id, span_kind, start, end, extra_json) VALUES (?, ?, ?, ?, ?)`,
			chunkID,
			spanKind,
			startValue,
			endValue,
			extra,
		); err != nil {
			return 0, err
		}
	}

	return chunkID, nil
}

func NewSQLiteStore(path string) *SQLiteStore {
	s := &SQLiteStore{path: path}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// openDB opens the SQLite file and applies the connection-pool and pragma
// settings that protect against in-process and cross-process write contention:
//
//   - SetMaxOpenConns(1) serializes all operations through this single
//     *sql.DB handle. Without this, the database/sql pool can hand out
//     multiple connections to concurrent goroutines (chunk writers and
//     embedding-mark batches), each starting its own write transaction and
//     producing SQLITE_BUSY.
//   - PRAGMA journal_mode=WAL reduces SQLite-level read/write blocking for
//     other connections and processes, even though operations through this
//     handle are still serialized by the single-connection pool.
//   - PRAGMA busy_timeout instructs SQLite to wait rather than return BUSY
//     immediately when an external process holds the database lock.
//
// sqliteDSN builds a DSN carrying the PER-CONNECTION pragmas.
//
// busy_timeout and foreign_keys are connection-local. They used to be applied
// with one ExecContext each, which stuck only because SetMaxOpenConns(1) meant
// there was exactly one connection to apply them to. The read pool (#429 F11)
// has several, and database/sql opens them lazily, so a pragma set via
// ExecContext would land on whichever connection happened to serve that call
// and silently miss the rest. Carrying them in the DSN makes every connection
// get them at open time.
//
// journal_mode is deliberately NOT here: it is a persistent, database-level
// setting, so one connection setting it is enough, and putting it in the DSN
// would apply it at connect time, before checkSchemaVersion can run. That would
// break the #405 tripwire, which requires a future-schema database to be left
// byte-for-byte untouched (the WAL switch creates a -wal file).
func sqliteDSN(path string) string {
	return "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
}

// sqliteReadOnlyDSN is sqliteDSN plus query_only, which makes SQLite itself
// reject any write attempted on the connection.
//
// Without it the read pool is read-only by convention alone: the connections
// are ordinary read/write handles, so a future call site could route a write
// through readDB and silently reintroduce the multi-writer SQLITE_BUSY that
// SetMaxOpenConns(1) exists to prevent. query_only turns that latent mistake
// into an immediate, obvious error at the point it is written.
func sqliteReadOnlyDSN(path string) string {
	return sqliteDSN(path) + "&_pragma=query_only(1)"
}

func openDB(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// busy_timeout and foreign_keys now arrive via the DSN (sqliteDSN), so they
	// are in effect on every connection the moment it opens. busy_timeout being
	// live before the WAL switch below still matters for the same reason it did
	// when it was an ExecContext: switching journal_mode can itself return
	// SQLITE_BUSY when another process holds the database lock, and without a
	// busy timeout already in effect that PRAGMA fails immediately instead of
	// waiting. Neither pragma creates or mutates an on-disk file.
	//
	// Reject a database written by a NEWER binary BEFORE any persistent mutation
	// (#405). PRAGMA journal_mode=WAL below persistently creates/modifies the
	// -wal file, so the downgrade tripwire MUST run first: a future-schema DB we
	// do not understand must be left byte-for-byte untouched. Reading
	// user_version is a pure read and does not create a -wal file.
	if err := checkSchemaVersion(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	// Order matters: busy_timeout (set above) MUST come before journal_mode.
	// Switching to WAL itself can fail with SQLITE_BUSY if another process holds
	// the database lock; without busy_timeout already in effect, that PRAGMA
	// returns immediately rather than waiting.
	//
	// journal_mode is persistent and database-level, so setting it once here is
	// enough and the read pool inherits WAL from the file. foreign_keys, which
	// used to sit in this loop, is per-connection and moved to the DSN: relying
	// on SetMaxOpenConns(1) to make it stick would have silently left the
	// ON DELETE CASCADE constraints (#405) inert on every read-pool connection.
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// openReadDB opens the concurrent read pool (#429 F11). It MUST be called only
// after openDB has run the #405 tripwire and switched the file to WAL: WAL is
// what lets these readers proceed while the writer holds its transaction, and
// opening a pool against a future-schema database would defeat the tripwire.
func openReadDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteReadOnlyDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(sqliteReadPoolSize)
	db.SetMaxIdleConns(sqliteReadPoolSize)
	return db, nil
}

// openVersionProbeDB opens the single-connection read-only handle that carries
// the `PRAGMA data_version` probe (#429 F10). Like openReadDB it MUST run only
// after openDB has passed the #405 tripwire and switched the file to WAL.
//
// One connection is the point: the probe pins it so successive data_version
// readings are comparable (they are meaningless across connections), and keeping
// it out of the shared read pool means the probe never queues behind a scan and
// never costs the pool a slot.
func openVersionProbeDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", sqliteReadOnlyDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

// sqliteReadPoolSize bounds concurrent readers. Small on purpose: these are
// short point lookups and paged scans, and every open connection costs a file
// descriptor and its own page cache.
const sqliteReadPoolSize = 4

// schemaVersion is the persistence-layer schema revision this binary
// understands, stamped into the database file header via PRAGMA user_version
// (#405). It is a monotonic counter, NOT the product/release version.
//
// All migrations to date are additive and idempotent (CREATE ... IF NOT
// EXISTS, ALTER TABLE ADD COLUMN with defaults), so upgrading or downgrading
// across them is safe and the floor stays at 1. Bump this ONLY when
// introducing a non-additive change (a NOT NULL column without a default, a
// type/tokenizer change, a table rewrite) so that:
//   - a newer DB opened by an older binary is refused rather than
//     silently mixed-version corrupted (the downgrade tripwire below), and
//   - the bump documents exactly which change is not backward compatible.
const schemaVersion = 1

// indexFormatVersion is the on-disk index format this binary reads and writes.
// It is persisted in settings under "index_format_version" on first init. Bump
// ONLY on a non-backward-compatible index format change; a mismatch between the
// persisted value and this constant surfaces the canonical §14.3
// INDEX_VERSION_MISMATCH so an operator reindexes with a compatible binary
// rather than reading a format this binary does not understand.
const indexFormatVersion = "1"

// IndexVersionMismatchError reports that a corpus's persisted
// index_format_version does not match indexFormatVersion. It carries the
// canonical §14.3 code and is NOT retryable — the corpus must be reindexed with
// a compatible binary.
type IndexVersionMismatchError struct {
	Persisted string
	Expected  string
}

func (e *IndexVersionMismatchError) Error() string {
	return fmt.Sprintf(
		"%s: corpus index_format_version %q does not match this binary's %q; reindex the corpus",
		protocol.ErrorCodeIndexVersionMismatch, e.Persisted, e.Expected,
	)
}

// Code returns the canonical §14.3 error code for this failure.
func (e *IndexVersionMismatchError) Code() string { return protocol.ErrorCodeIndexVersionMismatch }

// Retryable reports that an index-version mismatch is not retryable.
func (e *IndexVersionMismatchError) Retryable() bool { return false }

// checkIndexFormatVersion refuses to open a corpus whose persisted
// index_format_version differs from indexFormatVersion (§14.3). It runs AFTER
// bootstrapSettingsLocked, which inserts the current version for a fresh DB
// (ON CONFLICT DO NOTHING), so an existing corpus keeps its recorded value: a
// v1 corpus matches (no false positive), while a corpus stamped by a future,
// incompatible binary is rejected rather than silently misread. A legacy DB
// with the row absent, or an empty value, is treated as current.
func checkIndexFormatVersion(ctx context.Context, db *sql.DB) error {
	var persisted string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = 'index_format_version' LIMIT 1`).Scan(&persisted)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read index_format_version: %w", err)
	}
	persisted = strings.TrimSpace(persisted)
	if persisted == "" || persisted == indexFormatVersion {
		return nil
	}
	return &IndexVersionMismatchError{Persisted: persisted, Expected: indexFormatVersion}
}

// checkSchemaVersion reads PRAGMA user_version and refuses to proceed when the
// database was written by a newer binary (dbVersion > schemaVersion). Older or
// unstamped databases (user_version == 0, the SQLite default) are accepted and
// upgraded in place by the additive migrations, then re-stamped by
// stampSchemaVersion. openDB calls this BEFORE applying journal_mode=WAL (or any
// other persistent pragma/migration) so a future-schema DB is never mutated —
// not even its -wal file created — by a binary that does not understand it.
func checkSchemaVersion(ctx context.Context, db *sql.DB) error {
	var dbVersion int64
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&dbVersion); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if dbVersion > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d); upgrade dir2mcp to open this corpus", dbVersion, schemaVersion)
	}
	return nil
}

// stampSchemaVersion records the current schemaVersion into the database file
// header. PRAGMA user_version does not accept bound parameters, so the trusted
// integer constant is formatted directly (never user input).
func stampSchemaVersion(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("stamp schema version: %w", err)
	}
	return nil
}

// applyAdditiveColumnMigrations runs ALTER TABLE ADD COLUMN statements that
// extend existing tables with new optional columns. Each statement is wrapped
// with isDuplicateColumnError so that the migration is idempotent on
// already-upgraded databases. Add new entries here when introducing additive
// schema changes; do not modify existing entries (their effects are now
// historical migrations).
//
// The 'title' column on documents was added for #159 — surface a
// human-readable document title in citations alongside rel_path.
func applyAdditiveColumnMigrations(ctx context.Context, db *sql.DB) error {
	migrations := []string{
		`ALTER TABLE documents ADD COLUMN source_type TEXT NOT NULL DEFAULT 'filesystem'`,
		`ALTER TABLE mcp_sessions ADD COLUMN auth_scope TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE representations ADD COLUMN meta_json TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE documents ADD COLUMN title TEXT NOT NULL DEFAULT ''`,
		// error_category folds free-text embedding_error into a small
		// enum (store.ErrorCategory) so dir2mcp status / doctor can
		// surface "rate_limit: 67, payload_too_large: 21" instead of
		// just an aggregate failure count.
		`ALTER TABLE chunks ADD COLUMN error_category TEXT NOT NULL DEFAULT ''`,
		// error_message stores a short human-readable explanation when
		// a document ends up with status='error' (extraction crash,
		// representation generation failure). Surfaced in the support
		// bundle's list-files.json so maintainers can tell *why* a doc
		// failed without grepping server.log.
		`ALTER TABLE documents ADD COLUMN error_message TEXT NOT NULL DEFAULT ''`,
		// modality / media_ref support multimodal embeddings (SPEC 8.1.7):
		// modality is text|image|audio|video|pdf; media_ref is the corpus
		// rel_path of the source media for a non-text chunk (the embedding
		// worker reads those bytes and embeds them directly). Text chunks
		// keep modality='text', media_ref=''.
		`ALTER TABLE chunks ADD COLUMN modality TEXT NOT NULL DEFAULT 'text'`,
		`ALTER TABLE chunks ADD COLUMN media_ref TEXT NOT NULL DEFAULT ''`,
		// etag stores a remote object store's cheap change token (S3 object
		// ETag) so an incremental scan can skip re-reading + re-hashing an
		// unchanged object (SPEC §7.8.3, #245). Empty for local/NFS corpora,
		// whose change detection keeps using (size, mtime) then content_hash.
		`ALTER TABLE documents ADD COLUMN etag TEXT NOT NULL DEFAULT ''`,
		// sidecar_fingerprint persists the same subtitle-sidecar signature
		// (sorted rel_paths + mtimes) that buildDocumentWithContent folds into
		// content_hash. Persisting it separately lets the remote (S3) ETag fast
		// path detect a sidecar added/changed/removed while the media object's
		// own ETag is unchanged, without re-reading the media bytes (SPEC
		// §7.8.3, #298). Empty for local/NFS corpora, non-media docs, and media
		// with no sidecar — preserving the historical fast-path behavior.
		`ALTER TABLE documents ADD COLUMN sidecar_fingerprint TEXT NOT NULL DEFAULT ''`,
		// language denormalizes the effective BCP-47 language of a chunk's source
		// representation (SPEC §5.2/§8.8) onto the chunk so the per-language
		// retrieval filter (§9.5) can predicate at candidate selection without a
		// per-chunk representation meta_json lookup. Populated at chunk insert from
		// the representation's recorded meta language; empty means the
		// representation recorded no language (unknown) — which never matches a
		// specific filter, so a corpus indexed before any language was recorded
		// simply has empty values here (no migration of existing rows needed, §9.5).
		`ALTER TABLE chunks ADD COLUMN language TEXT NOT NULL DEFAULT ''`,
		// skip_reason records *why* a document was recorded as skipped (never
		// indexed) rather than ingested — one of the model.SkipReason* strings
		// ("archive", "binary_ignored", "ignore_rule", "secret_excluded",
		// "unsupported_format", …). Empty for ingested ("ok") and errored rows.
		// Aggregated into CorpusStats.SkipSummary so status/reindex can report
		// honest coverage ("what wasn't indexed & why", #414/#395). Existing rows
		// default to '' (unknown reason), which the aggregate normalizes.
		`ALTER TABLE documents ADD COLUMN skip_reason TEXT NOT NULL DEFAULT ''`,
		// chunk_context / embedding_mode back contextual retrieval (SPEC §5.3,
		// §8.1.8, issue #330). chunk_context holds the generated document-aware
		// context prepended to the chunk's EMBED input only — never to `text`,
		// which stays the raw, displayed and CITED chunk (#403). embedding_mode
		// disambiguates an empty context: 'disabled' (feature off), 'contextualized'
		// (context generated), or 'fallback' (generation failed, embedded raw).
		// Both are additive: a pre-feature index gets '' / 'disabled' on every
		// existing row, which is exactly how the spec says such a corpus must read,
		// and its embed identity migrates to `…|off` — so no reindex.
		`ALTER TABLE chunks ADD COLUMN chunk_context TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE chunks ADD COLUMN embedding_mode TEXT NOT NULL DEFAULT 'disabled'`,
		// embedding_failed_unix stamps WHEN a chunk entered embedding_status
		// ='error' so the failure aggregates can say how old the failures they
		// report are (issue #783). Without it corpus.json re-reported failures
		// persisted hours earlier under this run's fresh `ts`, which reads as
		// "this run just failed N times against the provider" — the worst
		// possible signal for an operator who is mid-way through swapping a
		// credential and made no request at all. Existing rows default to 0,
		// which the aggregate reports as "unknown age" rather than as 1970.
		`ALTER TABLE chunks ADD COLUMN embedding_failed_unix INTEGER NOT NULL DEFAULT 0`,
		// expires_unix stores the nonce-aligned lifetime of a persisted x402
		// payment outcome (issue #697). The in-memory outcome already carried
		// this time, but the row did not, so a restart fell back to the fixed
		// 10-minute TTL on UpdatedAt. The nonce ledger keeps a consumed nonce
		// for at least 15 minutes, so a valid idempotent retry inside that gap
		// found the outcome gone and got "nonce already used" instead of the
		// result it had already paid for. Existing rows default to 0, which
		// reads back as a zero time and keeps the old TTL fallback.
		`ALTER TABLE mcp_payment_outcomes ADD COLUMN expires_unix INTEGER NOT NULL DEFAULT 0`,
	}
	for _, stmt := range migrations {
		if _, err := db.ExecContext(ctx, stmt); err != nil && !isDuplicateColumnError(err) {
			return err
		}
	}
	return nil
}

// initLocked performs the same initialization work as Init but assumes
// the caller already holds s.mu. This helper allows ensureDB to set up the
// database under lock, closing a small race window against Close().
func (s *SQLiteStore) initLocked(ctx context.Context) error {
	if s.db != nil {
		return nil
	}

	// Ensure parent directory exists with restrictive permissions
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}

	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open database file: %w", err)
	}
	_ = f.Close()
	if err := os.Chmod(s.path, 0o600); err != nil {
		return fmt.Errorf("set database file permissions: %w", err)
	}

	// openDB refuses a database written by a newer binary before applying any
	// persistent pragma (WAL), so a future-schema DB is rejected untouched (#405).
	db, err := openDB(ctx, s.path)
	if err != nil {
		return err
	}

	schema := `
CREATE TABLE IF NOT EXISTS documents (
  doc_id INTEGER PRIMARY KEY AUTOINCREMENT,
  rel_path TEXT NOT NULL UNIQUE,
  doc_type TEXT NOT NULL,
  source_type TEXT NOT NULL DEFAULT 'filesystem',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  mtime_unix INTEGER NOT NULL DEFAULT 0,
  content_hash TEXT NOT NULL DEFAULT '',
  etag TEXT NOT NULL DEFAULT '',
  sidecar_fingerprint TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'ok',
  deleted INTEGER NOT NULL DEFAULT 0,
  skip_reason TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS representations (
  rep_id INTEGER PRIMARY KEY AUTOINCREMENT,
  doc_id INTEGER NOT NULL,
  rep_type TEXT NOT NULL,
  rep_hash TEXT NOT NULL,
  meta_json TEXT NOT NULL DEFAULT '',
  created_unix INTEGER NOT NULL,
  deleted INTEGER NOT NULL DEFAULT 0,
  UNIQUE(doc_id, rep_type),
  FOREIGN KEY (doc_id) REFERENCES documents(doc_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS chunks (
  chunk_id INTEGER PRIMARY KEY,
  rep_id INTEGER,
  ordinal INTEGER NOT NULL DEFAULT 0,
  rel_path TEXT NOT NULL,
  doc_type TEXT NOT NULL,
  rep_type TEXT NOT NULL DEFAULT 'raw_text',
  text TEXT NOT NULL,
  text_hash TEXT NOT NULL DEFAULT '',
  tokens_est INTEGER NOT NULL DEFAULT 0,
  index_kind TEXT NOT NULL DEFAULT 'text',
  modality TEXT NOT NULL DEFAULT 'text',
  media_ref TEXT NOT NULL DEFAULT '',
  language TEXT NOT NULL DEFAULT '',
  embedding_status TEXT NOT NULL DEFAULT 'pending',
  embedding_error TEXT NOT NULL DEFAULT '',
  error_category TEXT NOT NULL DEFAULT '',
  embedding_failed_unix INTEGER NOT NULL DEFAULT 0,
  chunk_context TEXT NOT NULL DEFAULT '',
  embedding_mode TEXT NOT NULL DEFAULT 'disabled',
  deleted INTEGER NOT NULL DEFAULT 0,
  UNIQUE(rep_id, ordinal),
  FOREIGN KEY (rep_id) REFERENCES representations(rep_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS spans (
  span_id INTEGER PRIMARY KEY AUTOINCREMENT,
  chunk_id INTEGER NOT NULL,
  span_kind TEXT NOT NULL,
  start INTEGER NOT NULL,
  end INTEGER NOT NULL,
  extra_json TEXT,
  FOREIGN KEY (chunk_id) REFERENCES chunks(chunk_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS mcp_sessions (
  session_id TEXT PRIMARY KEY,
  created_unix INTEGER NOT NULL,
  last_seen_unix INTEGER NOT NULL,
  auth_scope TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS mcp_payment_outcomes (
  execution_key TEXT PRIMARY KEY,
  status_code INTEGER NOT NULL,
  result_json TEXT NOT NULL DEFAULT '',
  rpc_error_json TEXT NOT NULL DEFAULT '',
  requires_settle INTEGER NOT NULL DEFAULT 0,
  settled INTEGER NOT NULL DEFAULT 0,
  payment_response TEXT NOT NULL DEFAULT '',
  updated_unix INTEGER NOT NULL,
  expires_unix INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS mcp_nonce_ledger (
  nonce TEXT PRIMARY KEY,
  request_key TEXT NOT NULL DEFAULT '',
  execution_key TEXT NOT NULL DEFAULT '',
  consumed INTEGER NOT NULL DEFAULT 0,
  expires_unix INTEGER NOT NULL,
  updated_unix INTEGER NOT NULL
);

CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
  text,
  content='chunks',
  content_rowid='chunk_id',
  tokenize='porter unicode61'
);

CREATE TRIGGER IF NOT EXISTS chunks_ai AFTER INSERT ON chunks BEGIN
  INSERT INTO chunks_fts(rowid, text) VALUES (new.chunk_id, new.text);
END;
CREATE TRIGGER IF NOT EXISTS chunks_ad AFTER DELETE ON chunks BEGIN
  INSERT INTO chunks_fts(chunks_fts, rowid, text) VALUES('delete', old.chunk_id, old.text);
END;
CREATE TRIGGER IF NOT EXISTS chunks_au AFTER UPDATE ON chunks BEGIN
  INSERT INTO chunks_fts(chunks_fts, rowid, text) VALUES('delete', old.chunk_id, old.text);
  INSERT INTO chunks_fts(rowid, text) VALUES (new.chunk_id, new.text);
END;

CREATE INDEX IF NOT EXISTS idx_documents_rel_path ON documents(rel_path);
CREATE INDEX IF NOT EXISTS idx_documents_deleted ON documents(deleted);
CREATE INDEX IF NOT EXISTS idx_representations_doc_id ON representations(doc_id);
CREATE INDEX IF NOT EXISTS idx_chunks_rep_id ON chunks(rep_id);
CREATE INDEX IF NOT EXISTS idx_chunks_embedding_status ON chunks(embedding_status);
CREATE INDEX IF NOT EXISTS idx_chunks_index_kind ON chunks(index_kind);
CREATE INDEX IF NOT EXISTS idx_chunks_rel_path_deleted ON chunks(rel_path, deleted);
-- Serves the keyset page of ListEmbeddedChunkMetadata (#732): the three equality
-- terms are the leading columns and chunk_id is last, so a kind-scoped page seeks
-- straight to the cursor and reads only the rows it returns, already in chunk_id
-- order (no sort, no scan of the rest of the corpus).
CREATE INDEX IF NOT EXISTS idx_chunks_embedded_kind_seek ON chunks(embedding_status, deleted, index_kind, chunk_id);
CREATE INDEX IF NOT EXISTS idx_spans_chunk_id_span_id ON spans(chunk_id, span_id);
CREATE INDEX IF NOT EXISTS idx_mcp_sessions_last_seen ON mcp_sessions(last_seen_unix);
CREATE INDEX IF NOT EXISTS idx_mcp_payment_outcomes_updated ON mcp_payment_outcomes(updated_unix);
CREATE INDEX IF NOT EXISTS idx_mcp_nonce_ledger_expires ON mcp_nonce_ledger(expires_unix);
`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return err
	}
	if err := applyAdditiveColumnMigrations(ctx, db); err != nil {
		_ = db.Close()
		return err
	}
	if err := repairFTSIfDrifted(ctx, db); err != nil {
		_ = db.Close()
		return err
	}

	if err := bootstrapSettingsLocked(ctx, db); err != nil {
		_ = db.Close()
		return err
	}

	// Refuse a corpus written in an incompatible index format (§14.3). Runs after
	// bootstrap so a fresh DB carries the current version and only a genuine
	// mismatch (a future/incompatible binary's stamp) is rejected.
	if err := checkIndexFormatVersion(ctx, db); err != nil {
		_ = db.Close()
		return err
	}

	// Stamp the schema version last, once every migration/repair above
	// succeeded, so a partially-migrated DB is never marked current (#405).
	if err := stampSchemaVersion(ctx, db); err != nil {
		_ = db.Close()
		return err
	}

	s.db = db
	// Open the read pool only now: the tripwire has passed, WAL is on, and every
	// migration has been stamped, so readers can never observe a half-migrated
	// schema and can never be the connection that touches a future-schema file.
	// A failure here is NOT fatal -- readDB falls back to the writer handle, so
	// the store degrades to the previous single-connection behaviour instead of
	// refusing to start (#429 F11).
	if rdb, rerr := openReadDB(s.path); rerr == nil {
		s.rdb = rdb
	}
	// Same deal for the data_version probe handle (#429 F10): a failure here just
	// disables the ListFiles memo, it never blocks startup.
	if vdb, verr := openVersionProbeDB(s.path); verr == nil {
		s.vdb = vdb
	}
	return nil
}

// repairFTSIfDrifted rebuilds the chunks_fts external-content index whenever it
// has drifted out of sync with the chunks table (#405). The chunks_ai/ad/au
// triggers mirror EVERY chunk row (including soft-deleted ones, whose deleted
// flag is a column update, not a row removal) into chunks_fts, so a healthy
// index has exactly one FTS entry per chunk row.
//
// Drift is measured against the chunks_fts_docsize shadow table, NOT
// `SELECT COUNT(*) FROM chunks_fts`: on an external-content FTS5 table a plain
// COUNT proxies back to the content table (chunks), so it can never reveal a
// missing index entry. chunks_fts_docsize holds one row per actually-indexed
// document, so COUNT(chunks_fts_docsize) == COUNT(chunks) is the true
// consistency invariant. (FTS5 always creates _docsize unless the index was
// declared with columnsize=0, which chunks_fts is not.)
//
// This supersedes the earlier "rebuild only when fully empty" probe, which
// missed PARTIAL drift: a crash mid-rebuild, or any future write path that
// bypasses the triggers, leaves a partially-populated FTS that the emptiness
// check waved through — silently losing lexical recall for the missing chunks
// until an operator ran an explicit rebuild. Any count mismatch (empty OR
// partial) triggers a full rebuild from the content='chunks' reference; the
// rebuild re-indexes every chunk row, so the counts match afterward and
// startup does not loop. Distinct from #373 (NULL bm25 score) and #439
// (quarantine query-time filter): both of those leave the FTS row set intact.
func repairFTSIfDrifted(ctx context.Context, db *sql.DB) error {
	var chunkCount, indexedCount int64
	// Read both counts in ONE statement so they observe a single, consistent
	// database snapshot. Two separate COUNT(*) queries can straddle a concurrent
	// writer's commit and see different snapshots, reporting a spurious mismatch
	// that triggers a needless (costly) full FTS rebuild.
	if err := db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM chunks),
		(SELECT COUNT(*) FROM chunks_fts_docsize)`).Scan(&chunkCount, &indexedCount); err != nil {
		return fmt.Errorf("count chunks vs chunks_fts_docsize: %w", err)
	}
	if chunkCount == indexedCount {
		return nil
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO chunks_fts(chunks_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("rebuild chunks_fts: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Init(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initLocked(ctx)
}

func (s *SQLiteStore) UpsertDocument(ctx context.Context, doc model.Document) error {
	relPath, err := normalizeRelPath(doc.RelPath)
	if err != nil {
		return err
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()

	// error_message is unconditionally overwritten by excluded.error_message
	// so a successful re-ingest after a prior error clears the stale message
	// (caller passes "" for healthy docs, the failure-recovery message
	// otherwise). Title intentionally uses the CASE-preserve pattern because
	// title can be extracted *after* the first upsert; error_message has no
	// such two-phase write, so the always-replace semantics are correct.
	_, err = db.ExecContext(
		ctx,
		`INSERT INTO documents(rel_path, doc_type, source_type, size_bytes, mtime_unix, content_hash, etag, sidecar_fingerprint, status, deleted, title, error_message, skip_reason)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(rel_path) DO UPDATE SET
		   doc_type=excluded.doc_type,
		   source_type=excluded.source_type,
		   size_bytes=excluded.size_bytes,
		   mtime_unix=excluded.mtime_unix,
		   content_hash=excluded.content_hash,
		   etag=excluded.etag,
		   sidecar_fingerprint=excluded.sidecar_fingerprint,
		   status=excluded.status,
		   deleted=excluded.deleted,
		   title=CASE WHEN excluded.title <> '' THEN excluded.title ELSE documents.title END,
		   error_message=excluded.error_message,
		   skip_reason=excluded.skip_reason`,
		relPath,
		normalizeDocType(doc.DocType),
		normalizeSourceType(doc.SourceType),
		doc.SizeBytes,
		doc.MTimeUnix,
		strings.TrimSpace(doc.ContentHash),
		strings.TrimSpace(doc.ETag),
		doc.SidecarFingerprint,
		normalizeStatus(doc.Status),
		boolToInt(doc.Deleted),
		strings.TrimSpace(doc.Title),
		sanitizeDocErrorMessage(doc.ErrorMessage),
		strings.TrimSpace(doc.SkipReason),
	)
	return err
}

// sanitizeDocErrorMessage normalises a free-text upstream error before it
// is persisted to documents.error_message. We strip leading/trailing
// whitespace, replace embedded control characters with spaces (so the
// message stays one line in the support bundle), and cap the length at
// 512 bytes so a runaway stack trace can't bloat the documents table.
// Truncation respects UTF-8 boundaries to avoid persisting an invalid
// trailing rune.
func sanitizeDocErrorMessage(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	const maxBytes = 512
	var b strings.Builder
	b.Grow(len(msg))
	for _, r := range msg {
		if r == '\n' || r == '\r' || r == '\t' {
			b.WriteByte(' ')
			continue
		}
		if r < 0x20 || r == 0x7f {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) <= maxBytes {
		return out
	}
	// Truncate on a rune boundary.
	cut := maxBytes
	for cut > 0 && (out[cut]&0xC0) == 0x80 {
		cut--
	}
	return out[:cut]
}

func (s *SQLiteStore) UpsertChunkTask(ctx context.Context, task model.ChunkTask) error {
	// Ensure the caller did not accidentally pass inconsistent IDs.
	if err := task.Validate(); err != nil {
		return err
	}
	if task.Label == 0 {
		return errors.New("task label must be a non-zero positive integer")
	}

	relPath, err := normalizeRelPath(task.Metadata.RelPath)
	if err != nil {
		return err
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()

	_, err = db.ExecContext(
		ctx,
		`INSERT INTO chunks(chunk_id, rel_path, doc_type, rep_type, text, index_kind, embedding_status, embedding_error, error_category, deleted)
		 VALUES(?, ?, ?, ?, ?, ?, 'pending', '', '', 0)
		 ON CONFLICT(chunk_id) DO UPDATE SET
		   rel_path=excluded.rel_path,
		   doc_type=excluded.doc_type,
		   rep_type=excluded.rep_type,
		   text=excluded.text,
		   index_kind=excluded.index_kind,
		   deleted=0,
		   embedding_status='pending',
		   embedding_error='',
		   error_category='',
		   -- Clear the failure stamp alongside the error text it belongs to: a
		   -- row moving back to pending is no longer failed, so leaving the
		   -- timestamp behind would let a future aggregate age the new failure
		   -- from the old one's clock (issue #783).
		   embedding_failed_unix=0`,
		int64(task.Label),
		relPath,
		defaultIfEmpty(task.Metadata.DocType, "unknown"),
		defaultIfEmpty(task.Metadata.RepType, "raw_text"),
		task.Text,
		normalizeIndexKind(task.IndexKind),
	)
	return err
}

func (s *SQLiteStore) UpsertRepresentation(ctx context.Context, rep model.Representation) (int64, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return 0, err
	}
	defer s.ReleaseDB()

	return upsertRepresentationWith(ctx, db, rep)
}

func (s *SQLiteStore) InsertChunkWithSpans(ctx context.Context, chunk model.Chunk, spans []model.Span) (int64, error) {
	if chunk.RepID <= 0 {
		return 0, errors.New("rep_id must be > 0")
	}
	if err := validateChunkText(chunk); err != nil {
		return 0, err
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return 0, err
	}
	defer s.ReleaseDB()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	relPath, docType, repType, language, err := lookupChunkDocContext(ctx, tx, chunk.RepID)
	if err != nil {
		return 0, err
	}
	chunkID, err := insertChunkWithSpansWith(ctx, tx, chunk, spans, relPath, docType, repType, language)
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	return chunkID, nil
}

func (s *SQLiteStore) SoftDeleteChunksFromOrdinal(ctx context.Context, repID int64, fromOrdinal int) error {
	if repID <= 0 {
		return errors.New("rep_id must be > 0")
	}
	if fromOrdinal < 0 {
		return errors.New("from_ordinal must be >= 0")
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()

	_, err = db.ExecContext(
		ctx,
		`UPDATE chunks
		 SET deleted = 1
		 WHERE rep_id = ? AND ordinal >= ?`,
		repID,
		fromOrdinal,
	)
	return err
}

// ClearDocumentContentHashes resets documents.content_hash for all rows.
// Reindex flows can use this to force "changed" semantics even when files are
// unchanged on disk.
func (s *SQLiteStore) ClearDocumentContentHashes(ctx context.Context) error {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()

	_, err = db.ExecContext(ctx, `UPDATE documents SET content_hash = ''`)
	return err
}

// reindexHashBackupTable is the ephemeral table that snapshots
// documents.content_hash before a reindex clears it. It lets an interrupted or
// failed reindex restore the incremental "already indexed" gate instead of
// forcing a full-corpus reprocess on the next sync (issue #418).
const reindexHashBackupTable = "_reindex_hash_backup"

// BackupContentHashes snapshots (doc_id, content_hash) for every document into
// an ephemeral backup table so RestoreContentHashes can undo a subsequent
// ClearDocumentContentHashes if the reindex rebuild is interrupted or fails.
// Any leftover backup from an earlier interrupted run is dropped first so the
// snapshot reflects the current, pre-clear state. Idempotent.
func (s *SQLiteStore) BackupContentHashes(ctx context.Context) error {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()

	present, err := reindexHashBackupPresent(ctx, db)
	if err != nil {
		return err
	}
	if present {
		// Refuse rather than clobber, which is the rule the FILE half of the
		// same staging already follows (reindexStaging.backup). Its comment
		// records why: it used to remove the destination first, "which is
		// exactly how the last-known-good generation got destroyed".
		//
		// The SQL half kept doing that. A snapshot is expected to find the slot
		// free, because RestoreContentHashes consumes any earlier one first. A
		// backup still present here means recovery did not run, and the table
		// by construction holds a COMPLETE pre-clear generation while
		// documents.content_hash may already be cleared by the run that
		// crashed. Overwriting therefore replaced the good snapshot with empty
		// strings, and the next restore put those empty strings back (#807).
		return fmt.Errorf(
			"refusing to overwrite the existing content-hash snapshot in %s: "+
				"it holds an unrecovered generation, so run `dir2mcp reindex` to finish the interrupted recovery first",
			reindexHashBackupTable)
	}
	// One statement, so the snapshot can never be half-taken. The previous
	// drop-then-create pair had a window in which no snapshot existed at all.
	_, err = db.ExecContext(ctx,
		`CREATE TABLE `+reindexHashBackupTable+` AS SELECT doc_id, content_hash FROM documents`)
	return err
}

// RestoreContentHashes restores documents.content_hash from the snapshot taken
// by BackupContentHashes for the backed-up rows, then drops the snapshot. It is
// a no-op when no snapshot exists (never taken, or already discarded), so it is
// safe to call on any failure path and is idempotent.
func (s *SQLiteStore) RestoreContentHashes(ctx context.Context) error {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()

	present, err := reindexHashBackupPresent(ctx, db)
	if err != nil {
		return err
	}
	if !present {
		// No snapshot to restore. This is legitimate when none was ever taken,
		// and it is a FAILURE when a clear has run, because the corpus is then
		// left with no content hashes at all. The caller knows which case it is
		// in, so report the count rather than decide here (#807).
		return ErrNoContentHashSnapshot
	}
	res, err := db.ExecContext(ctx,
		`UPDATE documents
		    SET content_hash = (
		        SELECT b.content_hash FROM `+reindexHashBackupTable+` b
		        WHERE b.doc_id = documents.doc_id
		    )
		  WHERE doc_id IN (SELECT doc_id FROM `+reindexHashBackupTable+`)`)
	if err != nil {
		return err
	}
	restored, _ := res.RowsAffected()
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS `+reindexHashBackupTable); err != nil {
		return err
	}
	if restored == 0 {
		// A snapshot existed and matched nothing. That is not a no-op, it is a
		// restore that failed to restore, and it used to exit 0 in silence.
		return ErrEmptyContentHashSnapshot
	}
	return nil
}

// ErrNoContentHashSnapshot and ErrEmptyContentHashSnapshot let a caller tell
// "there was nothing to restore" apart from "the restore did nothing", which
// the previous silent nil could not express. Both are sentinels rather than
// hard failures: a caller that never took a snapshot treats the first as
// success, while a caller that cleared the hashes must treat either as loud.
var (
	ErrNoContentHashSnapshot    = errors.New("no content-hash snapshot to restore")
	ErrEmptyContentHashSnapshot = errors.New("the content-hash snapshot restored no rows")
)

// DiscardContentHashBackup drops the snapshot taken by BackupContentHashes after
// a durable reindex. Idempotent and safe when no snapshot exists.
func (s *SQLiteStore) DiscardContentHashBackup(ctx context.Context) error {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()

	_, err = db.ExecContext(ctx, `DROP TABLE IF EXISTS `+reindexHashBackupTable)
	return err
}

// reindexHashBackupPresent reports whether the ephemeral content-hash snapshot
// table exists, so restore can be a clean no-op when it does not.
func reindexHashBackupPresent(ctx context.Context, db *sql.DB) (bool, error) {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`,
		reindexHashBackupTable).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ListDocumentHashes returns the (rel_path, content_hash) pair for every
// non-deleted document. It backs retrieval-time cross-file de-duplication
// (SPEC §9.2): the retrieval service maps a hit's rel_path to its content_hash
// to collapse byte-identical duplicate sources. Implements
// model.DocumentHashLister.
func (s *SQLiteStore) ListDocumentHashes(ctx context.Context) ([]model.DocumentHash, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	rows, err := db.QueryContext(ctx,
		`SELECT rel_path, content_hash FROM documents WHERE deleted = 0`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	hashes := make([]model.DocumentHash, 0)
	for rows.Next() {
		var h model.DocumentHash
		if err := rows.Scan(&h.RelPath, &h.ContentHash); err != nil {
			return nil, err
		}
		hashes = append(hashes, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return hashes, nil
}

// ListCorpusLanguages returns the distinct non-empty effective languages
// recorded across non-deleted chunks (SPEC §5.2/§8.8), as BCP-47 tags sorted
// for determinism. It backs the "auto" cross-lingual query-expansion target
// resolution (#325): the retrieval service expands a query into the corpus's
// detected languages (#267). Chunks with no recorded language (unknown) are
// excluded. Implements model.CorpusLanguageLister.
func (s *SQLiteStore) ListCorpusLanguages(ctx context.Context) ([]string, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT language FROM chunks WHERE deleted = 0 AND TRIM(language) != '' ORDER BY language`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	langs := make([]string, 0)
	for rows.Next() {
		var lang string
		if err := rows.Scan(&lang); err != nil {
			return nil, err
		}
		if lang = strings.TrimSpace(lang); lang != "" {
			langs = append(langs, lang)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return langs, nil
}

// WithTx begins a new database transaction and passes a transaction-bound
// representation store to the supplied callback. If the callback returns an
// error the transaction is rolled back; otherwise it is committed.  The
// implementation is specific to SQLite but the interface is used by callers
// such as the representation generator.
func (s *SQLiteStore) WithTx(ctx context.Context, fn func(tx model.RepresentationStore) error) error {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	txStore := &txSQLiteStore{parent: s, tx: tx}
	if err := fn(txStore); err != nil {
		return err
	}
	return tx.Commit()
}

// txSQLiteStore is a lightweight wrapper around SQLiteStore that routes all
// operations through a specific *sql.Tx.  Only the methods needed by
// representationStore are implemented.
type txSQLiteStore struct {
	parent *SQLiteStore
	tx     *sql.Tx
}

// WithTx on a txSQLiteStore is a no-op; the transaction already exists, so
// simply invoke the callback with the receiver itself.
func (t *txSQLiteStore) WithTx(ctx context.Context, fn func(tx model.RepresentationStore) error) error {
	return fn(t)
}

func (t *txSQLiteStore) UpsertRepresentation(ctx context.Context, rep model.Representation) (int64, error) {
	return upsertRepresentationWith(ctx, t.tx, rep)
}

func (t *txSQLiteStore) InsertChunkWithSpans(ctx context.Context, chunk model.Chunk, spans []model.Span) (int64, error) {
	if chunk.RepID <= 0 {
		return 0, errors.New("rep_id must be > 0")
	}
	if err := validateChunkText(chunk); err != nil {
		return 0, err
	}

	relPath, docType, repType, language, err := lookupChunkDocContext(ctx, t.tx, chunk.RepID)
	if err != nil {
		return 0, err
	}
	return insertChunkWithSpansWith(ctx, t.tx, chunk, spans, relPath, docType, repType, language)
}

func (t *txSQLiteStore) SoftDeleteChunksFromOrdinal(ctx context.Context, repID int64, fromOrdinal int) error {
	if repID <= 0 {
		return errors.New("rep_id must be > 0")
	}
	if fromOrdinal < 0 {
		return errors.New("from_ordinal must be >= 0")
	}
	_, err := t.tx.ExecContext(ctx, `UPDATE chunks
	 SET deleted = 1
	 WHERE rep_id = ? AND ordinal >= ?`, repID, fromOrdinal)
	return err
}

func (s *SQLiteStore) GetChunksByRepID(ctx context.Context, repID int64) ([]model.Chunk, error) {
	if repID <= 0 {
		return nil, errors.New("rep_id must be > 0")
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	rows, err := db.QueryContext(
		ctx,
		`SELECT chunk_id, rep_id, ordinal, text, text_hash, index_kind, embedding_status, embedding_error, error_category, deleted
		 FROM chunks
		 WHERE rep_id = ?
		 ORDER BY ordinal ASC`,
		repID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]model.Chunk, 0)
	for rows.Next() {
		var (
			chunk   model.Chunk
			deleted int
		)
		if err := rows.Scan(
			&chunk.ChunkID,
			&chunk.RepID,
			&chunk.Ordinal,
			&chunk.Text,
			&chunk.TextHash,
			&chunk.IndexKind,
			&chunk.EmbeddingStatus,
			&chunk.EmbeddingError,
			&chunk.ErrorCategory,
			&deleted,
		); err != nil {
			return nil, err
		}
		chunk.Deleted = deleted == 1
		out = append(out, chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// TranscriptRepresentation identifies one transcript representation of a
// document together with its persisted meta_json (used to match a requested
// language). It is the lookup unit for subtitle export (#254): a document may
// in principle carry more than one transcript (e.g. distinct languages from a
// sidecar, #253), so callers select among them by language.
type TranscriptRepresentation struct {
	RepID    int64
	MetaJSON string
}

// TranscriptSpanChunk pairs a transcript chunk's text with its (time) span. It
// is the read-side shape consumed by the subtitle cue builder; only active
// (non-deleted) chunks of the representation are returned, ordered by span
// start then end so cues render in playback order even if stored out of order.
type TranscriptSpanChunk struct {
	Text string
	Span model.Span
}

// TranscriptRepresentations returns the active transcript representations of
// the document at relPath, ordered by rep_id for determinism. An empty slice
// (with a nil error) means the document exists but has no transcript. The
// document-missing case is reported as os.ErrNotExist so callers can give a
// precise "no such document" error distinct from "no transcript".
func (s *SQLiteStore) TranscriptRepresentations(ctx context.Context, relPath string) ([]TranscriptRepresentation, error) {
	normalizedPath, err := normalizeRelPath(relPath)
	if err != nil {
		return nil, err
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	// Confirm the document exists first so "no document" and "no transcript"
	// surface as distinct errors to the caller.
	var docID int64
	if err := db.QueryRowContext(
		ctx,
		`SELECT doc_id FROM documents WHERE rel_path = ? AND deleted = 0 LIMIT 1`,
		normalizedPath,
	).Scan(&docID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}

	// Match the bare "transcript" rep_type and any language-suffixed
	// "transcript-<lang>" so per-language sidecar/translated transcripts (each a
	// distinct rep_type under UNIQUE(doc_id, rep_type)) are all surfaced; the
	// caller selects among them by meta_json language.
	rows, err := db.QueryContext(
		ctx,
		`SELECT rep_id, meta_json
		 FROM representations
		 WHERE doc_id = ? AND (rep_type = ? OR rep_type LIKE ?) AND deleted = 0
		 ORDER BY rep_id ASC`,
		docID,
		"transcript",
		"transcript-%",
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]TranscriptRepresentation, 0)
	for rows.Next() {
		var rep TranscriptRepresentation
		if err := rows.Scan(&rep.RepID, &rep.MetaJSON); err != nil {
			return nil, err
		}
		out = append(out, rep)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// RepresentationMetaByType returns the meta_json of the active (non-deleted)
// representation of repType for the document at relPath. It is the read side of
// the derivation-identity re-ingest gate (spec §8.6.7): the ingest service reads
// the stored transcript/OCR representation's recorded provider/model identity
// and compares it to the active model's identity to decide whether the
// representation is stale and must be re-derived. An empty string (with a nil
// error) means the document exists but has no such representation (or it carries
// no meta_json); the document-missing case is reported as os.ErrNotExist so the
// caller can distinguish it. repType is matched exactly, so passing
// "transcript" returns only the bare machine/translated transcript identity row,
// never a language-suffixed sidecar rep_type.
func (s *SQLiteStore) RepresentationMetaByType(ctx context.Context, relPath, repType string) (string, error) {
	normalizedPath, err := normalizeRelPath(relPath)
	if err != nil {
		return "", err
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return "", err
	}
	defer s.ReleaseDB()

	var docID int64
	if err := db.QueryRowContext(
		ctx,
		`SELECT doc_id FROM documents WHERE rel_path = ? AND deleted = 0 LIMIT 1`,
		normalizedPath,
	).Scan(&docID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", os.ErrNotExist
		}
		return "", err
	}

	var metaJSON string
	if err := db.QueryRowContext(
		ctx,
		`SELECT meta_json
		 FROM representations
		 WHERE doc_id = ? AND rep_type = ? AND deleted = 0
		 ORDER BY rep_id ASC
		 LIMIT 1`,
		docID,
		strings.TrimSpace(repType),
	).Scan(&metaJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Document exists but has no representation of this type yet.
			return "", nil
		}
		return "", err
	}
	return metaJSON, nil
}

// RepresentationTypesByPath returns the distinct rep_types of the active
// (non-deleted) representations for the document at relPath, sorted for
// deterministic output. Used by the batch run manifest (SPEC §8.6.11) to record
// the "outputs produced" for an asset. A missing document yields an empty slice
// and a nil error (no outputs, not an error).
func (s *SQLiteStore) RepresentationTypesByPath(ctx context.Context, relPath string) ([]string, error) {
	normalizedPath, err := normalizeRelPath(relPath)
	if err != nil {
		return nil, err
	}
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	rows, err := db.QueryContext(ctx, `
SELECT DISTINCT r.rep_type
  FROM representations r
  JOIN documents d ON d.doc_id = r.doc_id
 WHERE d.rel_path = ? AND d.deleted = 0 AND r.deleted = 0
 ORDER BY r.rep_type ASC`, normalizedPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var types []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		types = append(types, t)
	}
	return types, rows.Err()
}

// SoftDeleteSidecarTranscripts tombstones (deleted = 1) the document's
// sidecar-sourced transcript representations and their chunks, returning the
// number of representations retired. A representation is treated as
// sidecar-sourced when its rep_type is a language-suffixed "transcript-<lang>"
// (per-language sidecars persist under that rep_type) OR its rep_type is the
// bare "transcript" with meta_json source == "sidecar". This lets a forced STT
// reindex clear stale authored sidecar transcripts before writing the fresh
// machine transcript, so retrieval/export never mix the two (spec §8.6.4). The
// document-missing case is reported as os.ErrNotExist for parity with
// TranscriptRepresentations.
func (s *SQLiteStore) SoftDeleteSidecarTranscripts(ctx context.Context, relPath string) (int, error) {
	normalizedPath, err := normalizeRelPath(relPath)
	if err != nil {
		return 0, err
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return 0, err
	}
	defer s.ReleaseDB()

	var docID int64
	if err := db.QueryRowContext(
		ctx,
		`SELECT doc_id FROM documents WHERE rel_path = ? AND deleted = 0 LIMIT 1`,
		normalizedPath,
	).Scan(&docID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, os.ErrNotExist
		}
		return 0, err
	}

	// Identify the sidecar-sourced transcript reps: every "transcript-<lang>"
	// (per-language sidecar rep_type) plus any bare "transcript" whose meta_json
	// declares source == "sidecar". JSON containment is matched with LIKE rather
	// than a JSON function to stay portable across SQLite builds.
	const sidecarSourcePredicate = `(rep_type LIKE 'transcript-%' OR (rep_type = 'transcript' AND meta_json LIKE '%"source":"sidecar"%'))`

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(
		ctx,
		`SELECT rep_id FROM representations WHERE doc_id = ? AND deleted = 0 AND `+sidecarSourcePredicate,
		docID,
	)
	if err != nil {
		return 0, err
	}
	var repIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		repIDs = append(repIDs, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	for _, id := range repIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE chunks SET deleted = 1 WHERE rep_id = ?`, id); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE representations SET deleted = 1 WHERE rep_id = ?`, id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(repIDs), nil
}

// TranscriptSpanChunks returns the active chunks of a transcript representation
// joined to their span rows, ordered by span start then end. Only "time" spans
// carry subtitle timing; the span is reconstructed through the same
// spanFromRow mapping used elsewhere so degraded/legacy rows behave
// consistently. A chunk with no span row is skipped (subtitles require timing).
func (s *SQLiteStore) TranscriptSpanChunks(ctx context.Context, repID int64) ([]TranscriptSpanChunk, error) {
	if repID <= 0 {
		return nil, errors.New("rep_id must be > 0")
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	rows, err := db.QueryContext(
		ctx,
		`SELECT c.text, sp.span_kind, sp.start, sp.end, COALESCE(sp.extra_json, '')
		 FROM chunks c
		 JOIN spans sp ON sp.chunk_id = c.chunk_id
		 WHERE c.rep_id = ? AND c.deleted = 0
		 ORDER BY sp.start ASC, sp.end ASC`,
		repID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]TranscriptSpanChunk, 0)
	for rows.Next() {
		var (
			text      string
			kind      string
			start     int
			end       int
			extraJSON string
		)
		if err := rows.Scan(&text, &kind, &start, &end, &extraJSON); err != nil {
			return nil, err
		}
		out = append(out, TranscriptSpanChunk{
			Text: text,
			Span: spanFromRow(kind, start, end, extraJSON),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ChunkMediaSpanByID resolves a chunk id to its source media (rel_path,
// doc_type) and its time span, for the dir2mcp_open_media_clip tool (SPEC
// §15.11). The chunk's source is read straight from the chunks row (which
// carries rel_path/doc_type) and the span from the joined spans row. A missing
// chunk (or a chunk with no span / a deleted chunk) is reported as
// model.ErrNotFound so the caller can map it to FILE_NOT_FOUND. The returned
// span has whatever kind the chunk carries; callers that require a time span
// validate doc_type/span.Kind themselves.
func (s *SQLiteStore) ChunkMediaSpanByID(ctx context.Context, chunkID int64) (relPath, docType string, span model.Span, err error) {
	if chunkID <= 0 {
		return "", "", model.Span{}, model.ErrNotFound
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return "", "", model.Span{}, err
	}
	defer s.ReleaseDB()

	row := db.QueryRowContext(
		ctx,
		`SELECT c.rel_path, c.doc_type, sp.span_kind, sp.start, sp.end, COALESCE(sp.extra_json, '')
		 FROM chunks c
		 JOIN spans sp ON sp.chunk_id = c.chunk_id
		 WHERE c.chunk_id = ? AND c.deleted = 0
		 ORDER BY sp.start ASC, sp.end ASC
		 LIMIT 1`,
		chunkID,
	)
	var (
		kind      string
		start     int
		end       int
		extraJSON string
	)
	if err := row.Scan(&relPath, &docType, &kind, &start, &end, &extraJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", model.Span{}, model.ErrNotFound
		}
		return "", "", model.Span{}, err
	}
	return relPath, docType, spanFromRow(kind, start, end, extraJSON), nil
}

// ChunkTaskByID returns the full embedding task for a single LIVE chunk
// (SPEC §5.3), with its text, span, modality, and media ref — everything a
// distributed embed-worker (SPEC §8.7) needs to embed a leased job without the
// coordinator relaying payload bytes. It returns model.ErrNotFound when the
// chunk does not exist OR has been tombstoned (deleted=1): a worker treats that
// as a safe skip, honoring the tombstone so a job cannot resurrect a deleted
// chunk (SPEC §6.6, §8.7.3 tombstone safety). textHash is returned separately so
// a worker can detect a job enqueued for a since-changed chunk.
//
// A chunk whose parent document is tombstoned or in the error state is also
// reported as model.ErrNotFound (liveParentDocument, #707). This method is what
// retrieval uses to test whether a vector hit is still live, so the predicate
// keeps a failed document's earlier chunks out of search results, out of the
// related-document neighbours, and out of the answer context. The retrieval
// liveness pass also evicts each such chunk from the in-memory vector index, so
// the hit does not come back on the next query. A document that indexes again
// gets a fresh set of chunks, which the same pass then admits again. The embed
// worker sees the same answer, which matches NextPending: it does not embed a
// chunk of a failed document.
func (s *SQLiteStore) ChunkTaskByID(ctx context.Context, chunkID uint64) (task model.ChunkTask, textHash string, err error) {
	if chunkID == 0 {
		return model.ChunkTask{}, "", model.ErrNotFound
	}
	db, err := s.ensureDB(ctx)
	if err != nil {
		return model.ChunkTask{}, "", err
	}
	defer s.ReleaseDB()

	row := db.QueryRowContext(ctx, `
WITH the_chunk AS (
  SELECT chunk_id, rel_path, doc_type, rep_type, text, text_hash, index_kind, modality, media_ref, language, chunk_context
  FROM chunks
  WHERE chunk_id = ? AND deleted = 0
),
ranked_spans AS (
  SELECT s.chunk_id, s.span_kind, s.start, s."end", s.extra_json,
         ROW_NUMBER() OVER (PARTITION BY s.chunk_id ORDER BY s.span_id) AS rn
  FROM spans s
  JOIN the_chunk tc ON tc.chunk_id = s.chunk_id
)
SELECT tc.chunk_id, tc.rel_path, tc.doc_type, tc.rep_type, tc.text, tc.text_hash, tc.index_kind, tc.modality, tc.media_ref, tc.language, tc.chunk_context,
       COALESCE(sp.span_kind, ''), COALESCE(sp.start, 0), COALESCE(sp.end, 0), COALESCE(sp.extra_json, ''), COALESCE(d.mtime_unix, 0)
FROM the_chunk tc
LEFT JOIN ranked_spans sp ON sp.chunk_id = tc.chunk_id AND sp.rn = 1
LEFT JOIN documents d ON d.rel_path = tc.rel_path
WHERE `+liveParentDocument, int64(chunkID))

	var (
		cid       int64
		relPath   string
		docType   string
		repType   string
		text      string
		thash     string
		idxKind   string
		modality  string
		mediaRef  string
		language  string
		chunkCtx  string
		spanK     string
		spanS     int
		spanE     int
		spanExtra string
		mtimeUnix int64
	)
	if scanErr := row.Scan(&cid, &relPath, &docType, &repType, &text, &thash, &idxKind, &modality, &mediaRef, &language, &chunkCtx,
		&spanK, &spanS, &spanE, &spanExtra, &mtimeUnix); scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return model.ChunkTask{}, "", model.ErrNotFound
		}
		return model.ChunkTask{}, "", scanErr
	}
	if cid <= 0 {
		return model.ChunkTask{}, "", model.ErrNotFound
	}
	uid := uint64(cid)
	span := spanFromRow(spanK, spanS, spanE, spanExtra)
	t := model.NewChunkTask(uid, text, idxKind, model.ChunkMetadata{
		ChunkID:   uid,
		RelPath:   relPath,
		DocType:   docType,
		RepType:   repType,
		Snippet:   snippet(text, 240),
		Span:      span,
		Modality:  modality,
		MediaRef:  mediaRef,
		Language:  language,
		MTimeUnix: mtimeUnix,
	})
	t.Modality = modality
	t.MediaRef = mediaRef
	// Payload identity (SPEC §5.3). It is also returned separately, which is the
	// form the distributed worker compares against its leased job (#710); the
	// field keeps a task loaded here interchangeable with one from NextPending.
	t.TextHash = thash
	// The generated context rides on the task's Context field only — Text stays
	// the raw chunk, so a caller that renders a snippet/citation from this task
	// (reranking, liveness) can never surface it (SPEC §8.1.8, #403). Only the
	// embed path reads it, via ChunkTask.EmbedInput.
	t.Context = chunkCtx
	return t, thash, nil
}

// EmbeddedChunksByPath returns the embedded (status='ok'), non-deleted chunks of
// one document (corpus-relative rel_path), ordered by chunk_id. It is the store
// half of dir2mcp_related's (SPEC §15.12) rel_path seed: the tool aggregates the
// returned chunks' vectors as the query-by-example seed and excludes every one of
// them from the neighbours (a document is never related to itself). A rel_path
// that resolves to no embedded chunk returns an empty slice (the tool maps that
// to INVALID_FIELD — the source could not be located).
//
// The chunks must also have a live parent document (liveParentDocument, #707).
// A document that is tombstoned or in the error state therefore returns no
// chunks, so it cannot seed a related-document query with content that the
// corpus reports as failed.
func (s *SQLiteStore) EmbeddedChunksByPath(ctx context.Context, relPath string) ([]model.ChunkTask, error) {
	normalizedPath, err := normalizeRelPath(relPath)
	if err != nil {
		return nil, err
	}
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	rows, err := db.QueryContext(ctx, `
WITH filtered_chunks AS (
  SELECT c.chunk_id, c.rel_path, c.doc_type, c.rep_type, c.text, c.index_kind, c.modality, c.media_ref, c.language
  FROM chunks c
  WHERE c.rel_path = ? AND c.embedding_status = 'ok' AND c.deleted = 0
),
ranked_spans AS (
  SELECT s.chunk_id, s.span_kind, s.start, s."end", s.extra_json,
         ROW_NUMBER() OVER (PARTITION BY s.chunk_id ORDER BY s.span_id) AS rn
  FROM spans s
  JOIN filtered_chunks fc ON fc.chunk_id = s.chunk_id
)
SELECT fc.chunk_id, fc.rel_path, fc.doc_type, fc.rep_type, fc.text, fc.index_kind,
       COALESCE(sp.span_kind, ''), COALESCE(sp.start, 0), COALESCE(sp.end, 0), COALESCE(sp.extra_json, ''),
       COALESCE(d.title, ''), fc.modality, fc.media_ref, fc.language, COALESCE(d.mtime_unix, 0)
FROM filtered_chunks fc
LEFT JOIN ranked_spans sp ON sp.chunk_id = fc.chunk_id AND sp.rn = 1
LEFT JOIN documents d ON d.rel_path = fc.rel_path
WHERE `+liveParentDocument+`
ORDER BY fc.chunk_id`, normalizedPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []model.ChunkTask
	for rows.Next() {
		var (
			chunkID   int64
			rp        string
			docType   string
			repType   string
			text      string
			kind      string
			spanK     string
			spanS     int
			spanE     int
			spanExtra string
			title     string
			modality  string
			mediaRef  string
			language  string
			mtimeUnix int64
		)
		if err := rows.Scan(&chunkID, &rp, &docType, &repType, &text, &kind, &spanK, &spanS, &spanE, &spanExtra, &title, &modality, &mediaRef, &language, &mtimeUnix); err != nil {
			return nil, err
		}
		if chunkID <= 0 {
			return nil, fmt.Errorf("invalid non-positive chunk_id from database: %d", chunkID)
		}
		uid := uint64(chunkID)
		span := spanFromRow(spanK, spanS, spanE, spanExtra)
		out = append(out, model.ChunkTask{
			Label:     uid,
			Text:      text,
			IndexKind: kind,
			Modality:  modality,
			MediaRef:  mediaRef,
			Metadata: model.ChunkMetadata{
				ChunkID:   uid,
				RelPath:   rp,
				Title:     title,
				DocType:   docType,
				RepType:   repType,
				Snippet:   snippet(text, 240),
				Span:      span,
				Modality:  modality,
				MediaRef:  mediaRef,
				Language:  language,
				MTimeUnix: mtimeUnix,
			},
		})
	}
	return out, rows.Err()
}

func (s *SQLiteStore) MarkDocumentDeleted(ctx context.Context, relPath string) error {
	normalizedPath, err := normalizeRelPath(relPath)
	if err != nil {
		return err
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `UPDATE documents SET deleted = 1 WHERE rel_path = ?`, normalizedPath); err != nil {
		return err
	}
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE representations SET deleted = 1
		 WHERE doc_id IN (SELECT doc_id FROM documents WHERE rel_path = ?)`,
		normalizedPath,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE chunks SET deleted = 1
		 WHERE rep_id IN (
			SELECT rep_id FROM representations
			WHERE doc_id IN (SELECT doc_id FROM documents WHERE rel_path = ?)
		 )`,
		normalizedPath,
	); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *SQLiteStore) SetSetting(ctx context.Context, key, value string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("setting key is required")
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()

	_, err = db.ExecContext(
		ctx,
		`INSERT INTO settings(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key,
		value,
	)
	return err
}

func (s *SQLiteStore) GetSetting(ctx context.Context, key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("setting key is required")
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return "", err
	}
	defer s.ReleaseDB()

	var value string
	err = db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ? LIMIT 1`, key).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", os.ErrNotExist
		}
		return "", err
	}
	return value, nil
}

func (s *SQLiteStore) GetDocumentByPath(ctx context.Context, relPath string) (model.Document, error) {
	normalizedPath, err := normalizeRelPath(relPath)
	if err != nil {
		return model.Document{}, err
	}

	// Read pool (#429 F11): recency decay calls this once per unique rel_path per
	// query, so on the writer handle it queued behind in-flight ingest writes.
	db, err := s.ensureReadDB(ctx)
	if err != nil {
		return model.Document{}, err
	}
	defer s.ReleaseDB()

	var doc model.Document
	var deleted int
	row := db.QueryRowContext(
		ctx,
		`SELECT doc_id, rel_path, doc_type, source_type, size_bytes, mtime_unix, content_hash, etag, sidecar_fingerprint, status, deleted, title, error_message, skip_reason
		 FROM documents WHERE rel_path = ?`,
		normalizedPath,
	)
	if err := row.Scan(
		&doc.DocID,
		&doc.RelPath,
		&doc.DocType,
		&doc.SourceType,
		&doc.SizeBytes,
		&doc.MTimeUnix,
		&doc.ContentHash,
		&doc.ETag,
		&doc.SidecarFingerprint,
		&doc.Status,
		&deleted,
		&doc.Title,
		&doc.ErrorMessage,
		&doc.SkipReason,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Document{}, os.ErrNotExist
		}
		return model.Document{}, err
	}
	doc.Deleted = deleted == 1
	return doc, nil
}

// ListFiles is the model.Store listing: every non-deleted matching document,
// hidden dot-paths included. It is a thin delegate so the interface signature
// (and its ~30 callers) stay unchanged; callers that need the list_files
// visibility policy applied inside the query call ListVisibleFiles directly.
func (s *SQLiteStore) ListFiles(ctx context.Context, prefix, glob string, limit, offset int) ([]model.Document, int64, error) {
	return s.ListVisibleFiles(ctx, prefix, glob, limit, offset, true)
}

// ListVisibleFiles is ListFiles with the list_files hidden-path policy pushed
// into SQL (#694).
//
// The MCP handler used to apply that policy in Go, which forced it to walk the
// whole matching corpus from offset zero on every single call just to know how
// many rows survived. The policy is "hide a path when ANY segment starts with a
// dot" (#693), and relpath.NotHiddenSQL is that rule in SQL. It sits beside
// relpath.IsHidden, the Go form the handler's walk fallback uses, so the two
// cannot drift apart.
//
// Adding it to `where` is what makes this worth doing: the same slice is
// threaded through whereClause into the SQL LIMIT/OFFSET page query, into
// countListFiles, and into the glob scan, so all three start operating on the
// visible set and the caller can trust both the page AND the total without
// re-filtering.
//
// The memo key must carry the visibility policy. Both listings share the same
// prefix/glob filter but describe different result sets, so keying only on the
// filter would let a hidden-excluding call serve its total to a
// hidden-including one (and vice versa) for as long as no commit landed.
func (s *SQLiteStore) ListVisibleFiles(ctx context.Context, prefix, glob string, limit, offset int, includeHidden bool) ([]model.Document, int64, error) {
	// Read pool (#429 F11): SELECT-only, and the glob branch scans every matching
	// row per page, so this is exactly the shape that should not block on ingest.
	db, err := s.ensureReadDB(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer s.ReleaseDB()

	if limit <= 0 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}

	normalizedPrefix := model.NormalizePathPrefix(prefix)
	trimmedGlob := strings.TrimSpace(glob)

	selectCols := `SELECT doc_id, rel_path, doc_type, source_type, size_bytes, mtime_unix, content_hash, etag, sidecar_fingerprint, status, deleted, title, error_message, skip_reason FROM documents`
	where := []string{"deleted = 0"}
	prefixArgs := make([]any, 0, 1)
	if normalizedPrefix != "" {
		where = append(where, `rel_path LIKE ? ESCAPE '\'`)
		prefixArgs = append(prefixArgs, escapeLike(normalizedPrefix)+"%")
	}
	if !includeHidden {
		where = append(where, relpath.NotHiddenSQL("rel_path"))
	}
	whereClause := " WHERE " + strings.Join(where, " AND ")

	// Part of every memo key: the two visibility policies are two different
	// listings over the same filter, and their totals must never be crossed.
	visibility := "all"
	if !includeHidden {
		visibility = "visible"
	}

	// The `glob` filter uses the SAME canonical matcher as the search/ask
	// `file_glob` filter (model.MatchGlob, issue #441): segment-aware `*`,
	// recursive `**`, ASCII case-insensitive. SQLite `GLOB` cannot express those
	// semantics (its `*` crosses `/` and it has no `**`), so when a glob is set we
	// evaluate it in Go over the prefix-matched rows and paginate in Go. Without a
	// glob the query keeps its efficient SQL LIMIT/OFFSET pagination unchanged.
	if trimmedGlob == "" {
		key := "prefix\x00" + visibility + "\x00" + normalizedPrefix
		return s.listFilesSQLPaged(ctx, db, selectCols, whereClause, prefixArgs, key, limit, offset)
	}

	matcher, err := model.CompileGlob(trimmedGlob)
	if err != nil {
		// A malformed glob matches nothing rather than erroring, mirroring the
		// search/ask side; list_files with no match is not an error.
		return []model.Document{}, 0, nil
	}

	// The cache key is the full filter: whereClause/prefixArgs are a pure
	// function of normalizedPrefix and the visibility policy, so visibility plus
	// prefix plus glob identifies the listing.
	key := "glob\x00" + visibility + "\x00" + normalizedPrefix + "\x00" + trimmedGlob
	return s.listFilesGlobPaged(ctx, db, selectCols, whereClause, prefixArgs, matcher, key, limit, offset)
}

// listFilesSQLPaged runs the glob-free ListFiles path: prefix filtering plus
// SQL-side ORDER BY / LIMIT / OFFSET pagination and a matching total.
//
// The total is obtained, in order of cost:
//  1. from the page itself when the page came back short (that proves where the
//     result set ends, so no COUNT is needed);
//  2. from the memo, while no commit has landed since it was computed (see
//     listFilesCache: the guard is SQLite's own data_version, so a reused total
//     is a count of exactly the corpus the caller is looking at, not a stale
//     count of an older one);
//  3. from `SELECT COUNT(*)`, as before.
//
// Before #429 F10 step 3 ran on every page, so walking N documents cost O(N)
// full count scans.
//
// Exactness is unchanged by the memo: the total is exact as of a database state
// observed during this call. Ingest committing between the page query and the
// total can still make the two describe adjacent states, exactly as it could
// when every page ran its own COUNT after its own page query.
func (s *SQLiteStore) listFilesSQLPaged(
	ctx context.Context,
	db *sql.DB,
	selectCols, whereClause string,
	prefixArgs []any,
	key string,
	limit, offset int,
) ([]model.Document, int64, error) {
	// Probe before the page query so a memo hit is pinned to a state no newer
	// than the rows it is returned with.
	probe := s.versionProbeDB()
	epoch, cacheable := s.listCache.begin(ctx, probe)

	var memo listFilesCacheEntry
	memoized := false
	if cacheable {
		memo, memoized = s.listCache.lookup(epoch, key)
	}
	if memoized && int64(offset) >= memo.total {
		// The memo proves the listing ends before this offset, so the page query
		// would only make SQLite walk rows to discard them all.
		return []model.Document{}, memo.total, nil
	}

	query := selectCols + whereClause + " ORDER BY rel_path LIMIT ? OFFSET ?"
	args := append(append([]any{}, prefixArgs...), limit, offset)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	docs := make([]model.Document, 0, limit)
	for rows.Next() {
		doc, scanErr := scanListFilesRow(rows)
		if scanErr != nil {
			return nil, 0, scanErr
		}
		docs = append(docs, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	if memoized {
		return docs, memo.total, nil
	}

	total, exact := derivedListTotal(len(docs), limit, offset)
	if !exact {
		total, err = s.countListFiles(ctx, db, whereClause, prefixArgs)
		if err != nil {
			return nil, 0, err
		}
	}
	if cacheable {
		s.listCache.store(ctx, probe, epoch, key, listFilesCacheEntry{total: total})
	}
	return docs, total, nil
}

// derivedListTotal reports the total a page proves on its own. A page that came
// back with fewer rows than the limit ended the result set, so the total is
// exactly offset+len(page) and no COUNT is needed. A full page may or may not
// have more rows behind it, and an empty page at a non-zero offset proves
// nothing (the offset may simply be past the end); both report false.
func derivedListTotal(pageLen, limit, offset int) (int64, bool) {
	if pageLen >= limit {
		return 0, false
	}
	if pageLen == 0 && offset > 0 {
		return 0, false
	}
	return int64(offset + pageLen), true
}

// countListFiles runs the exact COUNT(*) for a ListFiles filter.
func (s *SQLiteStore) countListFiles(
	ctx context.Context,
	db *sql.DB,
	whereClause string,
	prefixArgs []any,
) (int64, error) {
	s.listCountQueries.Add(1)
	var total int64
	countQuery := "SELECT COUNT(*) FROM documents" + whereClause
	if err := db.QueryRowContext(ctx, countQuery, prefixArgs...).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

// listFilesGlobPaged runs the glob ListFiles path. The glob is evaluated in Go
// (SQLite GLOB cannot express the canonical segment-aware/`**` semantics), so a
// page cannot be expressed as SQL LIMIT/OFFSET.
//
// Before #429 F10 that meant every page rescanned and re-globbed every
// prefix-matched row, making a walk quadratic. Now the first page's scan also
// records the exact total and a sparse resume index, and later pages of the same
// listing resume from the nearest recorded boundary and stop as soon as the page
// is full. The memo (total and index alike) is dropped by the first commit from
// any writer, so a resumed page is over exactly the rows the full scan saw and
// the total is a count of that same corpus; see listFilesCache for the full
// exactness contract.
func (s *SQLiteStore) listFilesGlobPaged(
	ctx context.Context,
	db *sql.DB,
	selectCols, whereClause string,
	prefixArgs []any,
	matcher *model.CompiledGlob,
	key string,
	limit, offset int,
) ([]model.Document, int64, error) {
	probe := s.versionProbeDB()
	epoch, cacheable := s.listCache.begin(ctx, probe)
	if cacheable {
		if entry, ok := s.listCache.lookup(epoch, key); ok {
			if int64(offset) >= entry.total {
				// The memo proves the listing ends before this offset, so scanning
				// would only re-glob rows to discard them all.
				return []model.Document{}, entry.total, nil
			}
			start, skip := entry.startFor(offset)
			docs, err := s.scanGlobPage(ctx, db, selectCols, whereClause, prefixArgs, matcher, start, skip, limit)
			if err != nil {
				return nil, 0, err
			}
			return docs, entry.total, nil
		}
	}

	docs, entry, err := s.scanGlobAll(ctx, db, selectCols, whereClause, prefixArgs, matcher, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	if cacheable {
		s.listCache.store(ctx, probe, epoch, key, entry)
	}
	return docs, entry.total, nil
}

// scanGlobAll walks every prefix-matched row once, returning the requested page
// plus the cache entry (exact total + sparse resume index) for the listing.
func (s *SQLiteStore) scanGlobAll(
	ctx context.Context,
	db *sql.DB,
	selectCols, whereClause string,
	prefixArgs []any,
	matcher *model.CompiledGlob,
	limit, offset int,
) ([]model.Document, listFilesCacheEntry, error) {
	s.listGlobFullScans.Add(1)

	query := selectCols + whereClause + " ORDER BY rel_path"
	rows, err := db.QueryContext(ctx, query, prefixArgs...)
	if err != nil {
		return nil, listFilesCacheEntry{}, err
	}
	defer func() { _ = rows.Close() }()

	bounds := newGlobBounds(limit)
	matched := make([]model.Document, 0, limit)
	var total int64
	for rows.Next() {
		doc, scanErr := scanListFilesRow(rows)
		if scanErr != nil {
			return nil, listFilesCacheEntry{}, scanErr
		}
		if !matcher.Match(doc.RelPath) {
			continue
		}
		bounds.observe(total, doc.RelPath)
		// Only materialize the requested page; earlier rows advance offset and
		// later rows only bump the total count.
		if int64(offset) <= total && len(matched) < limit {
			matched = append(matched, doc)
		}
		total++
	}
	if err := rows.Err(); err != nil {
		return nil, listFilesCacheEntry{}, err
	}
	return matched, bounds.entry(total), nil
}

// scanGlobPage materializes one page of an already-counted glob listing: it
// resumes at start (a boundary recorded by scanGlobAll), discards skip further
// matches, and stops the moment the page is full.
func (s *SQLiteStore) scanGlobPage(
	ctx context.Context,
	db *sql.DB,
	selectCols, whereClause string,
	prefixArgs []any,
	matcher *model.CompiledGlob,
	start string,
	skip, limit int,
) ([]model.Document, error) {
	s.listGlobPageScans.Add(1)

	query := selectCols + whereClause
	args := append([]any{}, prefixArgs...)
	if start != "" {
		query += ` AND rel_path >= ?`
		args = append(args, start)
	}
	query += " ORDER BY rel_path"

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	matched := make([]model.Document, 0, limit)
	for rows.Next() {
		doc, scanErr := scanListFilesRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		if !matcher.Match(doc.RelPath) {
			continue
		}
		if skip > 0 {
			skip--
			continue
		}
		matched = append(matched, doc)
		if len(matched) >= limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return matched, nil
}

// scanListFilesRow scans one documents row into a model.Document for ListFiles.
func scanListFilesRow(rows *sql.Rows) (model.Document, error) {
	var doc model.Document
	var deleted int
	if err := rows.Scan(
		&doc.DocID,
		&doc.RelPath,
		&doc.DocType,
		&doc.SourceType,
		&doc.SizeBytes,
		&doc.MTimeUnix,
		&doc.ContentHash,
		&doc.ETag,
		&doc.SidecarFingerprint,
		&doc.Status,
		&deleted,
		&doc.Title,
		&doc.ErrorMessage,
		&doc.SkipReason,
	); err != nil {
		return model.Document{}, err
	}
	doc.Deleted = deleted == 1
	return doc, nil
}

// RecentFailures returns up to limit documents that ended ingest with
// status='error', newest-first by mtime_unix and rel_path-stable for
// ties (so callers see deterministic output across runs). Deleted rows
// are excluded — they're tombstones, not actionable failures. Backs
// the optional `recent_failures` field on dir2mcp_stats (SPEC §15.6).
//
// limit <= 0 is treated as the recommended default of 20 per the spec.
// Returned documents carry the same per-row fields ListFiles emits;
// callers should project to whatever shape they need.
func (s *SQLiteStore) RecentFailures(ctx context.Context, limit int) ([]model.Document, error) {
	if limit <= 0 {
		limit = 20
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	rows, err := db.QueryContext(
		ctx,
		`SELECT doc_id, rel_path, doc_type, source_type, size_bytes, mtime_unix, content_hash, etag, sidecar_fingerprint, status, deleted, title, error_message, skip_reason
		 FROM documents
		 WHERE status = 'error' AND deleted = 0
		 ORDER BY mtime_unix DESC, rel_path ASC
		 LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]model.Document, 0, limit)
	for rows.Next() {
		var doc model.Document
		var deleted int
		if err := rows.Scan(
			&doc.DocID,
			&doc.RelPath,
			&doc.DocType,
			&doc.SourceType,
			&doc.SizeBytes,
			&doc.MTimeUnix,
			&doc.ContentHash,
			&doc.ETag,
			&doc.SidecarFingerprint,
			&doc.Status,
			&deleted,
			&doc.Title,
			&doc.ErrorMessage,
			&doc.SkipReason,
		); err != nil {
			return nil, err
		}
		doc.Deleted = deleted == 1
		out = append(out, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ActiveDocCounts returns active-document counts grouped by doc_type along
// with the total active document count using aggregate SQL queries.  The
// implementation simply obtains a database handle and delegates to
// activeDocCountsWith so that callers holding an open handle can reuse it.
func (s *SQLiteStore) ActiveDocCounts(ctx context.Context) (map[string]int64, int64, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer s.ReleaseDB()

	return activeDocCountsWith(ctx, db)
}

// CorpusStats returns aggregate corpus/indexing counters derived from SQLite.
// It performs several independent SQL queries (counting documents by type,
// scanning/indexed/skipped/etc., representations, chunks, etc.) outside of a
// single transaction or snapshot. Under concurrent writes the results may be
// slightly inconsistent – for example TotalDocs (which comes from
// ActiveDocCounts) might not exactly equal Indexed+Skipped+Errors from the
// lifecycle query. That's acceptable since these stats are intended for
// monitoring and CLI status fallbacks. Callers requiring strict consistency
// would need to run the queries in one transaction or acquire a snapshot/lock.
func (s *SQLiteStore) CorpusStats(ctx context.Context) (model.CorpusStats, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return model.CorpusStats{}, err
	}
	defer s.ReleaseDB()

	stats := model.CorpusStats{}

	// fetch doc counts using helper so we don't reopen the database handle
	counts, total, err := activeDocCountsWith(ctx, db)
	if err != nil {
		return model.CorpusStats{}, err
	}
	stats.DocCounts = counts
	stats.TotalDocs = total

	err = db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) AS scanned,
			COALESCE(SUM(CASE WHEN deleted = 0 AND status = 'ok' THEN 1 ELSE 0 END), 0) AS indexed,
			COALESCE(SUM(CASE WHEN deleted = 0 AND status IN ('skipped', 'secret_excluded') THEN 1 ELSE 0 END), 0) AS skipped,
			COALESCE(SUM(CASE WHEN deleted = 1 THEN 1 ELSE 0 END), 0) AS deleted,
			COALESCE(SUM(CASE WHEN deleted = 0 AND status = 'error' THEN 1 ELSE 0 END), 0) AS errors
		FROM documents`,
	).Scan(&stats.Scanned, &stats.Indexed, &stats.Skipped, &stats.Deleted, &stats.Errors)
	if err != nil {
		return model.CorpusStats{}, err
	}

	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM representations WHERE deleted = 0`).Scan(&stats.Representations); err != nil {
		return model.CorpusStats{}, err
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks WHERE deleted = 0`).Scan(&stats.ChunksTotal); err != nil {
		return model.CorpusStats{}, err
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks WHERE deleted = 0 AND embedding_status = 'ok'`).Scan(&stats.EmbeddedOK); err != nil {
		return model.CorpusStats{}, err
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks WHERE deleted = 0 AND embedding_status = 'pending'`).Scan(&stats.EmbeddedPending); err != nil {
		return model.CorpusStats{}, err
	}

	summary, err := loadFailureSummary(ctx, db, failureSummaryMaxSamples)
	if err != nil {
		return model.CorpusStats{}, err
	}
	stats.FailureSummary = summary

	skipSummary, err := loadSkipSummary(ctx, db, skipSummaryMaxSamples)
	if err != nil {
		return model.CorpusStats{}, err
	}
	stats.SkipSummary = skipSummary

	return stats, nil
}

// nextPendingQuery builds the SQL for one embed batch.
//
// Both selective predicates and the LIMIT sit INSIDE filtered_chunks. #732
// fixed the same shape in embeddedChunkPageQuery; this one is #743. With
// index_kind and the LIMIT outside the CTE, every call materialised the whole
// pending set before discarding all but `limit` rows. That is not quadratic the
// way the metadata walk was, because there is no cursor to compound it, but it
// runs once per embed cycle for the life of an ingest: a deep queue paid a full
// scan of everything still pending on every batch.
//
// The first span comes from a correlated MIN(span_id) rather than a
// ROW_NUMBER() window for the reason #742 measured: with the LIMIT pushed down,
// the window made SQLite drive the join off a full scan of `spans`, which cost
// more than the CTE fix saved.
//
// The documents LEFT JOIN stays inside the CTE because it is a predicate, not
// decoration: a chunk whose parent document is errored or tombstoned must not
// be handed to the embed worker. The NULL guard preserves a chunk with no
// document row (UpsertChunkTask seeds bare chunks).
//
// idx_chunks_embedded_kind_seek (embedding_status, deleted, index_kind,
// chunk_id), added by #742, already covers this access path, so no new index.
func nextPendingQuery(indexKind string, limit int) (string, []any) {
	args := []any{"pending"}
	kindPredicate := ""
	if strings.TrimSpace(indexKind) != "" {
		kindPredicate = " AND c.index_kind = ?"
	}
	query := `WITH filtered_chunks AS (
	            SELECT c.chunk_id, c.rel_path, c.doc_type, c.rep_type, c.text, c.text_hash, c.index_kind, c.modality, c.media_ref, c.language,
	                   c.chunk_context,
	                   COALESCE(d.mtime_unix, 0) AS mtime_unix
	            FROM chunks c
	            LEFT JOIN documents d ON d.rel_path = c.rel_path
	            WHERE c.embedding_status = ? AND c.deleted = 0 AND c.chunk_id > 0
	              AND ` + liveParentDocument + kindPredicate + `
	            ORDER BY c.chunk_id
	            LIMIT ?
	          )
	          SELECT fc.chunk_id, fc.rel_path, fc.doc_type, fc.rep_type, fc.text, fc.text_hash, fc.index_kind, fc.modality, fc.media_ref, fc.language,
	                 fc.chunk_context,
	                 COALESCE(sp.span_kind, ''), COALESCE(sp.start, 0), COALESCE(sp."end", 0), COALESCE(sp.extra_json, ''), fc.mtime_unix
	          FROM filtered_chunks fc
	          LEFT JOIN spans sp ON sp.span_id = (
	            SELECT MIN(s.span_id) FROM spans s WHERE s.chunk_id = fc.chunk_id
	          )
	          ORDER BY fc.chunk_id`
	if kindPredicate != "" {
		args = append(args, indexKind)
	}
	args = append(args, limit)
	return query, args
}

// NextPendingQueryForTest returns the SQL one embed batch runs.
//
// Exported solely so the #743 regression test can live under tests/ as
// AGENTS.md requires. As #732 established, a query-plan assertion is not a
// sufficient guard: with the predicates outside the CTE SQLite still picks a
// reasonable index, so a plan-only test passes against unfixed code. What has
// to be pinned is WHERE in the statement the predicates and the LIMIT sit.
// Production code never calls this.
func NextPendingQueryForTest(indexKind string, limit int) (string, []any) {
	return nextPendingQuery(indexKind, limit)
}

// ExplainNextPendingForTest returns the SQLite query plan (one string per plan
// row) for a single embed batch.
//
// Exported solely so the #743 regression test can live under tests/ as
// AGENTS.md requires: the property worth pinning is that a batch SEEKS its rows
// through an index rather than scanning the whole chunks table, which is not
// observable through the ordinary store API. Production code never calls this.
func (s *SQLiteStore) ExplainNextPendingForTest(ctx context.Context, indexKind string, limit int) ([]string, error) {
	db, err := s.ensureReadDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	query, args := nextPendingQuery(indexKind, limit)
	rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			return nil, err
		}
		plan = append(plan, detail)
	}
	return plan, rows.Err()
}

func (s *SQLiteStore) NextPending(ctx context.Context, limit int, indexKind string) ([]model.ChunkTask, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()
	if limit <= 0 {
		limit = 32
	}

	query, args := nextPendingQuery(indexKind, limit)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	tasks := make([]model.ChunkTask, 0, limit)
	for rows.Next() {
		var (
			chunkID   int64
			relPath   string
			docType   string
			repType   string
			text      string
			textHash  string
			idxKind   string
			modality  string
			mediaRef  string
			language  string
			chunkCtx  string
			spanK     string
			spanS     int
			spanE     int
			spanExtra string
			mtimeUnix int64
		)
		if err := rows.Scan(&chunkID, &relPath, &docType, &repType, &text, &textHash, &idxKind, &modality, &mediaRef, &language, &chunkCtx, &spanK, &spanS, &spanE, &spanExtra, &mtimeUnix); err != nil {
			return nil, err
		}
		if chunkID <= 0 {
			return nil, fmt.Errorf("invalid non-positive chunk_id from database: %d", chunkID)
		}
		uid := uint64(chunkID)
		span := spanFromRow(spanK, spanS, spanE, spanExtra)
		task := model.NewChunkTask(uid, text, idxKind, model.ChunkMetadata{
			ChunkID:   uid,
			RelPath:   relPath,
			DocType:   docType,
			RepType:   repType,
			Snippet:   snippet(text, 240),
			Span:      span,
			Modality:  modality,
			MediaRef:  mediaRef,
			Language:  language,
			MTimeUnix: mtimeUnix,
		})
		task.Modality = modality
		task.MediaRef = mediaRef
		// Payload identity (SPEC §5.3/§8.7.2): the distributed coordinator stamps
		// this onto the job it enqueues so a worker can recognise a job it leases
		// for a chunk whose bytes have since been replaced in place (#710).
		task.TextHash = textHash
		// Contextual retrieval (SPEC §8.1.8): the generated context travels as a
		// SEPARATE field so only the embed path (ChunkTask.EmbedInput) joins it to
		// the chunk. Snippet above is built from the raw text, so nothing the
		// generator wrote can reach a citation (#403).
		task.Context = chunkCtx
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// ChunkModalityPresence reports, for a document's live chunks, whether any is a
// direct media chunk (modality not text) and whether any is text-bearing
// (modality text/empty). It lets open_file distinguish a permanent media-only
// document (MEDIA_NO_TEXT, SPEC 8.1.7) from a binary document whose OCR/
// transcript is merely pending (OCR_NOT_READY). A document with no chunks yields
// (false, false).
func (s *SQLiteStore) ChunkModalityPresence(ctx context.Context, relPath string) (hasMedia, hasText bool, err error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return false, false, err
	}
	defer s.ReleaseDB()

	const q = `SELECT
	             COALESCE(MAX(CASE WHEN modality NOT IN ('text', '') THEN 1 ELSE 0 END), 0),
	             COALESCE(MAX(CASE WHEN modality IN ('text', '') THEN 1 ELSE 0 END), 0)
	           FROM chunks
	           WHERE rel_path = ? AND deleted = 0`
	var m, t int
	if err := db.QueryRowContext(ctx, q, relPath).Scan(&m, &t); err != nil {
		return false, false, err
	}
	return m == 1, t == 1, nil
}

// embeddedChunkPageQuery builds one page of the ListEmbeddedChunkMetadata walk
// together with its positional arguments.
//
// Both selective predicates (the index_kind filter and the page LIMIT) live
// INSIDE filtered_chunks, which is what keeps a page's cost bounded (#732).
// They used to sit in the outer query. SQLite pushes an outer equality term like
// index_kind down on its own, but it can never push a LIMIT into a subquery, so
// the CTE (and the ROW_NUMBER window materialized over its spans) still covered
// EVERY embedded chunk above the cursor and the outer LIMIT threw all but one
// page of it away: a full keyset walk therefore cost O(N^2 / pageSize). Written
// this way the candidate scan itself stops at one page, and the walk is linear in
// the corpus. idx_chunks_embedded_kind_seek makes the kind-scoped page a pure
// index seek; without a kind, the leading embedding_status column of
// idx_chunks_embedding_status plus the implicit rowid serves the same seek.
//
// An explicit ORDER BY chunk_id inside the CTE makes the LIMIT select the FIRST
// page after the cursor (a bare LIMIT would take an arbitrary subset); the outer
// ORDER BY is kept so the returned rows stay ordered for the caller's keyset
// cursor regardless of how the joins are executed.
//
// The first span of a chunk is picked by joining on MIN(span_id) rather than by a
// ROW_NUMBER() window over a spans CTE. Same row (spans.span_id is the primary
// key, so the lowest span_id is the window's rn = 1), but once filtered_chunks is
// materialized SQLite drives that window off a full scan of the spans table on
// EVERY page, which reintroduces per-page O(N) work from the other side of the
// join. The correlated MIN is one index seek per returned row.
func embeddedChunkPageQuery(indexKind string, limit int, afterChunkID int64) (string, []any) {
	args := []any{"ok", afterChunkID}
	kindPredicate := ""
	if strings.TrimSpace(indexKind) != "" {
		kindPredicate = " AND c.index_kind = ?"
	}
	query := `WITH filtered_chunks AS (
	            SELECT c.chunk_id, c.rel_path, c.doc_type, c.rep_type, c.text, c.index_kind, c.modality, c.media_ref, c.language
	            FROM chunks c
	            WHERE c.embedding_status = ? AND c.deleted = 0 AND c.chunk_id > ?` + kindPredicate + `
	            ORDER BY c.chunk_id
	            LIMIT ?
	          )
	          SELECT fc.chunk_id, fc.rel_path, fc.doc_type, fc.rep_type, fc.text, fc.index_kind,
	                 COALESCE(sp.span_kind, ''), COALESCE(sp.start, 0), COALESCE(sp."end", 0), COALESCE(sp.extra_json, ''),
	                 COALESCE(d.title, ''), fc.modality, fc.media_ref, fc.language, COALESCE(d.mtime_unix, 0)
	          FROM filtered_chunks fc
	          LEFT JOIN spans sp ON sp.span_id = (
	            SELECT MIN(s.span_id) FROM spans s WHERE s.chunk_id = fc.chunk_id
	          )
	          LEFT JOIN documents d ON d.rel_path = fc.rel_path
	          ORDER BY fc.chunk_id`
	if kindPredicate != "" {
		args = append(args, indexKind)
	}
	args = append(args, limit)
	return query, args
}

// EmbeddedChunkPageQueryForTest returns the SQL a single ListEmbeddedChunkMetadata
// page runs.
//
// Exported solely so the #732 regression test can live under tests/ as AGENTS.md
// requires: the property worth pinning is WHERE in the statement the page's two
// selective predicates sit, which is neither observable through the ordinary
// store API nor reliably visible in a query plan (the planner is free to pick the
// same index either way). Production code never calls this.
func EmbeddedChunkPageQueryForTest(indexKind string, limit int, afterChunkID int64) (string, []any) {
	return embeddedChunkPageQuery(indexKind, limit, afterChunkID)
}

// ExplainEmbeddedChunkPageForTest returns the SQLite query plan (one string per
// plan row) for a single ListEmbeddedChunkMetadata page.
//
// Exported solely so the #732 regression test can live under tests/ as AGENTS.md
// requires: the property worth pinning is that a page SEEKS its rows through an
// index instead of scanning the whole chunks table, and a query plan is not
// observable through the ordinary store API. Production code never calls this.
func (s *SQLiteStore) ExplainEmbeddedChunkPageForTest(ctx context.Context, indexKind string, limit int, afterChunkID int64) ([]string, error) {
	db, err := s.ensureReadDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	query, args := embeddedChunkPageQuery(indexKind, limit, afterChunkID)
	rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			return nil, err
		}
		plan = append(plan, detail)
	}
	return plan, rows.Err()
}

// ListEmbeddedChunkMetadata pages through embedded ("ok") chunks using keyset
// (seek) pagination: afterChunkID is an exclusive lower bound on chunk_id (pass 0
// to start), and callers carry the last chunk_id of each page forward as the next
// afterChunkID. Rows are ordered by chunk_id ascending, so this walks each row
// exactly once — unlike OFFSET, which rescans and discards skipped rows every
// page (quadratic on large embedded sets).
//
// It reads through the query-only pool (#631), not the single-connection writer:
// it is a SELECT-only path, so on the writer it made every page queue behind
// in-flight ingest writes and made two concurrent kind walks serialize inside
// database/sql for no gain (#732). Both production callers (the startup warm-load
// in internal/cli and the vector reconciliation in internal/index) call it
// outside any write transaction, and the reconciler's interleaved re-pends only
// touch chunk_ids at or below the cursor it has already passed, so reading a
// slightly different snapshot than the writer holds cannot change the walk.
func (s *SQLiteStore) ListEmbeddedChunkMetadata(ctx context.Context, indexKind string, limit int, afterChunkID int64) ([]model.ChunkTask, error) {
	db, err := s.ensureReadDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()
	if limit <= 0 {
		limit = 500
	}
	if afterChunkID < 0 {
		afterChunkID = 0
	}

	query, args := embeddedChunkPageQuery(indexKind, limit, afterChunkID)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]model.ChunkTask, 0, limit)
	for rows.Next() {
		var (
			chunkID   int64
			relPath   string
			docType   string
			repType   string
			text      string
			kind      string
			spanK     string
			spanS     int
			spanE     int
			spanExtra string
			title     string
			modality  string
			mediaRef  string
			language  string
			mtimeUnix int64
		)
		if err := rows.Scan(&chunkID, &relPath, &docType, &repType, &text, &kind, &spanK, &spanS, &spanE, &spanExtra, &title, &modality, &mediaRef, &language, &mtimeUnix); err != nil {
			return nil, err
		}
		if chunkID <= 0 {
			return nil, fmt.Errorf("invalid non-positive chunk_id from database: %d", chunkID)
		}
		uid := uint64(chunkID)
		span := spanFromRow(spanK, spanS, spanE, spanExtra)
		out = append(out, model.ChunkTask{
			Label:     uid,
			Text:      text,
			IndexKind: kind,
			Modality:  modality,
			MediaRef:  mediaRef,
			Metadata: model.ChunkMetadata{
				ChunkID:   uid,
				RelPath:   relPath,
				Title:     title,
				DocType:   docType,
				RepType:   repType,
				Snippet:   snippet(text, 240),
				Span:      span,
				Modality:  modality,
				MediaRef:  mediaRef,
				Language:  language,
				MTimeUnix: mtimeUnix,
			},
		})
	}
	return out, rows.Err()
}

func spanFromRow(kind string, start, end int, extraJSON string) model.Span {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "page":
		if start <= 0 || end <= 0 || end != start {
			return model.Span{Kind: "lines"}
		}
		return model.Span{Kind: "page", Page: start}
	case "time":
		if start < 0 || end < 0 || end < start {
			return model.Span{Kind: "lines"}
		}
		speaker, speakerLabel := speakerFromExtraJSON(extraJSON)
		entities, event := annotationFromExtraJSON(extraJSON)
		return model.Span{
			Kind: "time", StartMS: start, EndMS: end,
			Words:        wordsFromExtraJSON(extraJSON),
			Speaker:      speaker,
			SpeakerLabel: speakerLabel,
			Entities:     entities,
			Event:        event,
		}
	case "region":
		return regionSpanFromRow(start, end, extraJSON)
	case "lines":
		if start > 0 && end >= start {
			return model.Span{Kind: "lines", StartLine: start, EndLine: end}
		}
	}
	return model.Span{Kind: "lines"}
}

// annotationFromExtraJSON reconstructs a recognition annotation's structured
// attribution (design 0004 §7) from a "time" span's stored extra_json. A
// NULL/empty/malformed payload yields no entities and no event, so a span that
// predates this (or comes from a transcript) degrades to exactly the behaviour
// it had before, rather than erroring.
func annotationFromExtraJSON(extraJSON string) (entities []string, event string) {
	if strings.TrimSpace(extraJSON) == "" {
		return nil, ""
	}
	var payload struct {
		Entities []string `json:"entities"`
		Event    string   `json:"event"`
	}
	if err := json.Unmarshal([]byte(extraJSON), &payload); err != nil {
		return nil, ""
	}
	return model.NormalizeEntityIDs(payload.Entities), strings.TrimSpace(payload.Event)
}

// wordsFromExtraJSON reconstructs per-word timing for a "time" span from its
// stored extra_json (spec §8.6.1). It tolerates a NULL/empty/malformed payload
// by returning nil so a time citation degrades cleanly to segment-level (the
// behaviour for a transcript without word timing). Words with empty text are
// dropped; negative durations are clamped to 0.
func wordsFromExtraJSON(extraJSON string) []model.WordSpan {
	if strings.TrimSpace(extraJSON) == "" {
		return nil
	}
	var payload struct {
		Words []model.WordSpan `json:"words"`
	}
	if err := json.Unmarshal([]byte(extraJSON), &payload); err != nil || len(payload.Words) == 0 {
		return nil
	}
	out := make([]model.WordSpan, 0, len(payload.Words))
	for _, w := range payload.Words {
		if strings.TrimSpace(w.W) == "" {
			continue
		}
		if w.D < 0 {
			w.D = 0
		}
		out = append(out, w)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// speakerFromExtraJSON reconstructs the diarized speaker attribution of a "time"
// span from its stored extra_json (spec §8.6.8). It tolerates a NULL/empty/
// malformed payload by returning empty strings so a non-diarized transcript span
// degrades to a flat, un-attributed citation (the behaviour for a transcript
// without diarization). A speaker_label with no stable speaker id is dropped:
// the id is the canonical attribution and a label alone is not a valid speaker.
func speakerFromExtraJSON(extraJSON string) (speaker, speakerLabel string) {
	if strings.TrimSpace(extraJSON) == "" {
		return "", ""
	}
	var payload struct {
		Speaker      string `json:"speaker"`
		SpeakerLabel string `json:"speaker_label"`
	}
	if err := json.Unmarshal([]byte(extraJSON), &payload); err != nil {
		return "", ""
	}
	speaker = strings.TrimSpace(payload.Speaker)
	if speaker == "" {
		return "", ""
	}
	return speaker, strings.TrimSpace(payload.SpeakerLabel)
}

// regionSpanFromRow reconstructs a region span from its stored columns. A
// region requires a well-formed extra_json payload with a bbox; if anything is
// missing or malformed it degrades to a page span on the start page (per spec:
// region clients fall back to page-level), or to lines when even the page range
// is unusable.
func regionSpanFromRow(start, end int, extraJSON string) model.Span {
	if start <= 0 || end <= 0 || end < start || strings.TrimSpace(extraJSON) == "" {
		if start > 0 {
			return model.Span{Kind: "page", Page: start}
		}
		return model.Span{Kind: "lines"}
	}
	var r model.RegionSpan
	if err := json.Unmarshal([]byte(extraJSON), &r); err != nil || r.BBox == nil {
		return model.Span{Kind: "page", Page: start}
	}
	r.StartPage = start
	r.EndPage = end
	return model.Span{Kind: "region", Region: &r}
}

// SpanToRow and SpanFromRow expose the span <-> row mapping for tests in the
// tests/ tree (the repo keeps all test files there, per AGENTS.md). They are
// thin wrappers over the unexported helpers and carry no behavior of their own.
func SpanToRow(span model.Span) (kind string, start, end int, extraJSON string, err error) {
	return spanToRow(span)
}

// SpanFromRow is the read-side counterpart to SpanToRow; see its doc.
func SpanFromRow(kind string, start, end int, extraJSON string) model.Span {
	return spanFromRow(kind, start, end, extraJSON)
}

func (s *SQLiteStore) MarkEmbedded(ctx context.Context, labels []uint64) error {
	return s.markEmbeddingStatus(ctx, labels, "ok", "", "")
}

// HasFallbackContextChunks reports whether the document at relPath has any live
// chunk whose contextual-retrieval state is `fallback` — a chunk whose context
// generation failed, so it embedded RAW (SPEC §8.1.8, issue #330).
//
// It is the self-heal gate: while contextualization stays on, the ingest scan
// treats such a document as stale even though its content hash is unchanged, so
// the failed chunks are retried on the next scan and coverage converges instead
// of leaving a silent, permanent hole.
func (s *SQLiteStore) HasFallbackContextChunks(ctx context.Context, relPath string) (bool, error) {
	normalizedPath, err := normalizeRelPath(relPath)
	if err != nil {
		return false, err
	}
	db, err := s.ensureDB(ctx)
	if err != nil {
		return false, err
	}
	defer s.ReleaseDB()

	var found int
	err = db.QueryRowContext(ctx,
		`SELECT 1 FROM chunks
		 WHERE rel_path = ? AND deleted = 0 AND embedding_mode = ?
		 LIMIT 1`,
		normalizedPath, model.EmbeddingModeFallback).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// RependEmbeddedChunks resets the given chunks' embedding_status back to
// "pending" (clearing any stale error/category) so the embed worker re-embeds
// them via NextPending. It is used by startup reconciliation (issue #402 A2)
// when an embedded chunk's vector is missing from a crash-recovered in-memory
// index, restoring recall that would otherwise be silently lost.
func (s *SQLiteStore) RependEmbeddedChunks(ctx context.Context, labels []uint64) error {
	return s.markEmbeddingStatus(ctx, labels, "pending", "", "")
}

// MarkFailed retains its original signature for callers that have not
// yet adopted error classification; they are recorded with an empty
// category (effectively ErrorCategoryUnknown for query/aggregation
// purposes). New call sites should prefer MarkFailedWithCategory so
// the failure surfaces in dir2mcp status / doctor breakdowns.
func (s *SQLiteStore) MarkFailed(ctx context.Context, labels []uint64, reason string) error {
	return s.markEmbeddingStatus(ctx, labels, "error", reason, "")
}

// MarkFailedWithCategory records a chunk-embedding failure together
// with a classification category (see ClassifyError). Category is
// stored alongside the existing free-text reason so the original
// message stays available for debugging while aggregations operate on
// the enum.
// category is the string form of an ErrorCategory: the index.ChunkSource
// interface deliberately declares it as a plain string so the embed worker
// (package index) need not depend on this package's ErrorCategory type. The
// parameter MUST stay `string` — typing it as ErrorCategory silently breaks the
// `store.(index.ChunkSource)` assertion in startEmbeddingIfNotReadOnly, which
// disables the embed worker corpus-wide with no error (issue #364).
func (s *SQLiteStore) MarkFailedWithCategory(ctx context.Context, labels []uint64, category, reason string) error {
	return s.markEmbeddingStatus(ctx, labels, "error", reason, category)
}

func (s *SQLiteStore) UpsertMCPSession(ctx context.Context, sessionID string, created, lastSeen time.Time, authScope string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return errors.New("session_id is required")
	}
	if created.IsZero() {
		created = time.Now().UTC()
	}
	if lastSeen.IsZero() {
		lastSeen = created
	}
	authScope = strings.TrimSpace(authScope)

	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()

	_, err = db.ExecContext(
		ctx,
		`INSERT INTO mcp_sessions(session_id, created_unix, last_seen_unix, auth_scope)
		 VALUES(?, ?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET
		   last_seen_unix=MAX(mcp_sessions.last_seen_unix, excluded.last_seen_unix),
		   auth_scope=COALESCE(NULLIF(excluded.auth_scope, ''), mcp_sessions.auth_scope)`,
		sessionID,
		created.UTC().Unix(),
		lastSeen.UTC().Unix(),
		authScope,
	)
	return err
}

func (s *SQLiteStore) DeleteMCPSession(ctx context.Context, sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()

	_, err = db.ExecContext(ctx, `DELETE FROM mcp_sessions WHERE session_id = ?`, sessionID)
	return err
}

func (s *SQLiteStore) ListMCPSessions(ctx context.Context) ([]MCPSessionRecord, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	rows, err := db.QueryContext(
		ctx,
		`SELECT session_id, created_unix, last_seen_unix, COALESCE(auth_scope, '')
		 FROM mcp_sessions
		 ORDER BY last_seen_unix DESC, created_unix DESC, session_id ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]MCPSessionRecord, 0)
	for rows.Next() {
		var (
			rec                   MCPSessionRecord
			createdUnix, lastUnix int64
		)
		if err := rows.Scan(&rec.ID, &createdUnix, &lastUnix, &rec.AuthScope); err != nil {
			return nil, err
		}
		rec.Created = time.Unix(createdUnix, 0).UTC()
		rec.LastSeen = time.Unix(lastUnix, 0).UTC()
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) UpsertMCPPaymentOutcome(ctx context.Context, rec MCPPaymentOutcomeRecord) error {
	rec.ExecutionKey = strings.TrimSpace(rec.ExecutionKey)
	if rec.ExecutionKey == "" {
		return errors.New("execution_key is required")
	}
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = time.Now().UTC()
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()

	_, err = db.ExecContext(
		ctx,
		`INSERT INTO mcp_payment_outcomes(
			execution_key, status_code, result_json, rpc_error_json,
			requires_settle, settled, payment_response, updated_unix, expires_unix
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(execution_key) DO UPDATE SET
			status_code=excluded.status_code,
			result_json=excluded.result_json,
			rpc_error_json=excluded.rpc_error_json,
			requires_settle=excluded.requires_settle,
			settled=excluded.settled,
			payment_response=excluded.payment_response,
			updated_unix=excluded.updated_unix,
			expires_unix=excluded.expires_unix`,
		rec.ExecutionKey,
		rec.StatusCode,
		strings.TrimSpace(rec.ResultJSON),
		strings.TrimSpace(rec.RPCErrorJSON),
		boolToInt(rec.RequiresSettle),
		boolToInt(rec.Settled),
		strings.TrimSpace(rec.PaymentResponse),
		rec.UpdatedAt.UTC().Unix(),
		optionalUnix(rec.ExpiresAt),
	)
	return err
}

// optionalUnix encodes a time that may be unset. A zero time becomes 0, not the
// year-1 Unix value, so a reader can tell "no time recorded" from a real time.
func optionalUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().Unix()
}

// timeFromOptionalUnix is the inverse of optionalUnix. A value of 0 or less
// becomes a zero time, which every caller reads as "no time recorded".
func timeFromOptionalUnix(unix int64) time.Time {
	if unix <= 0 {
		return time.Time{}
	}
	return time.Unix(unix, 0).UTC()
}

func (s *SQLiteStore) DeleteMCPPaymentOutcome(ctx context.Context, executionKey string) error {
	executionKey = strings.TrimSpace(executionKey)
	if executionKey == "" {
		return nil
	}
	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()

	_, err = db.ExecContext(ctx, `DELETE FROM mcp_payment_outcomes WHERE execution_key = ?`, executionKey)
	return err
}

func (s *SQLiteStore) ListMCPPaymentOutcomes(ctx context.Context) ([]MCPPaymentOutcomeRecord, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	rows, err := db.QueryContext(
		ctx,
		`SELECT execution_key, status_code, result_json, rpc_error_json, requires_settle, settled, payment_response, updated_unix, expires_unix
		 FROM mcp_payment_outcomes
		 ORDER BY updated_unix DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]MCPPaymentOutcomeRecord, 0)
	for rows.Next() {
		var (
			rec                     MCPPaymentOutcomeRecord
			requiresSettle, settled int
			updatedUnix             int64
			expiresUnix             int64
		)
		if err := rows.Scan(
			&rec.ExecutionKey,
			&rec.StatusCode,
			&rec.ResultJSON,
			&rec.RPCErrorJSON,
			&requiresSettle,
			&settled,
			&rec.PaymentResponse,
			&updatedUnix,
			&expiresUnix,
		); err != nil {
			return nil, err
		}
		rec.RequiresSettle = requiresSettle == 1
		rec.Settled = settled == 1
		rec.UpdatedAt = time.Unix(updatedUnix, 0).UTC()
		rec.ExpiresAt = timeFromOptionalUnix(expiresUnix)
		out = append(out, rec)
	}
	return out, rows.Err()
}

// UpsertMCPNonceLedger inserts or updates a single-use replay ledger entry. The
// nonce is the primary key; request_key/execution_key/consumed/expiry are
// overwritten on conflict so the reservation can be promoted to consumed.
func (s *SQLiteStore) UpsertMCPNonceLedger(ctx context.Context, rec MCPNonceLedgerRecord) error {
	rec.Nonce = strings.TrimSpace(rec.Nonce)
	if rec.Nonce == "" {
		return errors.New("nonce is required")
	}
	if rec.ExpiresAt.IsZero() {
		rec.ExpiresAt = time.Now().UTC()
	}
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = time.Now().UTC()
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()

	_, err = db.ExecContext(
		ctx,
		`INSERT INTO mcp_nonce_ledger(
			nonce, request_key, execution_key, consumed, expires_unix, updated_unix
		) VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(nonce) DO UPDATE SET
			-- Consumption is monotonic: a durably-consumed record (consumed=1)
			-- is never downgraded to a reservation by a later write for the same
			-- nonce, and its request/execution binding + expiry are preserved so a
			-- replay cannot clobber the single-use record.
			request_key=CASE WHEN mcp_nonce_ledger.consumed=1 THEN mcp_nonce_ledger.request_key ELSE excluded.request_key END,
			execution_key=CASE WHEN mcp_nonce_ledger.consumed=1 THEN mcp_nonce_ledger.execution_key ELSE excluded.execution_key END,
			consumed=MAX(mcp_nonce_ledger.consumed, excluded.consumed),
			expires_unix=CASE WHEN mcp_nonce_ledger.consumed=1 THEN MAX(mcp_nonce_ledger.expires_unix, excluded.expires_unix) ELSE excluded.expires_unix END,
			updated_unix=excluded.updated_unix`,
		rec.Nonce,
		strings.TrimSpace(rec.RequestKey),
		strings.TrimSpace(rec.ExecutionKey),
		boolToInt(rec.Consumed),
		rec.ExpiresAt.UTC().Unix(),
		rec.UpdatedAt.UTC().Unix(),
	)
	return err
}

// DeleteMCPNonceLedger removes a single ledger entry (used to roll back a
// reservation that was never durably consumed).
func (s *SQLiteStore) DeleteMCPNonceLedger(ctx context.Context, nonce string) error {
	nonce = strings.TrimSpace(nonce)
	if nonce == "" {
		return nil
	}
	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()

	_, err = db.ExecContext(ctx, `DELETE FROM mcp_nonce_ledger WHERE nonce = ?`, nonce)
	return err
}

// GetMCPNonceLedger returns the durable ledger record for a single nonce. The
// second return is false when no row exists. It lets the enforcement layer treat
// the persisted ledger as the source of truth on an in-memory cache miss (e.g.
// after a cap eviction), so a consumed nonce evicted from memory cannot be
// replayed within its validity window.
func (s *SQLiteStore) GetMCPNonceLedger(ctx context.Context, nonce string) (MCPNonceLedgerRecord, bool, error) {
	nonce = strings.TrimSpace(nonce)
	if nonce == "" {
		return MCPNonceLedgerRecord{}, false, nil
	}
	db, err := s.ensureDB(ctx)
	if err != nil {
		return MCPNonceLedgerRecord{}, false, err
	}
	defer s.ReleaseDB()

	var (
		rec                      MCPNonceLedgerRecord
		consumed                 int
		expiresUnix, updatedUnix int64
	)
	err = db.QueryRowContext(
		ctx,
		`SELECT nonce, request_key, execution_key, consumed, expires_unix, updated_unix
		 FROM mcp_nonce_ledger WHERE nonce = ?`,
		nonce,
	).Scan(&rec.Nonce, &rec.RequestKey, &rec.ExecutionKey, &consumed, &expiresUnix, &updatedUnix)
	if errors.Is(err, sql.ErrNoRows) {
		return MCPNonceLedgerRecord{}, false, nil
	}
	if err != nil {
		return MCPNonceLedgerRecord{}, false, err
	}
	rec.Consumed = consumed == 1
	rec.ExpiresAt = time.Unix(expiresUnix, 0).UTC()
	rec.UpdatedAt = time.Unix(updatedUnix, 0).UTC()
	return rec, true, nil
}

// DeleteExpiredMCPNonceLedger removes every ledger row whose expiry is at or
// before nowUnix, directly in the database. In-memory cap eviction can drop a
// persisted row from the cache without deleting it; this reclaims those rows at
// TTL rather than leaving them until the next process restart.
func (s *SQLiteStore) DeleteExpiredMCPNonceLedger(ctx context.Context, nowUnix int64) error {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()

	_, err = db.ExecContext(ctx, `DELETE FROM mcp_nonce_ledger WHERE expires_unix <= ?`, nowUnix)
	return err
}

// ListMCPNonceLedger returns all ledger entries (used to hydrate the in-memory
// ledger on startup so a consumed nonce survives process restart).
func (s *SQLiteStore) ListMCPNonceLedger(ctx context.Context) ([]MCPNonceLedgerRecord, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	rows, err := db.QueryContext(
		ctx,
		`SELECT nonce, request_key, execution_key, consumed, expires_unix, updated_unix
		 FROM mcp_nonce_ledger
		 ORDER BY updated_unix DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]MCPNonceLedgerRecord, 0)
	for rows.Next() {
		var (
			rec                      MCPNonceLedgerRecord
			consumed                 int
			expiresUnix, updatedUnix int64
		)
		if err := rows.Scan(
			&rec.Nonce,
			&rec.RequestKey,
			&rec.ExecutionKey,
			&consumed,
			&expiresUnix,
			&updatedUnix,
		); err != nil {
			return nil, err
		}
		rec.Consumed = consumed == 1
		rec.ExpiresAt = time.Unix(expiresUnix, 0).UTC()
		rec.UpdatedAt = time.Unix(updatedUnix, 0).UTC()
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) markEmbeddingStatus(ctx context.Context, labels []uint64, status, reason, category string) error {
	if len(labels) == 0 {
		return nil
	}

	// validate labels fit in signed 64-bit range before casting below; this
	// mirrors the check we perform when reading chunk IDs from the database in
	// NextPending/ListEmbeddedChunkMetadata where we convert them to uint64.
	for _, label := range labels {
		if label > uint64(math.MaxInt64) {
			return fmt.Errorf("label %d is too large for int64", label)
		}
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		`UPDATE chunks SET embedding_status = ?, embedding_error = ?, error_category = ?, embedding_failed_unix = ? WHERE chunk_id = ?`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()

	// Stamp the failure time on the way into 'error' and clear it on the way
	// out, so the value always describes the state the row is actually in: a
	// chunk that later embeds (or is re-queued) must not keep advertising an
	// old failure the aggregates would then report as current (issue #783).
	failedUnix := embeddingFailureStamp(status, time.Now())
	for _, label := range labels {
		if _, err := stmt.ExecContext(ctx, status, reason, category, failedUnix, int64(label)); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *SQLiteStore) Close() error {
	s.mu.Lock()
	for s.closing {
		s.cond.Wait()
	}
	if s.db == nil {
		s.mu.Unlock()
		return nil
	}
	s.closing = true
	db := s.db
	rdb := s.rdb
	vdb := s.vdb
	s.db = nil
	s.rdb = nil
	s.vdb = nil
	for s.activeOps > 0 {
		s.cond.Wait()
	}
	s.mu.Unlock()

	// Release the pinned data_version probe connection BEFORE closing the handle
	// it came from, and drop every memo so a reopened store starts clean.
	s.listCache.reset()

	err := db.Close()
	if rdb != nil {
		// Report the writer's error in preference: it is the handle whose close
		// failure can indicate unflushed work.
		if rerr := rdb.Close(); err == nil {
			err = rerr
		}
	}
	if vdb != nil {
		if verr := vdb.Close(); err == nil {
			err = verr
		}
	}

	s.mu.Lock()
	s.closing = false
	s.cond.Broadcast()
	s.mu.Unlock()
	return err
}

func (s *SQLiteStore) ensureDB(ctx context.Context) (*sql.DB, error) {
	// Acquire lock early so Close cannot clear s.db between init and use.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return nil, errors.New("sqlite db is closing")
	}

	if s.db == nil {
		if err := s.initLocked(ctx); err != nil {
			return nil, err
		}
	}
	if s.db == nil {
		return nil, errors.New("sqlite db not initialized")
	}
	s.activeOps++
	return s.db, nil
}

// ensureReadDB returns the concurrent read pool for a query-only path, holding
// the same activeOps refcount as ensureDB so Close still waits for in-flight
// reads. Callers MUST pair it with ReleaseDB.
//
// Falls back to the writer handle when the pool is unavailable, so a caller is
// never broken by the pool failing to open; it just loses the concurrency.
// Only SELECT-only paths may use this: a write issued here would race the
// writer's serialization and reintroduce the SQLITE_BUSY this store avoids.
func (s *SQLiteStore) ensureReadDB(ctx context.Context) (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return nil, errors.New("sqlite db is closing")
	}
	if s.db == nil {
		if err := s.initLocked(ctx); err != nil {
			return nil, err
		}
	}
	if s.db == nil {
		return nil, errors.New("sqlite db not initialized")
	}
	s.activeOps++
	if s.rdb != nil {
		return s.rdb, nil
	}
	return s.db, nil
}

// HandlesForTest exposes the writer handle, the read pool (nil when it failed to
// open) and the pool's configured size.
//
// Exported solely so the read-pool tests can live under tests/ as AGENTS.md
// requires: the properties worth pinning are that a read proceeds while a write
// transaction is held open, and that the per-connection pragmas reach every
// pooled connection, and neither is observable through the ordinary store API.
// Production code never calls this.
func (s *SQLiteStore) HandlesForTest(ctx context.Context) (writer, reader *sql.DB, poolSize int, err error) {
	if _, err := s.ensureDB(ctx); err != nil {
		return nil, nil, 0, err
	}
	defer s.ReleaseDB()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db, s.rdb, sqliteReadPoolSize, nil
}

type dbQueryHandle interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// EnsureDB returns a constrained query handle for tests and diagnostics while
// preserving store lifecycle control through ReleaseDB.
func (s *SQLiteStore) EnsureDB(ctx context.Context) (dbQueryHandle, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	return db, nil
}

// ReleaseDB marks completion of an operation that previously acquired a
// database handle via ensureDB.
func (s *SQLiteStore) ReleaseDB() {
	s.mu.Lock()
	if s.activeOps > 0 {
		s.activeOps--
	}
	if s.activeOps == 0 {
		s.cond.Broadcast()
	}
	s.mu.Unlock()
}

func bootstrapSettingsLocked(ctx context.Context, db *sql.DB) error {
	defaults := map[string]string{
		"schema_version":       "1",
		"protocol_version":     "2025-11-25",
		"index_format_version": indexFormatVersion,
		"embed_text_model":     "mistral-embed",
		"embed_code_model":     "codestral-embed",
		"ocr_model":            mistral.DefaultOCRModel,
		"stt_provider":         "mistral",
		"stt_model":            mistral.DefaultTranscribeModel,
		"chat_model":           mistral.DefaultChatModel,
	}

	for key, value := range defaults {
		if _, err := db.ExecContext(
			ctx,
			`INSERT INTO settings(key, value) VALUES(?, ?)
			 ON CONFLICT(key) DO NOTHING`,
			key,
			value,
		); err != nil {
			return err
		}
	}
	return nil
}

// normalizeRelPath is the store's own containment check on a document key: no
// absolute path, no parent traversal.
//
// The traversal test is by SEGMENT, not substring. The old `strings.Contains(
// normalized, "/..")` test rejected ordinary filenames whose name merely starts
// with two dots (`sub/...notes.md`, `sub/..draft.txt`), and since the caller
// only sees an upsert error, the file was counted as an error with no document
// row and no path in the log: a silent drop (#718, same defect the archive
// member check had). Note the substring test could never catch a real traversal
// the segment test misses: filepath.Clean hoists every surviving `..` to the
// front of the path, so a `..` segment after the first is unreachable by
// construction, and the leading case is already covered by the `../` prefix.
func normalizeRelPath(relPath string) (string, error) {
	normalized := strings.TrimSpace(relPath)
	if filepath.IsAbs(normalized) {
		return "", errors.New(relPathErrorMessage)
	}

	normalized = filepath.ToSlash(filepath.Clean(normalized))
	if normalized == "" ||
		normalized == "." ||
		normalized == ".." ||
		strings.HasPrefix(normalized, "../") ||
		relpath.HasDotDotSegment(normalized) {
		return "", errors.New(relPathErrorMessage)
	}

	return normalized, nil
}

func normalizeStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "skipped":
		return "skipped"
	case "error":
		return "error"
	case "secret_excluded":
		// A document withheld because it contains secrets is not "ok": it has
		// zero searchable chunks and must persist faithfully so it stays
		// visible as an audit signal (and is counted as skipped, not indexed).
		return "secret_excluded"
	case "pending":
		return "pending"
	default:
		return "ok"
	}
}

func normalizeDocType(docType string) string {
	docType = strings.TrimSpace(docType)
	if docType == "" {
		return "text"
	}
	return docType
}

func normalizeSourceType(sourceType string) string {
	switch strings.ToLower(strings.TrimSpace(sourceType)) {
	case "archive_member":
		return "archive_member"
	default:
		return "filesystem"
	}
}

func isDuplicateColumnError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "duplicate column name")
}

func normalizeIndexKind(indexKind string) string {
	switch strings.ToLower(strings.TrimSpace(indexKind)) {
	case "code":
		return "code"
	default:
		return "text"
	}
}

// validateChunkText enforces non-empty text for text chunks, while allowing
// an empty text body for media chunks (SPEC 8.1.7) — a media chunk carries no
// text; its bytes are read from media_ref at embed time.
func validateChunkText(chunk model.Chunk) error {
	if normalizeModality(chunk.Modality) == "text" {
		if strings.TrimSpace(chunk.Text) == "" {
			return errors.New("chunk text must be non-empty")
		}
		return nil
	}
	// A media chunk carries no text but MUST reference its source bytes, so
	// the failure is deterministic at write time rather than later at embed.
	if strings.TrimSpace(chunk.MediaRef) == "" {
		return errors.New("media chunk must have a media_ref")
	}
	return nil
}

// normalizeModality maps a chunk modality to a known value, defaulting to
// "text" (SPEC 8.1.7).
func normalizeModality(modality string) string {
	switch strings.ToLower(strings.TrimSpace(modality)) {
	case "image", "audio", "video", "pdf":
		return strings.ToLower(strings.TrimSpace(modality))
	default:
		return "text"
	}
}

func normalizeEmbeddingStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ok":
		return "ok"
	case "error":
		return "error"
	default:
		return "pending"
	}
}

// spanToRow flattens a span to its stored columns. extraJSON is the empty
// string for the scalar kinds (stored as SQL NULL) and a JSON object for
// region spans carrying bbox/section/label (spec §5.4).
func spanToRow(span model.Span) (kind string, start int, end int, extraJSON string, err error) {
	switch strings.ToLower(strings.TrimSpace(span.Kind)) {
	case "lines":
		if span.StartLine <= 0 || span.EndLine <= 0 || span.EndLine < span.StartLine {
			return "", 0, 0, "", errors.New("invalid lines span")
		}
		return "lines", span.StartLine, span.EndLine, "", nil
	case "page":
		if span.Page <= 0 {
			return "", 0, 0, "", errors.New("invalid page span")
		}
		return "page", span.Page, span.Page, "", nil
	case "time":
		if span.StartMS < 0 || span.EndMS < 0 || span.EndMS < span.StartMS {
			return "", 0, 0, "", errors.New("invalid time span")
		}
		extra, eErr := timeSpanExtraJSON(span.Words, span.Speaker, span.SpeakerLabel, span.Entities, span.Event)
		if eErr != nil {
			return "", 0, 0, "", eErr
		}
		return "time", span.StartMS, span.EndMS, extra, nil
	case "region":
		return regionSpanToRow(span.Region)
	default:
		return "", 0, 0, "", fmt.Errorf("unsupported span kind: %q", span.Kind)
	}
}

// timeSpanExtraJSON marshals the optional metadata of a "time" span into the
// stored extra_json object: per-word timing (`words`, spec §8.6.1) plus the
// diarized speaker attribution (`speaker`/`speaker_label`, spec §8.6.8). Each
// field is emitted only when present (omitempty), so a transcript with neither
// returns the empty string (stored as SQL NULL) and round-trips byte-identically
// to before diarization existed. speaker/speaker_label are trimmed; an empty
// speaker drops both (a label without a stable id is not a valid attribution).
func timeSpanExtraJSON(
	words []model.WordSpan, speaker, speakerLabel string, entities []string, event string,
) (string, error) {
	speaker = strings.TrimSpace(speaker)
	speakerLabel = strings.TrimSpace(speakerLabel)
	if speaker == "" {
		// A label with no stable id is not a valid attribution; drop both so the
		// stored shape never carries a dangling speaker_label.
		speakerLabel = ""
	}
	entities = model.NormalizeEntityIDs(entities)
	event = strings.TrimSpace(event)
	if len(words) == 0 && speaker == "" && len(entities) == 0 && event == "" {
		return "", nil
	}
	payload := struct {
		Words        []model.WordSpan `json:"words,omitempty"`
		Speaker      string           `json:"speaker,omitempty"`
		SpeakerLabel string           `json:"speaker_label,omitempty"`
		Entities     []string         `json:"entities,omitempty"`
		Event        string           `json:"event,omitempty"`
	}{
		Words: words, Speaker: speaker, SpeakerLabel: speakerLabel,
		Entities: entities, Event: event,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal time span extra_json: %w", err)
	}
	return string(encoded), nil
}

// regionSpanToRow validates and flattens a region span: the page range goes to
// start/end and the bbox/section/label payload is marshaled to extra_json. A
// region MUST carry a bbox and a valid page range (spec §5.4).
//
// It is the persistence boundary that enforces the §5.4 Span constraints so a
// stored span always round-trips against the published Span schema (a strict
// client would otherwise reject an out-of-enum value with "Failed to call
// tool"): coord_origin is normalized to {TOPLEFT,BOTTOMLEFT}, label to the
// eight-value enum, and the bbox.page ∈ [start_page,end_page] invariant (§5.4
// MUST) is validated — hard-rejected, because a bbox on a page outside the
// chunk's range is a provenance error, not something to silently clamp.
func regionSpanToRow(r *model.RegionSpan) (kind string, start int, end int, extraJSON string, err error) {
	if r == nil {
		return "", 0, 0, "", errors.New("invalid region span: missing region payload")
	}
	if r.StartPage <= 0 || r.EndPage <= 0 || r.EndPage < r.StartPage {
		return "", 0, 0, "", errors.New("invalid region span: bad page range")
	}
	if r.BBox == nil {
		return "", 0, 0, "", errors.New("invalid region span: missing bbox")
	}
	// §5.4 MUST: start_page ≤ bbox.page ≤ end_page (bbox.page is the primary page).
	if r.BBox.Page < r.StartPage || r.BBox.Page > r.EndPage {
		return "", 0, 0, "", fmt.Errorf(
			"invalid region span: bbox.page %d outside page range [%d,%d]",
			r.BBox.Page, r.StartPage, r.EndPage,
		)
	}
	// Normalize coord_origin + label to their §5.4 enums on a copy so the stored
	// extra_json conforms without mutating the caller's span.
	normalized := *r
	bbox := *r.BBox
	bbox.CoordOrigin = model.NormalizeCoordOrigin(bbox.CoordOrigin)
	normalized.BBox = &bbox
	normalized.Label = model.NormalizeRegionLabel(r.Label)
	encoded, mErr := json.Marshal(&normalized)
	if mErr != nil {
		return "", 0, 0, "", fmt.Errorf("marshal region span: %w", mErr)
	}
	return "region", r.StartPage, r.EndPage, string(encoded), nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func defaultIfEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func escapeLike(v string) string {
	v = strings.ReplaceAll(v, `\\`, `\\\\`)
	v = strings.ReplaceAll(v, `%`, `\\%`)
	v = strings.ReplaceAll(v, `_`, `\\_`)
	return v
}

func snippet(text string, max int) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max])
}

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
	"time"

	_ "modernc.org/sqlite"

	"github.com/dirstral/dir2mcp/internal/mistral"
	"github.com/dirstral/dir2mcp/internal/model"
)

const relPathErrorMessage = "rel_path must be a non-empty relative path without parent-traversal or absolute paths"

type SQLiteStore struct {
	path string

	mu sync.Mutex
	db *sql.DB

	activeOps int
	closing   bool
	cond      *sync.Cond
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

func lookupChunkDocContext(ctx context.Context, exec dbExecutor, repID int64) (relPath, docType, repType string, err error) {
	err = exec.QueryRowContext(
		ctx,
		`SELECT d.rel_path, d.doc_type, r.rep_type
		 FROM representations r
		 JOIN documents d ON d.doc_id = r.doc_id
		 WHERE r.rep_id = ?
		 LIMIT 1`,
		repID,
	).Scan(&relPath, &docType, &repType)
	return relPath, docType, repType, err
}

func insertChunkWithSpansWith(ctx context.Context, exec dbExecutor, chunk model.Chunk, spans []model.Span, relPath, docType, repType string) (int64, error) {
	_, err := exec.ExecContext(
		ctx,
		`INSERT INTO chunks(rep_id, ordinal, rel_path, doc_type, rep_type, text, text_hash, tokens_est, index_kind, modality, media_ref, embedding_status, embedding_error, error_category, deleted)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
		   embedding_status=excluded.embedding_status,
		   embedding_error=excluded.embedding_error,
		   error_category=excluded.error_category,
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
		normalizeEmbeddingStatus(chunk.EmbeddingStatus),
		strings.TrimSpace(chunk.EmbeddingError),
		strings.TrimSpace(chunk.ErrorCategory),
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
func openDB(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// Order matters: busy_timeout MUST come before journal_mode. Switching to
	// WAL itself can fail with SQLITE_BUSY if another process holds the
	// database lock; without busy_timeout already in effect, that PRAGMA
	// returns immediately rather than waiting.
	for _, pragma := range []string{
		`PRAGMA busy_timeout=5000;`,
		`PRAGMA journal_mode=WAL;`,
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return db, nil
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
  status TEXT NOT NULL DEFAULT 'ok',
  deleted INTEGER NOT NULL DEFAULT 0
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
  embedding_status TEXT NOT NULL DEFAULT 'pending',
  embedding_error TEXT NOT NULL DEFAULT '',
  error_category TEXT NOT NULL DEFAULT '',
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
CREATE INDEX IF NOT EXISTS idx_spans_chunk_id_span_id ON spans(chunk_id, span_id);
CREATE INDEX IF NOT EXISTS idx_mcp_sessions_last_seen ON mcp_sessions(last_seen_unix);
CREATE INDEX IF NOT EXISTS idx_mcp_payment_outcomes_updated ON mcp_payment_outcomes(updated_unix);
`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return err
	}
	if err := applyAdditiveColumnMigrations(ctx, db); err != nil {
		_ = db.Close()
		return err
	}
	if err := backfillFTSIfEmpty(ctx, db); err != nil {
		_ = db.Close()
		return err
	}

	if err := bootstrapSettingsLocked(ctx, db); err != nil {
		_ = db.Close()
		return err
	}

	s.db = db
	return nil
}

// backfillFTSIfEmpty handles the upgrade path where chunks_fts is created
// fresh against an existing chunks table. Without this, FTS searches on a
// pre-existing index return zero hits until each chunk is reprocessed by
// ingest. The 'rebuild' command re-derives FTS content from the
// content='chunks' external-content reference.
func backfillFTSIfEmpty(ctx context.Context, db *sql.DB) error {
	var exists, chunkCount int64
	err := db.QueryRowContext(ctx, `SELECT 1 FROM chunks_fts LIMIT 1`).Scan(&exists)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check chunks_fts empty: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks WHERE deleted = 0`).Scan(&chunkCount); err != nil {
		return fmt.Errorf("count chunks: %w", err)
	}
	if chunkCount == 0 {
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
		`INSERT INTO documents(rel_path, doc_type, source_type, size_bytes, mtime_unix, content_hash, status, deleted, title, error_message)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(rel_path) DO UPDATE SET
		   doc_type=excluded.doc_type,
		   source_type=excluded.source_type,
		   size_bytes=excluded.size_bytes,
		   mtime_unix=excluded.mtime_unix,
		   content_hash=excluded.content_hash,
		   status=excluded.status,
		   deleted=excluded.deleted,
		   title=CASE WHEN excluded.title <> '' THEN excluded.title ELSE documents.title END,
		   error_message=excluded.error_message`,
		relPath,
		normalizeDocType(doc.DocType),
		normalizeSourceType(doc.SourceType),
		doc.SizeBytes,
		doc.MTimeUnix,
		strings.TrimSpace(doc.ContentHash),
		normalizeStatus(doc.Status),
		boolToInt(doc.Deleted),
		strings.TrimSpace(doc.Title),
		sanitizeDocErrorMessage(doc.ErrorMessage),
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
		   error_category=''`,
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
	relPath, docType, repType, err := lookupChunkDocContext(ctx, tx, chunk.RepID)
	if err != nil {
		return 0, err
	}
	chunkID, err := insertChunkWithSpansWith(ctx, tx, chunk, spans, relPath, docType, repType)
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

	relPath, docType, repType, err := lookupChunkDocContext(ctx, t.tx, chunk.RepID)
	if err != nil {
		return 0, err
	}
	return insertChunkWithSpansWith(ctx, t.tx, chunk, spans, relPath, docType, repType)
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

	db, err := s.ensureDB(ctx)
	if err != nil {
		return model.Document{}, err
	}
	defer s.ReleaseDB()

	var doc model.Document
	var deleted int
	row := db.QueryRowContext(
		ctx,
		`SELECT doc_id, rel_path, doc_type, source_type, size_bytes, mtime_unix, content_hash, status, deleted, title, error_message
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
		&doc.Status,
		&deleted,
		&doc.Title,
		&doc.ErrorMessage,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Document{}, os.ErrNotExist
		}
		return model.Document{}, err
	}
	doc.Deleted = deleted == 1
	return doc, nil
}

func (s *SQLiteStore) ListFiles(ctx context.Context, prefix, glob string, limit, offset int) ([]model.Document, int64, error) {
	db, err := s.ensureDB(ctx)
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

	query := `SELECT doc_id, rel_path, doc_type, source_type, size_bytes, mtime_unix, content_hash, status, deleted, title, error_message FROM documents`
	where := []string{"deleted = 0"}
	args := make([]any, 0, 4)
	if normalizedPrefix != "" {
		where = append(where, `rel_path LIKE ? ESCAPE '\'`)
		args = append(args, escapeLike(normalizedPrefix)+"%")
	}
	if strings.TrimSpace(glob) != "" {
		where = append(where, "rel_path GLOB ?")
		args = append(args, glob)
	}
	query += " WHERE " + strings.Join(where, " AND ")
	query += " ORDER BY rel_path LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	docs := make([]model.Document, 0, limit)
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
			&doc.Status,
			&deleted,
			&doc.Title,
			&doc.ErrorMessage,
		); err != nil {
			return nil, 0, err
		}
		doc.Deleted = deleted == 1
		docs = append(docs, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	var total int64
	countQuery := "SELECT COUNT(*) FROM documents"
	countArgs := make([]any, 0)
	if len(where) > 0 {
		countQuery += " WHERE " + strings.Join(where, " AND ")
		countArgs = append(countArgs, args[:len(args)-2]...)
	}
	if err := db.QueryRowContext(ctx, countQuery, countArgs...).Scan(&total); err != nil {
		return nil, 0, err
	}

	return docs, total, nil
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
		`SELECT doc_id, rel_path, doc_type, source_type, size_bytes, mtime_unix, content_hash, status, deleted, title, error_message
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
			&doc.Status,
			&deleted,
			&doc.Title,
			&doc.ErrorMessage,
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
			COALESCE(SUM(CASE WHEN deleted = 0 AND status = 'skipped' THEN 1 ELSE 0 END), 0) AS skipped,
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

	summary, err := loadFailureSummary(ctx, db, failureSummaryMaxSamples)
	if err != nil {
		return model.CorpusStats{}, err
	}
	stats.FailureSummary = summary

	return stats, nil
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

	args := []any{"pending"}
	query := `WITH filtered_chunks AS (
	            SELECT c.chunk_id, c.rel_path, c.doc_type, c.rep_type, c.text, c.index_kind, c.modality, c.media_ref
	            FROM chunks c
	            WHERE c.embedding_status = ? AND c.deleted = 0 AND c.chunk_id > 0
	          ),
	          ranked_spans AS (
	            SELECT s.chunk_id, s.span_kind, s.start, s."end", s.extra_json,
	                   ROW_NUMBER() OVER (PARTITION BY s.chunk_id ORDER BY s.span_id) AS rn
	            FROM spans s
	            JOIN filtered_chunks fc ON fc.chunk_id = s.chunk_id
	          )
	          SELECT fc.chunk_id, fc.rel_path, fc.doc_type, fc.rep_type, fc.text, fc.index_kind, fc.modality, fc.media_ref,
	                 COALESCE(sp.span_kind, ''), COALESCE(sp.start, 0), COALESCE(sp.end, 0), COALESCE(sp.extra_json, '')
	          FROM filtered_chunks fc
	          LEFT JOIN ranked_spans sp ON sp.chunk_id = fc.chunk_id AND sp.rn = 1`
	if strings.TrimSpace(indexKind) != "" {
		query += " WHERE fc.index_kind = ?"
		args = append(args, indexKind)
	}
	args = append(args, limit)
	query += " ORDER BY fc.chunk_id LIMIT ?"

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
			idxKind   string
			modality  string
			mediaRef  string
			spanK     string
			spanS     int
			spanE     int
			spanExtra string
		)
		if err := rows.Scan(&chunkID, &relPath, &docType, &repType, &text, &idxKind, &modality, &mediaRef, &spanK, &spanS, &spanE, &spanExtra); err != nil {
			return nil, err
		}
		if chunkID <= 0 {
			return nil, fmt.Errorf("invalid non-positive chunk_id from database: %d", chunkID)
		}
		uid := uint64(chunkID)
		span := spanFromRow(spanK, spanS, spanE, spanExtra)
		task := model.NewChunkTask(uid, text, idxKind, model.ChunkMetadata{
			ChunkID:  uid,
			RelPath:  relPath,
			DocType:  docType,
			RepType:  repType,
			Snippet:  snippet(text, 240),
			Span:     span,
			Modality: modality,
			MediaRef: mediaRef,
		})
		task.Modality = modality
		task.MediaRef = mediaRef
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

func (s *SQLiteStore) ListEmbeddedChunkMetadata(ctx context.Context, indexKind string, limit, offset int) ([]model.ChunkTask, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()
	if limit <= 0 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}

	args := []any{"ok"}
	query := `WITH filtered_chunks AS (
	            SELECT c.chunk_id, c.rel_path, c.doc_type, c.rep_type, c.text, c.index_kind, c.modality, c.media_ref
	            FROM chunks c
	            WHERE c.embedding_status = ? AND c.deleted = 0 AND c.chunk_id > 0
	          ),
	          ranked_spans AS (
	            SELECT s.chunk_id, s.span_kind, s.start, s."end", s.extra_json,
	                   ROW_NUMBER() OVER (PARTITION BY s.chunk_id ORDER BY s.span_id) AS rn
	            FROM spans s
	            JOIN filtered_chunks fc ON fc.chunk_id = s.chunk_id
	          )
	          SELECT fc.chunk_id, fc.rel_path, fc.doc_type, fc.rep_type, fc.text, fc.index_kind,
	                 COALESCE(sp.span_kind, ''), COALESCE(sp.start, 0), COALESCE(sp.end, 0), COALESCE(sp.extra_json, ''),
	                 COALESCE(d.title, ''), fc.modality, fc.media_ref
	          FROM filtered_chunks fc
	          LEFT JOIN ranked_spans sp ON sp.chunk_id = fc.chunk_id AND sp.rn = 1
	          LEFT JOIN documents d ON d.rel_path = fc.rel_path`
	if strings.TrimSpace(indexKind) != "" {
		query += ` WHERE fc.index_kind = ?`
		args = append(args, indexKind)
	}
	args = append(args, limit, offset)
	query += ` ORDER BY fc.chunk_id LIMIT ? OFFSET ?`

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
		)
		if err := rows.Scan(&chunkID, &relPath, &docType, &repType, &text, &kind, &spanK, &spanS, &spanE, &spanExtra, &title, &modality, &mediaRef); err != nil {
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
				ChunkID:  uid,
				RelPath:  relPath,
				Title:    title,
				DocType:  docType,
				RepType:  repType,
				Snippet:  snippet(text, 240),
				Span:     span,
				Modality: modality,
				MediaRef: mediaRef,
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
		return model.Span{Kind: "time", StartMS: start, EndMS: end, Words: wordsFromExtraJSON(extraJSON)}
	case "region":
		return regionSpanFromRow(start, end, extraJSON)
	case "lines":
		if start > 0 && end >= start {
			return model.Span{Kind: "lines", StartLine: start, EndLine: end}
		}
	}
	return model.Span{Kind: "lines"}
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
func (s *SQLiteStore) MarkFailedWithCategory(ctx context.Context, labels []uint64, category ErrorCategory, reason string) error {
	return s.markEmbeddingStatus(ctx, labels, "error", reason, string(category))
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
			requires_settle, settled, payment_response, updated_unix
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(execution_key) DO UPDATE SET
			status_code=excluded.status_code,
			result_json=excluded.result_json,
			rpc_error_json=excluded.rpc_error_json,
			requires_settle=excluded.requires_settle,
			settled=excluded.settled,
			payment_response=excluded.payment_response,
			updated_unix=excluded.updated_unix`,
		rec.ExecutionKey,
		rec.StatusCode,
		strings.TrimSpace(rec.ResultJSON),
		strings.TrimSpace(rec.RPCErrorJSON),
		boolToInt(rec.RequiresSettle),
		boolToInt(rec.Settled),
		strings.TrimSpace(rec.PaymentResponse),
		rec.UpdatedAt.UTC().Unix(),
	)
	return err
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
		`SELECT execution_key, status_code, result_json, rpc_error_json, requires_settle, settled, payment_response, updated_unix
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
		); err != nil {
			return nil, err
		}
		rec.RequiresSettle = requiresSettle == 1
		rec.Settled = settled == 1
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
		`UPDATE chunks SET embedding_status = ?, embedding_error = ?, error_category = ? WHERE chunk_id = ?`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()

	for _, label := range labels {
		if _, err := stmt.ExecContext(ctx, status, reason, category, int64(label)); err != nil {
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
	s.db = nil
	for s.activeOps > 0 {
		s.cond.Wait()
	}
	s.mu.Unlock()

	err := db.Close()

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
		"index_format_version": "1",
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
		strings.Contains(normalized, "/..") {
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
		extra, eErr := timeSpanExtraJSON(span.Words)
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

// timeSpanExtraJSON marshals per-word timing for a "time" span into the stored
// extra_json shape `{"words":[{"t":..,"d":..,"w":".."}]}` (spec §8.6.1).
// Returns the empty string (stored as SQL NULL, preserving prior behaviour) when
// there is no word timing, so words-absent transcripts round-trip unchanged.
func timeSpanExtraJSON(words []model.WordSpan) (string, error) {
	if len(words) == 0 {
		return "", nil
	}
	payload := struct {
		Words []model.WordSpan `json:"words"`
	}{Words: words}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal time span words: %w", err)
	}
	return string(encoded), nil
}

// regionSpanToRow validates and flattens a region span: the page range goes to
// start/end and the bbox/section/label payload is marshaled to extra_json. A
// region MUST carry a bbox and a valid page range (spec §5.4).
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
	encoded, mErr := json.Marshal(r)
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

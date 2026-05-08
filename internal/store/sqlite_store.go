package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"dir2mcp/internal/mistral"
	"dir2mcp/internal/model"
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
		`INSERT INTO chunks(rep_id, ordinal, rel_path, doc_type, rep_type, text, text_hash, tokens_est, index_kind, embedding_status, embedding_error, deleted)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(rep_id, ordinal) DO UPDATE SET
		   rel_path=excluded.rel_path,
		   doc_type=excluded.doc_type,
		   rep_type=excluded.rep_type,
		   text=excluded.text,
		   text_hash=excluded.text_hash,
		   tokens_est=excluded.tokens_est,
		   index_kind=excluded.index_kind,
		   embedding_status=excluded.embedding_status,
		   embedding_error=excluded.embedding_error,
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
		normalizeEmbeddingStatus(chunk.EmbeddingStatus),
		strings.TrimSpace(chunk.EmbeddingError),
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
		spanKind, startValue, endValue, spanErr := spanToRow(span)
		if spanErr != nil {
			return 0, spanErr
		}
		if _, err := exec.ExecContext(
			ctx,
			`INSERT INTO spans (chunk_id, span_kind, start, end, extra_json) VALUES (?, ?, ?, ?, NULL)`,
			chunkID,
			spanKind,
			startValue,
			endValue,
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
	for _, pragma := range []string{
		`PRAGMA journal_mode=WAL;`,
		`PRAGMA busy_timeout=5000;`,
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return db, nil
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
  embedding_status TEXT NOT NULL DEFAULT 'pending',
  embedding_error TEXT NOT NULL DEFAULT '',
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
	if _, err := db.ExecContext(ctx, `ALTER TABLE documents ADD COLUMN source_type TEXT NOT NULL DEFAULT 'filesystem'`); err != nil && !isDuplicateColumnError(err) {
		_ = db.Close()
		return err
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE mcp_sessions ADD COLUMN auth_scope TEXT NOT NULL DEFAULT ''`); err != nil && !isDuplicateColumnError(err) {
		_ = db.Close()
		return err
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE representations ADD COLUMN meta_json TEXT NOT NULL DEFAULT ''`); err != nil && !isDuplicateColumnError(err) {
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

	_, err = db.ExecContext(
		ctx,
		`INSERT INTO documents(rel_path, doc_type, source_type, size_bytes, mtime_unix, content_hash, status, deleted)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(rel_path) DO UPDATE SET
		   doc_type=excluded.doc_type,
		   source_type=excluded.source_type,
		   size_bytes=excluded.size_bytes,
		   mtime_unix=excluded.mtime_unix,
		   content_hash=excluded.content_hash,
		   status=excluded.status,
		   deleted=excluded.deleted`,
		relPath,
		normalizeDocType(doc.DocType),
		normalizeSourceType(doc.SourceType),
		doc.SizeBytes,
		doc.MTimeUnix,
		strings.TrimSpace(doc.ContentHash),
		normalizeStatus(doc.Status),
		boolToInt(doc.Deleted),
	)
	return err
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
		`INSERT INTO chunks(chunk_id, rel_path, doc_type, rep_type, text, index_kind, embedding_status, embedding_error, deleted)
		 VALUES(?, ?, ?, ?, ?, ?, 'pending', '', 0)
		 ON CONFLICT(chunk_id) DO UPDATE SET
		   rel_path=excluded.rel_path,
		   doc_type=excluded.doc_type,
		   rep_type=excluded.rep_type,
		   text=excluded.text,
		   index_kind=excluded.index_kind,
		   deleted=0,
		   embedding_status='pending',
		   embedding_error=''`,
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
	if strings.TrimSpace(chunk.Text) == "" {
		return 0, errors.New("chunk text must be non-empty")
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
	if strings.TrimSpace(chunk.Text) == "" {
		return 0, errors.New("chunk text must be non-empty")
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
		`SELECT chunk_id, rep_id, ordinal, text, text_hash, index_kind, embedding_status, embedding_error, deleted
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
		`SELECT doc_id, rel_path, doc_type, source_type, size_bytes, mtime_unix, content_hash, status, deleted
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

	normalizedPrefix := normalizePrefix(prefix)

	query := `SELECT doc_id, rel_path, doc_type, source_type, size_bytes, mtime_unix, content_hash, status, deleted FROM documents`
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
	            SELECT c.chunk_id, c.rel_path, c.doc_type, c.rep_type, c.text, c.index_kind
	            FROM chunks c
	            WHERE c.embedding_status = ? AND c.deleted = 0 AND c.chunk_id > 0
	          ),
	          ranked_spans AS (
	            SELECT s.chunk_id, s.span_kind, s.start, s."end",
	                   ROW_NUMBER() OVER (PARTITION BY s.chunk_id ORDER BY s.span_id) AS rn
	            FROM spans s
	            JOIN filtered_chunks fc ON fc.chunk_id = s.chunk_id
	          )
	          SELECT fc.chunk_id, fc.rel_path, fc.doc_type, fc.rep_type, fc.text, fc.index_kind,
	                 COALESCE(sp.span_kind, ''), COALESCE(sp.start, 0), COALESCE(sp.end, 0)
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
			chunkID int64
			relPath string
			docType string
			repType string
			text    string
			idxKind string
			spanK   string
			spanS   int
			spanE   int
		)
		if err := rows.Scan(&chunkID, &relPath, &docType, &repType, &text, &idxKind, &spanK, &spanS, &spanE); err != nil {
			return nil, err
		}
		if chunkID <= 0 {
			return nil, fmt.Errorf("invalid non-positive chunk_id from database: %d", chunkID)
		}
		uid := uint64(chunkID)
		span := spanFromRow(spanK, spanS, spanE)
		tasks = append(tasks, model.NewChunkTask(uid, text, idxKind, model.ChunkMetadata{
			ChunkID: uid,
			RelPath: relPath,
			DocType: docType,
			RepType: repType,
			Snippet: snippet(text, 240),
			Span:    span,
		}))
	}
	return tasks, rows.Err()
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
	            SELECT c.chunk_id, c.rel_path, c.doc_type, c.rep_type, c.text, c.index_kind
	            FROM chunks c
	            WHERE c.embedding_status = ? AND c.deleted = 0 AND c.chunk_id > 0
	          ),
	          ranked_spans AS (
	            SELECT s.chunk_id, s.span_kind, s.start, s."end",
	                   ROW_NUMBER() OVER (PARTITION BY s.chunk_id ORDER BY s.span_id) AS rn
	            FROM spans s
	            JOIN filtered_chunks fc ON fc.chunk_id = s.chunk_id
	          )
	          SELECT fc.chunk_id, fc.rel_path, fc.doc_type, fc.rep_type, fc.text, fc.index_kind,
	                 COALESCE(sp.span_kind, ''), COALESCE(sp.start, 0), COALESCE(sp.end, 0)
	          FROM filtered_chunks fc
	          LEFT JOIN ranked_spans sp ON sp.chunk_id = fc.chunk_id AND sp.rn = 1`
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
			chunkID int64
			relPath string
			docType string
			repType string
			text    string
			kind    string
			spanK   string
			spanS   int
			spanE   int
		)
		if err := rows.Scan(&chunkID, &relPath, &docType, &repType, &text, &kind, &spanK, &spanS, &spanE); err != nil {
			return nil, err
		}
		if chunkID <= 0 {
			return nil, fmt.Errorf("invalid non-positive chunk_id from database: %d", chunkID)
		}
		uid := uint64(chunkID)
		span := spanFromRow(spanK, spanS, spanE)
		out = append(out, model.ChunkTask{
			Label:     uid,
			Text:      text,
			IndexKind: kind,
			Metadata: model.ChunkMetadata{
				ChunkID: uid,
				RelPath: relPath,
				DocType: docType,
				RepType: repType,
				Snippet: snippet(text, 240),
				Span:    span,
			},
		})
	}
	return out, rows.Err()
}

func spanFromRow(kind string, start, end int) model.Span {
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
		return model.Span{Kind: "time", StartMS: start, EndMS: end}
	case "lines":
		if start > 0 && end >= start {
			return model.Span{Kind: "lines", StartLine: start, EndLine: end}
		}
	}
	return model.Span{Kind: "lines"}
}

func (s *SQLiteStore) MarkEmbedded(ctx context.Context, labels []uint64) error {
	return s.markEmbeddingStatus(ctx, labels, "ok", "")
}

func (s *SQLiteStore) MarkFailed(ctx context.Context, labels []uint64, reason string) error {
	return s.markEmbeddingStatus(ctx, labels, "error", reason)
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

func (s *SQLiteStore) markEmbeddingStatus(ctx context.Context, labels []uint64, status, reason string) error {
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

	stmt, err := tx.PrepareContext(ctx, `UPDATE chunks SET embedding_status = ?, embedding_error = ? WHERE chunk_id = ?`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()

	for _, label := range labels {
		if _, err := stmt.ExecContext(ctx, status, reason, int64(label)); err != nil {
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

func normalizePrefix(prefix string) string {
	trimmed := strings.TrimSpace(prefix)
	if trimmed == "" {
		return ""
	}
	trimmed = strings.TrimPrefix(trimmed, "./")
	trimmed = filepath.ToSlash(filepath.Clean(trimmed))
	trimmed = strings.TrimPrefix(trimmed, "/")
	if trimmed == "." {
		return ""
	}
	return trimmed
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

func spanToRow(span model.Span) (kind string, start int, end int, err error) {
	switch strings.ToLower(strings.TrimSpace(span.Kind)) {
	case "lines":
		if span.StartLine <= 0 || span.EndLine <= 0 || span.EndLine < span.StartLine {
			return "", 0, 0, errors.New("invalid lines span")
		}
		return "lines", span.StartLine, span.EndLine, nil
	case "page":
		if span.Page <= 0 {
			return "", 0, 0, errors.New("invalid page span")
		}
		return "page", span.Page, span.Page, nil
	case "time":
		if span.StartMS < 0 || span.EndMS < 0 || span.EndMS < span.StartMS {
			return "", 0, 0, errors.New("invalid time span")
		}
		return "time", span.StartMS, span.EndMS, nil
	default:
		return "", 0, 0, fmt.Errorf("unsupported span kind: %q", span.Kind)
	}
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

// Package pgvectorindex implements the pluggable model.Index contract (issue
// #269) on top of PostgreSQL + the pgvector extension. It is the Tier-C
// "boring infrastructure" backend: an opt-in alternative to the in-memory HNSW
// default (and to the Qdrant backend, issue #268) for operators who already run
// Postgres.
//
// The client is pure-Go (github.com/jackc/pgx/v5), so CGO_ENABLED=0 builds are
// unaffected. Vectors are stored in a vector(dim) column with an HNSW index
// using cosine distance; payload columns mirror model.Filter's predicate fields
// and the full payload is additionally kept as JSONB so no field is lost on
// read-back.
package pgvectorindex

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dirstral/dir2mcp/internal/model"
)

// compile-time assertions that Index satisfies the core contract and the
// optional FilteringIndex capability (issue #247/#269). It deliberately does
// NOT implement Persistable: a networked backend owns its own durability.
var (
	_ model.Index          = (*Index)(nil)
	_ model.FilteringIndex = (*Index)(nil)
)

// Querier is the subset of pgx's API the index uses, narrowed to an interface
// so unit tests can stub it without a live database. *pgxpool.Pool satisfies
// it. It is exported so out-of-package tests can construct an Index over a stub
// via NewWithQuerier.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Config configures a pgvector-backed index.
type Config struct {
	// DSN is the libpq connection string (resolved from a secret; never
	// persisted). Required.
	DSN string
	// Schema and Table name the vectors table; empty values fall back to
	// DefaultSchema / DefaultTable.
	Schema string
	// Table is the vectors table name.
	Table string
	// Dim is the embedding dimension. Zero means "infer from the first vector":
	// the vectors table (with its fixed vector(dim) column) is then created
	// lazily on the first Upsert/Reset. This accommodates providers whose
	// dimension is the model's native size and not statically known at startup.
	Dim int
}

// withDefaults returns a copy of cfg with empty Schema/Table filled in.
func (c Config) withDefaults() Config {
	if c.Schema == "" {
		c.Schema = DefaultSchema
	}
	if c.Table == "" {
		c.Table = DefaultTable
	}
	return c
}

// validate checks the config is usable and the identifiers are safe to
// interpolate into DDL.
func (c Config) validate() error {
	if c.DSN == "" {
		return errors.New("pgvector dsn is required")
	}
	if err := ValidateIdentifier("schema", c.Schema); err != nil {
		return err
	}
	if err := ValidateIdentifier("table", c.Table); err != nil {
		return err
	}
	if c.Dim < 0 {
		return fmt.Errorf("pgvector embedding dimension must be non-negative, got %d", c.Dim)
	}
	return nil
}

// Index is a pgvector-backed model.Index.
type Index struct {
	pool   *pgxpool.Pool
	db     Querier
	schema string
	table  string

	mu  sync.Mutex
	dim int // 0 until known (config or first vector)
	// tableReady is true once THIS process created (or re-issued the DDL for) the
	// vectors table and its HNSW index, so Upsert may skip the DDL.
	tableReady bool
	// tableExists is true once the vectors table has been observed in the
	// database. It gates Search/Delete only, and it is weaker than tableReady on
	// purpose: a table another process created answers queries here, and a table
	// whose HNSW index a crashed process never finished still needs the DDL
	// re-issued by the next Upsert (issue #666).
	tableExists bool
}

// Open connects to Postgres, verifies pgvector is available, and ensures the
// schema (vectors table + HNSW index + identity table) exists. A connection or
// extension failure returns a clear, remediable error — there is no silent
// fallback to the in-memory backend (issue #269).
//
// With an unknown dimension the vectors table cannot be created here, so Open
// asks the catalog whether an earlier run already created it. That makes a
// restart over a populated corpus queryable before it embeds anything, and it
// keeps a fresh corpus an empty index rather than an error (issue #666).
func Open(ctx context.Context, cfg Config) (*Index, error) {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("pgvector: connect: %w (check index.pgvector.dsn and that the server is reachable)", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgvector: ping: %w (check index.pgvector.dsn and that the server is reachable)", err)
	}
	ix := &Index{pool: pool, db: pool, schema: cfg.Schema, table: cfg.Table, dim: cfg.Dim}
	if err := ix.ensureBaseSchema(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	// When the dimension is known up front, create the vectors table eagerly so
	// a connection/privilege problem surfaces at startup rather than on the
	// first embedded chunk. With dim==0 the table is created lazily, so ask the
	// catalog whether a previous run already created it: a restart over a
	// populated corpus must serve searches before it embeds anything (issue
	// #666).
	if cfg.Dim > 0 {
		if err := ix.ensureVectorsTable(ctx, cfg.Dim); err != nil {
			pool.Close()
			return nil, err
		}
		return ix, nil
	}
	if _, err := ix.vectorsTablePresent(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return ix, nil
}

// NewWithQuerier constructs an Index over a caller-supplied Querier without
// opening a real connection pool. It is the seam out-of-package tests use to
// exercise the Index methods against a stub Querier (no live database). Schema
// and Table fall back to the package defaults when empty; if tableReady is true
// the vectors table is assumed to already exist, so Upsert does not re-issue the
// DDL and Search/Delete do not check the catalog for it.
func NewWithQuerier(db Querier, cfg Config, tableReady bool) *Index {
	cfg = cfg.withDefaults()
	return &Index{
		db:          db,
		schema:      cfg.Schema,
		table:       cfg.Table,
		dim:         cfg.Dim,
		tableReady:  tableReady,
		tableExists: tableReady,
	}
}

// ensureBaseSchema creates the pgvector extension and the identity table, which
// are independent of the embedding dimension.
func (i *Index) ensureBaseSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE EXTENSION IF NOT EXISTS vector`,
		CreateIdentityTableSQL(i.schema, i.table),
	}
	for _, s := range stmts {
		if _, err := i.db.Exec(ctx, s); err != nil {
			return fmt.Errorf("pgvector: ensure schema: %w (the connection role needs CREATE privileges and the pgvector extension must be installable)", err)
		}
	}
	return nil
}

// ensureVectorsTable creates the dimensioned vectors table and, when the
// dimension is within pgvector's index limit (HNSWMaxDim), its HNSW index, if
// not already created in this process. dim must be positive. It is idempotent
// and safe under concurrent callers.
func (i *Index) ensureVectorsTable(ctx context.Context, dim int) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.tableReady {
		return nil
	}
	if dim <= 0 {
		return fmt.Errorf("pgvector: embedding dimension must be positive, got %d", dim)
	}
	stmts := []string{CreateTableSQL(i.schema, i.table, dim)}
	// pgvector's hnsw/ivfflat indexes are capped at HNSWMaxDim dimensions; above
	// that, CREATE INDEX fails and would leave tableReady false and every Upsert
	// re-failing corpus-wide (issue #437 F2). For high-dimensional models we skip
	// the ANN index — the table still answers queries via exact search — and warn
	// the operator rather than failing the backend.
	if idxSQL, ok := CreateHNSWIndexSQL(i.schema, i.table, dim); ok {
		stmts = append(stmts, idxSQL)
	} else {
		log.Printf("pgvector: ANN (HNSW) indexing disabled for table %q: embedding dimension %d exceeds pgvector's %d-dim index limit; falling back to exact (sequential-scan) search. For faster ANN search, use a smaller Matryoshka embedding dimension (<= %d).",
			i.table, dim, HNSWMaxDim, HNSWMaxDim)
	}
	for _, s := range stmts {
		if _, err := i.db.Exec(ctx, s); err != nil {
			return fmt.Errorf("pgvector: ensure vectors table: %w (the connection role needs CREATE privileges)", err)
		}
	}
	i.dim = dim
	i.tableReady = true
	i.tableExists = true
	return nil
}

// vectorsTablePresent reports whether the vectors table exists. The answer comes
// from the database catalog, never from this process's memory, so a table another
// process (or an earlier run) created is found: a fresh index is an empty index,
// but a populated table must never be reported empty (issue #666). A positive
// answer is cached; a negative one is not, because the table appears as soon as
// something is embedded.
func (i *Index) vectorsTablePresent(ctx context.Context) (bool, error) {
	i.mu.Lock()
	known := i.tableExists
	i.mu.Unlock()
	if known {
		return true, nil
	}
	var present bool
	if err := i.db.QueryRow(ctx, tableExistsSQL, qualifiedTable(i.schema, i.table)).Scan(&present); err != nil {
		return false, fmt.Errorf("pgvector: check vectors table: %w", err)
	}
	if present {
		i.mu.Lock()
		i.tableExists = true
		i.mu.Unlock()
	}
	return present, nil
}

// forgetVectorsTable clears the cached presence and readiness flags. It is called
// when the database reports the table missing under a query that expected it,
// which happens when another process dropped it (Reset) after the check. The
// next call probes again, so the state repairs itself.
func (i *Index) forgetVectorsTable() {
	i.mu.Lock()
	i.tableExists = false
	i.tableReady = false
	i.mu.Unlock()
}

// isUndefinedTable reports whether err is Postgres' undefined_table (42P01).
func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == undefinedTableCode
	}
	return false
}

// Upsert stores (or replaces) the vector and its payload, keyed by
// payload.ChunkID.
func (i *Index) Upsert(ctx context.Context, vector []float32, payload model.IndexPayload) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(vector) == 0 {
		return errors.New("vector cannot be empty")
	}
	if payload.ChunkID == 0 {
		return errors.New("payload chunk_id cannot be zero")
	}
	if err := i.ensureVectorsTable(ctx, len(vector)); err != nil {
		return err
	}
	sql, args, err := UpsertSQL(i.schema, i.table, vector, payload)
	if err != nil {
		return fmt.Errorf("pgvector: marshal payload: %w", err)
	}
	if _, err := i.db.Exec(ctx, sql, args...); err != nil {
		return fmt.Errorf("pgvector: upsert chunk %d: %w", payload.ChunkID, err)
	}
	return nil
}

// Delete removes the vectors and payloads for the given chunk IDs. Unknown IDs
// are ignored.
func (i *Index) Delete(ctx context.Context, chunkIDs []uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(chunkIDs) == 0 {
		return nil
	}
	// A vectors table that does not exist holds no vector, so there is nothing to
	// delete. Without this the first reconciliation of a corpus that has not
	// embedded anything yet fails with "relation ... does not exist" (issue #666).
	present, err := i.vectorsTablePresent(ctx)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	sql, args := DeleteSQL(i.schema, i.table, chunkIDs)
	if _, err := i.db.Exec(ctx, sql, args...); err != nil {
		if isUndefinedTable(err) {
			// Dropped between the check and the delete: still nothing to delete.
			i.forgetVectorsTable()
			return nil
		}
		return fmt.Errorf("pgvector: delete: %w", err)
	}
	return nil
}

// Search returns the k best matches for vector, ordered best-first. The
// pushable predicates of filter are evaluated in SQL; PathGlob (if set) is not,
// so retrieval applies the residual filter in Go (CanFilter reports false).
func (i *Index) Search(ctx context.Context, vector []float32, k int, filter model.Filter) ([]model.IndexHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(vector) == 0 {
		return nil, errors.New("query vector cannot be empty")
	}
	if k <= 0 {
		return []model.IndexHit{}, nil
	}
	// A vectors table that does not exist holds no vector, so the answer is zero
	// hits. Without this a search before the first embedded chunk fails with
	// "relation ... does not exist" (issue #666).
	present, err := i.vectorsTablePresent(ctx)
	if err != nil {
		return nil, err
	}
	if !present {
		return []model.IndexHit{}, nil
	}
	sql, args := SearchSQL(i.schema, i.table, vector, k, filter)
	rows, err := i.db.Query(ctx, sql, args...)
	if err != nil {
		if isUndefinedTable(err) {
			// Dropped between the check and the query: an empty index again.
			i.forgetVectorsTable()
			return []model.IndexHit{}, nil
		}
		return nil, fmt.Errorf("pgvector: search: %w", err)
	}
	defer rows.Close()
	return scanHits(rows)
}

// scanHits materialises IndexHits from a result set whose columns match
// SearchSQL's projection.
func scanHits(rows pgx.Rows) ([]model.IndexHit, error) {
	var hits []model.IndexHit
	for rows.Next() {
		var (
			chunkID                                   int64
			score                                     float64
			payloadJSON                               []byte
			rel, docType, modality, language, speaker string
			startMS, endMS                            int
		)
		if err := rows.Scan(&chunkID, &rel, &docType, &modality, &startMS, &endMS, &language, &speaker, &payloadJSON, &score); err != nil {
			return nil, fmt.Errorf("pgvector: scan: %w", err)
		}
		hit, err := RowToHit(chunkID, score, payloadJSON, rel, docType, modality, language, speaker, startMS, endMS)
		if err != nil {
			return nil, fmt.Errorf("pgvector: decode payload for chunk %d: %w", chunkID, err)
		}
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgvector: rows: %w", err)
	}
	return hits, nil
}

// CanFilter reports whether the backend can evaluate the filter entirely in
// SQL. It returns false when PathGlob is set (no faithful SQL equivalent), so
// retrieval falls back to overfetch-then-filter in Go.
func (i *Index) CanFilter(filter model.Filter) bool {
	return CanFilterFilter(filter)
}

// Identity returns the recorded corpus-lifetime embed identity, or "" when the
// index is fresh.
func (i *Index) Identity(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var identity string
	sql := fmt.Sprintf(`SELECT identity FROM %s WHERE id LIMIT 1`, identityTable(i.schema, i.table))
	err := i.db.QueryRow(ctx, sql).Scan(&identity)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("pgvector: read identity: %w", err)
	}
	return identity, nil
}

// Reset clears all vectors/payloads and records identity as the new
// corpus-lifetime embed identity.
func (i *Index) Reset(ctx context.Context, identity string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Drop rather than TRUNCATE: the vectors table may not exist yet (lazy
	// creation when the dimension is unknown), and a dimension change between
	// runs requires recreating the column with the new vector(dim) type. The
	// next Upsert recreates the table at the current dimension. DROP IF EXISTS
	// is a no-op when the table is absent.
	drop := fmt.Sprintf(`DROP TABLE IF EXISTS %s`, qualifiedTable(i.schema, i.table))
	if _, err := i.db.Exec(ctx, drop); err != nil {
		return fmt.Errorf("pgvector: reset vectors: %w", err)
	}
	i.mu.Lock()
	i.tableReady = false
	i.tableExists = false
	i.mu.Unlock()
	upsertIdentity := fmt.Sprintf(`INSERT INTO %s (id, identity) VALUES (true, $1)
ON CONFLICT (id) DO UPDATE SET identity = EXCLUDED.identity`, identityTable(i.schema, i.table))
	if _, err := i.db.Exec(ctx, upsertIdentity, identity); err != nil {
		return fmt.Errorf("pgvector: record identity: %w", err)
	}
	return nil
}

// Close releases the connection pool.
func (i *Index) Close() error {
	if i.pool != nil {
		i.pool.Close()
	}
	return nil
}

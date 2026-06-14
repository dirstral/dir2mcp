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

	mu         sync.Mutex
	dim        int  // 0 until known (config or first vector)
	tableReady bool // vectors table + HNSW index created
}

// Open connects to Postgres, verifies pgvector is available, and ensures the
// schema (vectors table + HNSW index + identity table) exists. A connection or
// extension failure returns a clear, remediable error — there is no silent
// fallback to the in-memory backend (issue #269).
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
	// first embedded chunk. With dim==0 the table is created lazily.
	if cfg.Dim > 0 {
		if err := ix.ensureVectorsTable(ctx, cfg.Dim); err != nil {
			pool.Close()
			return nil, err
		}
	}
	return ix, nil
}

// NewWithQuerier constructs an Index over a caller-supplied Querier without
// opening a real connection pool. It is the seam out-of-package tests use to
// exercise the Index methods against a stub Querier (no live database). Schema
// and Table fall back to the package defaults when empty; if tableReady is true
// the vectors table is assumed to already exist (Upsert will not re-issue DDL).
func NewWithQuerier(db Querier, cfg Config, tableReady bool) *Index {
	cfg = cfg.withDefaults()
	return &Index{
		db:         db,
		schema:     cfg.Schema,
		table:      cfg.Table,
		dim:        cfg.Dim,
		tableReady: tableReady,
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

// ensureVectorsTable creates the dimensioned vectors table and its HNSW index
// if not already created in this process. dim must be positive. It is
// idempotent and safe under concurrent callers.
func (i *Index) ensureVectorsTable(ctx context.Context, dim int) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.tableReady {
		return nil
	}
	if dim <= 0 {
		return fmt.Errorf("pgvector: embedding dimension must be positive, got %d", dim)
	}
	stmts := []string{
		CreateTableSQL(i.schema, i.table, dim),
		CreateHNSWIndexSQL(i.schema, i.table),
	}
	for _, s := range stmts {
		if _, err := i.db.Exec(ctx, s); err != nil {
			return fmt.Errorf("pgvector: ensure vectors table: %w (the connection role needs CREATE privileges)", err)
		}
	}
	i.dim = dim
	i.tableReady = true
	return nil
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
	sql, args := DeleteSQL(i.schema, i.table, chunkIDs)
	if _, err := i.db.Exec(ctx, sql, args...); err != nil {
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
	sql, args := SearchSQL(i.schema, i.table, vector, k, filter)
	rows, err := i.db.Query(ctx, sql, args...)
	if err != nil {
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

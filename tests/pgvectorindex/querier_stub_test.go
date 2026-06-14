package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/dirstral/dir2mcp/internal/index/pgvectorindex"
	"github.com/dirstral/dir2mcp/internal/model"
)

// execCall records one Exec invocation against the stub querier.
type execCall struct {
	sql  string
	args []any
}

// stubQuerier is a minimal pgvectorindex.Querier that records Exec calls and
// returns a canned identity from QueryRow, so the Index methods can be exercised
// with no live database.
type stubQuerier struct {
	execs    []execCall
	identity string
	noRows   bool
}

func (s *stubQuerier) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	s.execs = append(s.execs, execCall{sql: sql, args: args})
	return pgconn.NewCommandTag("OK"), nil
}

func (s *stubQuerier) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}

func (s *stubQuerier) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return &stubRow{identity: s.identity, noRows: s.noRows}
}

type stubRow struct {
	identity string
	noRows   bool
}

func (r *stubRow) Scan(dest ...any) error {
	if r.noRows {
		return pgx.ErrNoRows
	}
	if len(dest) == 1 {
		if p, ok := dest[0].(*string); ok {
			*p = r.identity
		}
	}
	return nil
}

// TestIndex_Upsert_StubQuerier verifies Upsert lazily creates the vectors table
// (DDL on first call) and then issues the INSERT ... ON CONFLICT with the scalar
// args parameterised.
func TestIndex_Upsert_StubQuerier(t *testing.T) {
	stub := &stubQuerier{}
	// tableReady=false: the first Upsert must emit the CREATE TABLE / CREATE INDEX
	// DDL before the INSERT.
	ix := pgvectorindex.NewWithQuerier(stub, pgvectorindex.Config{}, false)

	p := model.IndexPayload{ChunkID: 7, RelPath: "a.md", DocType: "md"}
	if err := ix.Upsert(context.Background(), []float32{0.1, 0.2}, p); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if len(stub.execs) < 2 {
		t.Fatalf("expected DDL + insert execs, got %d", len(stub.execs))
	}
	last := stub.execs[len(stub.execs)-1]
	if !strings.Contains(last.sql, "ON CONFLICT (chunk_id) DO UPDATE") {
		t.Fatalf("last exec is not the upsert: %q", last.sql)
	}
	// chunk_id must be a parameterised arg, not interpolated.
	if last.args[0] != int64(7) {
		t.Fatalf("expected chunk_id arg int64(7), got %T %v", last.args[0], last.args[0])
	}

	// A second Upsert must NOT re-issue DDL (table already marked ready).
	before := len(stub.execs)
	if err := ix.Upsert(context.Background(), []float32{0.3, 0.4}, model.IndexPayload{ChunkID: 8}); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	if got := len(stub.execs) - before; got != 1 {
		t.Fatalf("second Upsert should issue exactly 1 exec (no DDL), got %d", got)
	}
}

// TestIndex_Upsert_RejectsBadInput mirrors the HNSW contract: empty vector and
// zero chunk_id are errors.
func TestIndex_Upsert_RejectsBadInput(t *testing.T) {
	ix := pgvectorindex.NewWithQuerier(&stubQuerier{}, pgvectorindex.Config{}, true)
	if err := ix.Upsert(context.Background(), nil, model.IndexPayload{ChunkID: 1}); err == nil {
		t.Fatalf("expected error for empty vector")
	}
	if err := ix.Upsert(context.Background(), []float32{1}, model.IndexPayload{ChunkID: 0}); err == nil {
		t.Fatalf("expected error for zero chunk_id")
	}
}

// TestIndex_Delete_StubQuerier verifies Delete issues one DELETE with the ID
// array arg and is a no-op for an empty slice.
func TestIndex_Delete_StubQuerier(t *testing.T) {
	stub := &stubQuerier{}
	ix := pgvectorindex.NewWithQuerier(stub, pgvectorindex.Config{}, true)

	if err := ix.Delete(context.Background(), nil); err != nil {
		t.Fatalf("empty Delete: %v", err)
	}
	if len(stub.execs) != 0 {
		t.Fatalf("empty Delete should issue no exec, got %d", len(stub.execs))
	}

	if err := ix.Delete(context.Background(), []uint64{1, 2}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(stub.execs) != 1 || !strings.Contains(stub.execs[0].sql, "chunk_id = ANY($1)") {
		t.Fatalf("unexpected delete exec: %+v", stub.execs)
	}
}

// TestIndex_Identity_StubQuerier covers both the recorded-identity path and the
// fresh (ErrNoRows -> "") path.
func TestIndex_Identity_StubQuerier(t *testing.T) {
	ix := pgvectorindex.NewWithQuerier(&stubQuerier{identity: "openai:text-embedding-3-small:1536"}, pgvectorindex.Config{}, true)
	got, err := ix.Identity(context.Background())
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if got != "openai:text-embedding-3-small:1536" {
		t.Fatalf("identity = %q", got)
	}

	fresh := pgvectorindex.NewWithQuerier(&stubQuerier{noRows: true}, pgvectorindex.Config{}, true)
	got, err = fresh.Identity(context.Background())
	if err != nil {
		t.Fatalf("fresh Identity: %v", err)
	}
	if got != "" {
		t.Fatalf("fresh identity should be empty, got %q", got)
	}
}

// TestIndex_Reset_StubQuerier verifies Reset drops the vectors table and upserts
// the new identity (parameterised), and clears the table-ready flag so the next
// Upsert recreates the table.
func TestIndex_Reset_StubQuerier(t *testing.T) {
	stub := &stubQuerier{}
	ix := pgvectorindex.NewWithQuerier(stub, pgvectorindex.Config{}, true)

	if err := ix.Reset(context.Background(), "id-1"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if len(stub.execs) != 2 {
		t.Fatalf("expected DROP + identity upsert, got %d: %+v", len(stub.execs), stub.execs)
	}
	if !strings.Contains(stub.execs[0].sql, "DROP TABLE IF EXISTS") {
		t.Fatalf("first reset exec should DROP, got %q", stub.execs[0].sql)
	}
	idUpsert := stub.execs[1]
	if !strings.Contains(idUpsert.sql, "ON CONFLICT (id) DO UPDATE") {
		t.Fatalf("second reset exec should upsert identity, got %q", idUpsert.sql)
	}
	if len(idUpsert.args) != 1 || idUpsert.args[0] != "id-1" {
		t.Fatalf("identity must be a parameterised arg, got %+v", idUpsert.args)
	}

	// After Reset the table must be recreated on the next Upsert (DDL re-issued).
	before := len(stub.execs)
	if err := ix.Upsert(context.Background(), []float32{1}, model.IndexPayload{ChunkID: 1}); err != nil {
		t.Fatalf("post-reset Upsert: %v", err)
	}
	if got := len(stub.execs) - before; got < 2 {
		t.Fatalf("post-reset Upsert should re-issue DDL, got %d execs", got)
	}
}

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

// lazyTableQuerier is a Querier that behaves like a Postgres database in which
// the dimensioned vectors table is created lazily: while the table is absent,
// every statement against it fails with undefined_table (42P01), exactly as the
// server does. It records what the Index issued so the tests can assert that no
// statement was sent to a table that does not exist (issue #666).
type lazyTableQuerier struct {
	// tablePresent mirrors the database: the vectors table exists or it does not.
	tablePresent bool
	// searchRows, when > 0, is the number of rows a search over a present table
	// returns.
	searchRows int

	execSQL   []string
	querySQL  []string
	probes    int
	dropSeen  bool
	forceGone bool // report present, then fail the statement: a concurrent drop
}

func (q *lazyTableQuerier) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	q.execSQL = append(q.execSQL, sql)
	if strings.Contains(sql, "DROP TABLE") {
		q.dropSeen = true
		q.tablePresent = false
		return pgconn.NewCommandTag("DROP TABLE"), nil
	}
	if strings.Contains(sql, "CREATE TABLE IF NOT EXISTS") && !strings.Contains(sql, "_identity") {
		q.tablePresent = true
		return pgconn.NewCommandTag("CREATE TABLE"), nil
	}
	if q.touchesVectors(sql) && !q.tablePresent {
		return pgconn.CommandTag{}, undefinedTable()
	}
	return pgconn.NewCommandTag("OK"), nil
}

func (q *lazyTableQuerier) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	q.querySQL = append(q.querySQL, sql)
	if q.touchesVectors(sql) && (!q.tablePresent || q.forceGone) {
		return nil, undefinedTable()
	}
	return &cannedRows{remaining: q.searchRows}, nil
}

func (q *lazyTableQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if isCatalogProbe(sql) {
		q.probes++
		return &boolRow{value: q.tablePresent || q.forceGone}
	}
	return &boolRow{}
}

// isCatalogProbe reports whether sql asks the catalog whether a relation exists.
// Such a statement is valid whether or not the table is there.
func isCatalogProbe(sql string) bool {
	return strings.Contains(sql, "to_regclass") || strings.Contains(sql, "information_schema")
}

// touchesVectors reports whether sql reads or writes the vectors table (as
// opposed to the identity table or the catalog probe).
func (q *lazyTableQuerier) touchesVectors(sql string) bool {
	if strings.Contains(sql, "_identity") || isCatalogProbe(sql) {
		return false
	}
	return strings.Contains(sql, pgvectorindex.DefaultTable)
}

// undefinedTable is the error Postgres returns for a statement against a table
// that does not exist.
func undefinedTable() error {
	return &pgconn.PgError{
		Code:    "42P01",
		Message: `relation "public.dir2mcp_vectors" does not exist`,
	}
}

// boolRow scans a single boolean, which is what the table-presence probe reads.
type boolRow struct{ value bool }

func (r *boolRow) Scan(dest ...any) error {
	if len(dest) == 1 {
		if p, ok := dest[0].(*bool); ok {
			*p = r.value
		}
	}
	return nil
}

// cannedRows is a pgx.Rows over a fixed number of identical search rows.
type cannedRows struct{ remaining int }

func (r *cannedRows) Close()                                       {}
func (r *cannedRows) Err() error                                   { return nil }
func (r *cannedRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *cannedRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *cannedRows) Values() ([]any, error)                       { return nil, nil }
func (r *cannedRows) RawValues() [][]byte                          { return nil }
func (r *cannedRows) Conn() *pgx.Conn                              { return nil }

func (r *cannedRows) Next() bool {
	if r.remaining <= 0 {
		return false
	}
	r.remaining--
	return true
}

// Scan fills the projection SearchSQL selects, in order.
func (r *cannedRows) Scan(dest ...any) error {
	set := func(i int, v any) {
		switch p := dest[i].(type) {
		case *int64:
			*p = v.(int64)
		case *string:
			*p = v.(string)
		case *int:
			*p = v.(int)
		case *float64:
			*p = v.(float64)
		case *[]byte:
			*p = v.([]byte)
		}
	}
	set(0, int64(42))
	set(1, "a.md")
	set(2, "md")
	set(3, "text")
	set(4, 0)
	set(5, 0)
	set(6, "en")
	set(7, "")
	set(8, []byte(`{}`))
	set(9, 0.9)
	return nil
}

// TestPgvector_SearchBeforeFirstUpsertIsEmpty drives #666: the vectors table is
// created on the first Upsert, so a search before any chunk is embedded must
// report an empty index rather than fail on a missing relation.
func TestPgvector_SearchBeforeFirstUpsertIsEmpty(t *testing.T) {
	q := &lazyTableQuerier{}
	ix := pgvectorindex.NewWithQuerier(q, pgvectorindex.Config{}, false)

	hits, err := ix.Search(context.Background(), []float32{0.1, 0.2}, 5, model.Filter{})
	if err != nil {
		t.Fatalf("Search on a fresh index must succeed, got: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("hits = %d, want 0", len(hits))
	}
	for _, sql := range q.querySQL {
		if strings.Contains(sql, pgvectorindex.DefaultTable) {
			t.Fatalf("no statement may be sent to a table that does not exist, got: %q", sql)
		}
	}
}

// TestPgvector_DeleteBeforeFirstUpsertIsNoop drives the other half of #666: a
// reconciliation that deletes chunk ids before anything is embedded must be a
// no-op, not an error.
func TestPgvector_DeleteBeforeFirstUpsertIsNoop(t *testing.T) {
	q := &lazyTableQuerier{}
	ix := pgvectorindex.NewWithQuerier(q, pgvectorindex.Config{}, false)

	if err := ix.Delete(context.Background(), []uint64{1, 2, 3}); err != nil {
		t.Fatalf("Delete on a fresh index must succeed, got: %v", err)
	}
	for _, sql := range q.execSQL {
		if strings.Contains(sql, "DELETE FROM") {
			t.Fatalf("no DELETE may be sent to a table that does not exist, got: %q", sql)
		}
	}
}

// TestPgvector_SearchAndDeleteOverAnExistingTableStillRunSQL is the guard that
// keeps the #666 fix from becoming a silent empty result. A restart over a
// populated corpus has embedded nothing yet in THIS process, so the readiness
// flag is false while the table is full. The presence check must come from the
// database, so both operations still run their SQL.
func TestPgvector_SearchAndDeleteOverAnExistingTableStillRunSQL(t *testing.T) {
	q := &lazyTableQuerier{tablePresent: true, searchRows: 1}
	ix := pgvectorindex.NewWithQuerier(q, pgvectorindex.Config{}, false)

	hits, err := ix.Search(context.Background(), []float32{0.1, 0.2}, 5, model.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ChunkID != 42 {
		t.Fatalf("hits = %+v, want the one stored row", hits)
	}
	if err := ix.Delete(context.Background(), []uint64{42}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var sawDelete bool
	for _, sql := range q.execSQL {
		if strings.Contains(sql, "DELETE FROM") {
			sawDelete = true
		}
	}
	if !sawDelete {
		t.Fatalf("Delete over a populated table must issue the DELETE, execs: %v", q.execSQL)
	}
	// The positive answer is cached: one probe covers both operations.
	if q.probes != 1 {
		t.Errorf("catalog probes = %d, want 1 (the presence answer is cached)", q.probes)
	}
}

// TestPgvector_ResetRequiresAFreshPresenceCheck covers the window Reset opens:
// it DROPs the table, so a search after it must not query the dropped table on
// the strength of a cached answer.
func TestPgvector_ResetRequiresAFreshPresenceCheck(t *testing.T) {
	q := &lazyTableQuerier{tablePresent: true, searchRows: 1}
	ix := pgvectorindex.NewWithQuerier(q, pgvectorindex.Config{}, true)

	if err := ix.Reset(context.Background(), "ident-A"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if !q.dropSeen {
		t.Fatal("Reset must drop the vectors table")
	}
	hits, err := ix.Search(context.Background(), []float32{0.1, 0.2}, 5, model.Filter{})
	if err != nil {
		t.Fatalf("Search after Reset must succeed, got: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("hits = %d, want 0 after Reset", len(hits))
	}
}

// TestPgvector_TableDroppedAfterTheCheckIsStillEmpty covers the one window the
// presence check cannot close: another process drops the table between the check
// and the statement. The undefined_table error is then read as the empty index it
// describes, and the cached answer is discarded so the next call checks again.
func TestPgvector_TableDroppedAfterTheCheckIsStillEmpty(t *testing.T) {
	q := &lazyTableQuerier{forceGone: true}
	ix := pgvectorindex.NewWithQuerier(q, pgvectorindex.Config{}, false)

	hits, err := ix.Search(context.Background(), []float32{0.1, 0.2}, 5, model.Filter{})
	if err != nil {
		t.Fatalf("Search must read a dropped table as empty, got: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("hits = %d, want 0", len(hits))
	}
	if err := ix.Delete(context.Background(), []uint64{1}); err != nil {
		t.Fatalf("Delete must read a dropped table as a no-op, got: %v", err)
	}
	if q.probes < 2 {
		t.Errorf("catalog probes = %d, want the cache discarded and re-checked", q.probes)
	}
}

package tests

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index/pgvectorindex"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TestBuildFilterPredicates_Empty verifies a zero filter yields no clauses and
// no args (the KNN query then has no WHERE).
func TestBuildFilterPredicates_Empty(t *testing.T) {
	clauses, args := pgvectorindex.BuildFilterPredicates(model.Filter{}, 1)
	if len(clauses) != 0 {
		t.Fatalf("expected no clauses, got %v", clauses)
	}
	if len(args) != 0 {
		t.Fatalf("expected no args, got %v", args)
	}
}

// TestBuildFilterPredicates_ExcludeOrphans maps to the btrim(rel_path) check
// and contributes no positional argument.
func TestBuildFilterPredicates_ExcludeOrphans(t *testing.T) {
	clauses, args := pgvectorindex.BuildFilterPredicates(model.Filter{ExcludeOrphans: true}, 1)
	if len(clauses) != 1 || !strings.Contains(clauses[0], "btrim(rel_path) <> ''") {
		t.Fatalf("unexpected clauses: %v", clauses)
	}
	if len(args) != 0 {
		t.Fatalf("ExcludeOrphans should add no args, got %v", args)
	}
}

// TestBuildFilterPredicates_PathPrefix produces a case-insensitive LIKE
// prefix||'%' predicate over a NORMALIZED prefix and escapes LIKE
// metacharacters in the literal prefix (issue #437 F1). NormalizePathPrefix
// strips the trailing slash via path.Clean, so the trailing "/" is gone.
func TestBuildFilterPredicates_PathPrefix(t *testing.T) {
	clauses, args := pgvectorindex.BuildFilterPredicates(model.Filter{PathPrefix: "docs/50%_off/"}, 1)
	if len(clauses) != 1 {
		t.Fatalf("expected 1 clause, got %v", clauses)
	}
	// The pushdown must be case-insensitive (lower() on both sides) to mirror
	// model.MatchesPathPrefix's ASCII fold.
	if !strings.Contains(clauses[0], "lower(rel_path) LIKE lower($1) ESCAPE") {
		t.Fatalf("expected case-insensitive LIKE lower($1) ESCAPE clause, got %q", clauses[0])
	}
	if len(args) != 1 {
		t.Fatalf("expected 1 arg, got %v", args)
	}
	got, ok := args[0].(string)
	if !ok {
		t.Fatalf("expected string arg, got %T", args[0])
	}
	// The % and _ in the prefix must be escaped; the trailing % wildcard must
	// remain unescaped. The trailing path separator is removed by normalization.
	want := `docs/50\%\_off%`
	if got != want {
		t.Fatalf("escaped prefix arg = %q, want %q", got, want)
	}
}

// TestBuildFilterPredicates_PathPrefix_Normalized verifies the prefix is run
// through model.NormalizePathPrefix so a "./docs"-style or differently-cased
// input produces the SAME pushdown the Go-side model.Filter.Match recheck uses
// (issue #437 F1 / #286). All variants must reduce to the same lowercased,
// normalized argument so the backend never silently under-returns rows.
func TestBuildFilterPredicates_PathPrefix_Normalized(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		want   string // expected LIKE argument (escaped, + trailing %)
	}{
		{"leading dot-slash", "./docs", `docs%`},
		{"trailing slash", "docs/", `docs%`},
		{"backslashes", `docs\sub`, `docs/sub%`},
		{"redundant separators", "docs//sub/", `docs/sub%`},
		{"mixed case preserved verbatim", "Docs", `Docs%`},
		{"leading slash stripped", "/docs", `docs%`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clauses, args := pgvectorindex.BuildFilterPredicates(model.Filter{PathPrefix: tc.prefix}, 1)
			if len(clauses) != 1 {
				t.Fatalf("expected 1 clause, got %v", clauses)
			}
			if !strings.Contains(clauses[0], "lower(rel_path) LIKE lower($1) ESCAPE") {
				t.Fatalf("expected case-insensitive normalized LIKE clause, got %q", clauses[0])
			}
			if len(args) != 1 || args[0] != tc.want {
				t.Fatalf("LIKE arg = %v, want %q", args, tc.want)
			}
		})
	}
}

// TestBuildFilterPredicates_PathPrefix_NormalizesAway verifies a prefix that
// reduces to "no prefix" (".", "./") imposes NO constraint, matching the
// matcher (MatchesPathPrefix returns true for everything in that case).
func TestBuildFilterPredicates_PathPrefix_NormalizesAway(t *testing.T) {
	for _, p := range []string{".", "./", "  ", `.\`} {
		clauses, args := pgvectorindex.BuildFilterPredicates(model.Filter{PathPrefix: p}, 1)
		if len(clauses) != 0 || len(args) != 0 {
			t.Fatalf("prefix %q should add no predicate, got clauses=%v args=%v", p, clauses, args)
		}
	}
}

// TestBuildFilterPredicates_PathPrefix_AgreesWithMatch is the cross-check that
// the F1 fix exists to guarantee: every row the SQL pushdown is meant to keep
// is exactly the set model.Filter.Match keeps. We assert agreement on the
// hard cases (case-fold + form mismatch) by simulating the lowercased LIKE
// prefix test in Go and comparing against model.Filter.Match.
func TestBuildFilterPredicates_PathPrefix_AgreesWithMatch(t *testing.T) {
	relPaths := []string{
		"Docs/readme.md", "docs/readme.md", "DOCS/a/b.md",
		"other/x.md", "docsy/x.md", "doc/x.md",
	}
	prefixes := []string{"docs", "Docs", "./docs/", `docs\`, "DOCS/"}
	for _, prefix := range prefixes {
		f := model.Filter{PathPrefix: prefix}
		clauses, args := pgvectorindex.BuildFilterPredicates(f, 1)
		// Recover the lowercased, normalized LIKE pattern (strip trailing %).
		var pattern string
		if len(args) == 1 {
			pattern = strings.ToLower(strings.TrimSuffix(args[0].(string), "%"))
		}
		_ = clauses
		for _, rp := range relPaths {
			// Emulate "lower(rel_path) LIKE lower(prefix)||'%'": since the only
			// metacharacters were escaped away in these prefixes, this is a plain
			// case-insensitive prefix test.
			sqlKeeps := pattern == "" || strings.HasPrefix(strings.ToLower(rp), pattern)
			goKeeps := f.Match(model.IndexPayload{RelPath: rp})
			if sqlKeeps != goKeeps {
				t.Errorf("prefix %q rel %q: SQL pushdown keeps=%v but Filter.Match keeps=%v",
					prefix, rp, sqlKeeps, goKeeps)
			}
		}
	}
}

// TestBuildFilterPredicates_DocTypes lower-cases the set and uses = ANY().
func TestBuildFilterPredicates_DocTypes(t *testing.T) {
	clauses, args := pgvectorindex.BuildFilterPredicates(model.Filter{DocTypes: []string{"MD", " Code "}}, 1)
	if len(clauses) != 1 || !strings.Contains(clauses[0], "lower(doc_type) = ANY($1)") {
		t.Fatalf("unexpected clauses: %v", clauses)
	}
	if len(args) != 1 {
		t.Fatalf("expected 1 arg, got %v", args)
	}
	got, ok := args[0].([]string)
	if !ok {
		t.Fatalf("expected []string arg, got %T", args[0])
	}
	want := []string{"md", "code"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lowered doc types = %v, want %v", got, want)
	}
}

// TestBuildFilterPredicates_Combined verifies placeholder numbering increments
// across predicates and starts at the requested index.
func TestBuildFilterPredicates_Combined(t *testing.T) {
	f := model.Filter{ExcludeOrphans: true, PathPrefix: "a/", DocTypes: []string{"md"}}
	clauses, args := pgvectorindex.BuildFilterPredicates(f, 1)
	if len(clauses) != 3 {
		t.Fatalf("expected 3 clauses, got %d: %v", len(clauses), clauses)
	}
	// ExcludeOrphans contributes no arg; PathPrefix is $1, DocTypes is $2.
	if !strings.Contains(clauses[1], "$1") {
		t.Fatalf("expected PathPrefix at $1, got %q", clauses[1])
	}
	if !strings.Contains(clauses[2], "$2") {
		t.Fatalf("expected DocTypes at $2, got %q", clauses[2])
	}
	if len(args) != 2 {
		t.Fatalf("expected 2 args, got %v", args)
	}
}

// TestBuildFilterPredicates_StartArg honours a non-1 starting placeholder.
func TestBuildFilterPredicates_StartArg(t *testing.T) {
	clauses, _ := pgvectorindex.BuildFilterPredicates(model.Filter{PathPrefix: "a/"}, 5)
	if !strings.Contains(clauses[0], "$5") {
		t.Fatalf("expected $5 placeholder, got %q", clauses[0])
	}
}

// TestSearchSQL_NoFilter builds a KNN query with no WHERE, ordering by the
// cosine distance operator and limiting via the trailing placeholder.
func TestSearchSQL_NoFilter(t *testing.T) {
	sql, args := pgvectorindex.SearchSQL("public", "vt", []float32{1, 0, 0.5}, 7, model.Filter{})
	if strings.Contains(sql, "WHERE") {
		t.Fatalf("expected no WHERE for empty filter, got:\n%s", sql)
	}
	if !strings.Contains(sql, "embedding <=> '[1,0,0.5]'::vector") {
		t.Fatalf("expected cosine order-by with vector literal, got:\n%s", sql)
	}
	if !strings.Contains(sql, "ORDER BY embedding <=>") {
		t.Fatalf("expected ORDER BY cosine distance, got:\n%s", sql)
	}
	if !strings.Contains(sql, "1 - (embedding <=>") {
		t.Fatalf("expected score = 1 - distance projection, got:\n%s", sql)
	}
	// k is the only positional arg, at $1 (no filter args precede it).
	if !strings.Contains(sql, "LIMIT $1") {
		t.Fatalf("expected LIMIT $1, got:\n%s", sql)
	}
	if len(args) != 1 || args[0] != 7 {
		t.Fatalf("expected args [7], got %v", args)
	}
}

// TestSearchSQL_WithFilter places the LIMIT placeholder after the filter args.
func TestSearchSQL_WithFilter(t *testing.T) {
	f := model.Filter{PathPrefix: "docs/", DocTypes: []string{"md"}}
	sql, args := pgvectorindex.SearchSQL("s", "t", []float32{1}, 3, f)
	if !strings.Contains(sql, "WHERE") {
		t.Fatalf("expected WHERE clause, got:\n%s", sql)
	}
	// 2 filter args ($1, $2) then LIMIT $3.
	if !strings.Contains(sql, "LIMIT $3") {
		t.Fatalf("expected LIMIT $3, got:\n%s", sql)
	}
	if len(args) != 3 || args[2] != 3 {
		t.Fatalf("expected 3 args ending in k=3, got %v", args)
	}
	if !strings.Contains(sql, `"s"."t"`) {
		t.Fatalf("expected quoted schema-qualified table, got:\n%s", sql)
	}
}

// TestUpsertSQL builds an INSERT ... ON CONFLICT and aligns args 1:1 with the
// scalar placeholders ($1..$9); the vector is interpolated as a literal.
func TestUpsertSQL(t *testing.T) {
	p := model.IndexPayload{
		ChunkID:  42,
		RelPath:  "docs/a.md",
		DocType:  "md",
		Modality: "text",
		StartMS:  100,
		EndMS:    200,
		Language: "en",
		Speaker:  "alice",
	}
	sql, args, err := pgvectorindex.UpsertSQL("public", "vt", []float32{0.1, 0.2}, p)
	if err != nil {
		t.Fatalf("UpsertSQL: %v", err)
	}
	if !strings.Contains(sql, "ON CONFLICT (chunk_id) DO UPDATE") {
		t.Fatalf("expected ON CONFLICT upsert, got:\n%s", sql)
	}
	if !strings.Contains(sql, "'[0.1,0.2]'::vector") {
		t.Fatalf("expected vector literal, got:\n%s", sql)
	}
	if len(args) != 9 {
		t.Fatalf("expected 9 scalar args, got %d: %v", len(args), args)
	}
	if args[0] != int64(42) {
		t.Fatalf("expected chunk_id arg int64(42), got %T %v", args[0], args[0])
	}
	if args[1] != "docs/a.md" {
		t.Fatalf("expected rel_path arg, got %v", args[1])
	}
	// payload_json is the 9th arg ([]byte); it must round-trip the full payload.
	pj, ok := args[8].([]byte)
	if !ok {
		t.Fatalf("expected payload_json []byte, got %T", args[8])
	}
	round, err := pgvectorindex.UnmarshalPayload(pj)
	if err != nil {
		t.Fatalf("UnmarshalPayload: %v", err)
	}
	if round.ChunkID != 42 || round.Speaker != "alice" || round.Language != "en" {
		t.Fatalf("payload_json did not round-trip: %+v", round)
	}
}

// TestDeleteSQL passes chunk IDs as a single int64 array argument.
func TestDeleteSQL(t *testing.T) {
	sql, args := pgvectorindex.DeleteSQL("public", "vt", []uint64{1, 2, 3})
	if !strings.Contains(sql, "chunk_id = ANY($1)") {
		t.Fatalf("expected ANY($1) delete, got:\n%s", sql)
	}
	if len(args) != 1 {
		t.Fatalf("expected 1 arg, got %v", args)
	}
	got, ok := args[0].([]int64)
	if !ok {
		t.Fatalf("expected []int64 arg, got %T", args[0])
	}
	if !reflect.DeepEqual(got, []int64{1, 2, 3}) {
		t.Fatalf("ids = %v, want [1 2 3]", got)
	}
}

// TestCanFilterFilter reports false only when PathGlob is set (no SQL
// equivalent), true for every SQL-pushable predicate combination.
func TestCanFilterFilter(t *testing.T) {
	cases := []struct {
		name string
		f    model.Filter
		want bool
	}{
		{"empty", model.Filter{}, true},
		{"prefix", model.Filter{PathPrefix: "a/"}, true},
		{"doctypes", model.Filter{DocTypes: []string{"md"}}, true},
		{"orphans", model.Filter{ExcludeOrphans: true}, true},
		{"glob", model.Filter{PathGlob: "*.md"}, false},
		{"glob+prefix", model.Filter{PathGlob: "*.md", PathPrefix: "a/"}, false},
	}
	for _, tc := range cases {
		if got := pgvectorindex.CanFilterFilter(tc.f); got != tc.want {
			t.Errorf("%s: CanFilterFilter = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRowToHit reconstructs an IndexHit, with the scalar columns overriding the
// payload_json fields and chunkID/score taken from the row.
func TestRowToHit(t *testing.T) {
	// payload_json carries a Span and a stale rel_path; the scalar column wins.
	full := model.IndexPayload{
		ChunkID: 9,
		RelPath: "stale/old.md",
		Title:   "Doc Title",
		Span:    model.Span{Kind: "line", StartLine: 3, EndLine: 7},
	}
	pj, err := pgvectorindex.MarshalPayload(full)
	if err != nil {
		t.Fatalf("MarshalPayload: %v", err)
	}
	hit, err := pgvectorindex.RowToHit(9, 0.875, pj, "docs/new.md", "md", "text", "en", "bob", 10, 20)
	if err != nil {
		t.Fatalf("RowToHit: %v", err)
	}
	if hit.ChunkID != 9 {
		t.Fatalf("chunk id = %d, want 9", hit.ChunkID)
	}
	if hit.Score != float32(0.875) {
		t.Fatalf("score = %v, want 0.875", hit.Score)
	}
	if hit.Payload.RelPath != "docs/new.md" {
		t.Fatalf("rel_path column should override payload, got %q", hit.Payload.RelPath)
	}
	if hit.Payload.DocType != "md" || hit.Payload.Language != "en" || hit.Payload.Speaker != "bob" {
		t.Fatalf("scalar columns not applied: %+v", hit.Payload)
	}
	if hit.Payload.StartMS != 10 || hit.Payload.EndMS != 20 {
		t.Fatalf("ms columns not applied: %+v", hit.Payload)
	}
	// Fields without a dedicated column (Title, Span) come from payload_json.
	if hit.Payload.Title != "Doc Title" {
		t.Fatalf("title should come from payload_json, got %q", hit.Payload.Title)
	}
	if hit.Payload.Span.Kind != "line" || hit.Payload.Span.EndLine != 7 {
		t.Fatalf("span should come from payload_json, got %+v", hit.Payload.Span)
	}
}

// TestRowToHit_EmptyPayload tolerates a NULL/empty payload_json blob.
func TestRowToHit_EmptyPayload(t *testing.T) {
	hit, err := pgvectorindex.RowToHit(1, 0.5, nil, "a.md", "md", "", "", "", 0, 0)
	if err != nil {
		t.Fatalf("RowToHit empty payload: %v", err)
	}
	if hit.ChunkID != 1 || hit.Payload.RelPath != "a.md" {
		t.Fatalf("unexpected hit: %+v", hit)
	}
}

// TestValidateIdentifier accepts safe identifiers and rejects unsafe ones.
func TestValidateIdentifier(t *testing.T) {
	good := []string{"public", "dir2mcp_vectors", "t1", "_x"}
	for _, g := range good {
		if err := pgvectorindex.ValidateIdentifier("table", g); err != nil {
			t.Errorf("ValidateIdentifier(%q) unexpected error: %v", g, err)
		}
	}
	bad := []string{"", "1abc", "a.b", `a"b`, "a b", "drop;table", "schema-name"}
	for _, b := range bad {
		if err := pgvectorindex.ValidateIdentifier("table", b); err == nil {
			t.Errorf("ValidateIdentifier(%q) expected error, got nil", b)
		}
	}
}

// TestCreateTableSQL embeds the dimension and quotes the table.
func TestCreateTableSQL(t *testing.T) {
	sql := pgvectorindex.CreateTableSQL("public", "vt", 768)
	if !strings.Contains(sql, "vector(768)") {
		t.Fatalf("expected vector(768) column, got:\n%s", sql)
	}
	if !strings.Contains(sql, `"public"."vt"`) {
		t.Fatalf("expected quoted qualified table, got:\n%s", sql)
	}
	if !strings.Contains(sql, "chunk_id      bigint PRIMARY KEY") {
		t.Fatalf("expected chunk_id PK, got:\n%s", sql)
	}
}

// TestCreateHNSWIndexSQL uses the cosine ops class for in-limit dimensions and
// reports ok=true (issue #437 F2).
func TestCreateHNSWIndexSQL(t *testing.T) {
	sql, ok := pgvectorindex.CreateHNSWIndexSQL("public", "vt", 768)
	if !ok {
		t.Fatalf("expected ok=true for dim 768")
	}
	if !strings.Contains(sql, "USING hnsw (embedding vector_cosine_ops)") {
		t.Fatalf("expected HNSW cosine index, got:\n%s", sql)
	}
}

// TestCreateHNSWIndexSQL_DimGuard verifies the index is created at and below the
// 2000-dim pgvector limit and SKIPPED (ok=false, empty SQL) above it, so a
// high-dimensional model (e.g. gemini-embedding-001's 3072) falls back to exact
// search instead of permanently breaking the backend (issue #437 F2).
func TestCreateHNSWIndexSQL_DimGuard(t *testing.T) {
	cases := []struct {
		dim    int
		wantOK bool
	}{
		{1, true},
		{768, true},
		{1536, true},
		{2000, true},  // exactly the limit is still indexable
		{2001, false}, // one over the limit
		{3072, false}, // gemini-embedding-001 native dim
	}
	for _, tc := range cases {
		sql, ok := pgvectorindex.CreateHNSWIndexSQL("public", "vt", tc.dim)
		if ok != tc.wantOK {
			t.Errorf("dim %d: ok = %v, want %v", tc.dim, ok, tc.wantOK)
		}
		if ok && !strings.Contains(sql, "USING hnsw") {
			t.Errorf("dim %d: expected HNSW DDL, got:\n%s", tc.dim, sql)
		}
		if !ok && sql != "" {
			t.Errorf("dim %d: expected empty SQL when skipped, got:\n%s", tc.dim, sql)
		}
	}
	if pgvectorindex.HNSWMaxDim != 2000 {
		t.Fatalf("HNSWMaxDim = %d, want 2000 (pgvector hnsw/ivfflat hard limit)", pgvectorindex.HNSWMaxDim)
	}
}

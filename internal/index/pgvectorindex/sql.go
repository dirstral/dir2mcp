package pgvectorindex

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/dirstral/dir2mcp/internal/model"
)

// DefaultSchema and DefaultTable are used when the operator does not override
// them in config. The schema defaults to Postgres' implicit "public" search
// path; the table name is dir2mcp-specific so it does not collide with
// unrelated objects.
const (
	DefaultSchema = "public"
	DefaultTable  = "dir2mcp_vectors"

	// identityTableSuffix is appended to the vectors table name to form the
	// single-row identity table (e.g. dir2mcp_vectors_identity). It records the
	// corpus-lifetime embed identity (SPEC 8.1.4) so a vector space built under
	// a different embed provider/model/dimension is reset, never silently
	// reused.
	identityTableSuffix = "_identity"
)

// quoteIdent quotes a SQL identifier (schema or table name) for safe
// interpolation. pgx does not parameterise identifiers, so DDL/queries
// interpolate them; double any embedded quote and wrap in double-quotes per the
// SQL standard. Config-sourced identifiers are validated by ValidateIdentifier
// before reaching here, but quoting is applied defensively regardless.
func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// qualifiedTable returns the schema-qualified, quoted table name.
func qualifiedTable(schema, table string) string {
	return quoteIdent(schema) + "." + quoteIdent(table)
}

// identityTable returns the schema-qualified, quoted identity table name.
func identityTable(schema, table string) string {
	return quoteIdent(schema) + "." + quoteIdent(table+identityTableSuffix)
}

// ValidateIdentifier rejects schema/table names that are not safe, unqualified
// SQL identifiers. It permits letters, digits, and underscores, must not be
// empty, and must not start with a digit. This is intentionally stricter than
// Postgres so config-sourced names cannot smuggle quoting, dots, or whitespace
// into interpolated DDL.
func ValidateIdentifier(kind, ident string) error {
	if ident == "" {
		return fmt.Errorf("%s name must not be empty", kind)
	}
	for i, r := range ident {
		isLetter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		isDigit := r >= '0' && r <= '9'
		isUnderscore := r == '_'
		if isLetter || isUnderscore {
			continue
		}
		if isDigit && i > 0 {
			continue
		}
		return fmt.Errorf("%s name %q contains an invalid character %q (allowed: letters, digits, underscore; must not start with a digit)", kind, ident, string(r))
	}
	return nil
}

// vectorLiteral formats a float32 slice as a pgvector text literal, e.g.
// "[0.1,0.2,0.3]". pgvector accepts this form for both storage and the <=>
// distance operator's right-hand operand.
func vectorLiteral(vec []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(v), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// CreateTableSQL returns the DDL to create the vectors table (if absent) with
// the given embedding dimension, plus the HNSW index on the vector column using
// cosine distance (vector_cosine_ops). dim must be positive.
func CreateTableSQL(schema, table string, dim int) string {
	tbl := qualifiedTable(schema, table)
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	chunk_id      bigint PRIMARY KEY,
	rel_path      text NOT NULL DEFAULT '',
	doc_type      text NOT NULL DEFAULT '',
	modality      text NOT NULL DEFAULT '',
	start_ms      integer NOT NULL DEFAULT 0,
	end_ms        integer NOT NULL DEFAULT 0,
	language      text NOT NULL DEFAULT '',
	speaker       text NOT NULL DEFAULT '',
	payload_json  jsonb NOT NULL DEFAULT '{}'::jsonb,
	embedding     vector(%d) NOT NULL
)`, tbl, dim)
}

// CreateHNSWIndexSQL returns the DDL to create the HNSW index on the embedding
// column using cosine distance. The index name is derived from the table name
// so repeated calls are idempotent.
func CreateHNSWIndexSQL(schema, table string) string {
	idxName := quoteIdent(table + "_embedding_hnsw")
	return fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS %s ON %s USING hnsw (embedding vector_cosine_ops)`,
		idxName, qualifiedTable(schema, table),
	)
}

// CreateIdentityTableSQL returns the DDL for the single-row identity table.
func CreateIdentityTableSQL(schema, table string) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	id       boolean PRIMARY KEY DEFAULT true,
	identity text NOT NULL,
	CONSTRAINT %s CHECK (id)
)`, identityTable(schema, table), quoteIdent(table+identityTableSuffix+"_singleton"))
}

// UpsertSQL returns the INSERT ... ON CONFLICT statement and its argument list
// for one vector+payload. Placeholders are $1.. for the scalar columns; the
// embedding is interpolated as a pgvector literal (it is not a plain
// parameteriseable scalar via the simple protocol). The returned args align
// 1:1 with the $N placeholders.
func UpsertSQL(schema, table string, vector []float32, p model.IndexPayload) (string, []any, error) {
	payloadJSON, err := MarshalPayload(p)
	if err != nil {
		return "", nil, err
	}
	tbl := qualifiedTable(schema, table)
	// Scalar columns are parameterised; the vector is interpolated as a literal
	// because the <=> operator and pgvector storage both accept the text form,
	// keeping the query free of a custom type registration.
	sql := fmt.Sprintf(`INSERT INTO %s (chunk_id, rel_path, doc_type, modality, start_ms, end_ms, language, speaker, payload_json, embedding)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, '%s'::vector)
ON CONFLICT (chunk_id) DO UPDATE SET
	rel_path = EXCLUDED.rel_path,
	doc_type = EXCLUDED.doc_type,
	modality = EXCLUDED.modality,
	start_ms = EXCLUDED.start_ms,
	end_ms = EXCLUDED.end_ms,
	language = EXCLUDED.language,
	speaker = EXCLUDED.speaker,
	payload_json = EXCLUDED.payload_json,
	embedding = EXCLUDED.embedding`, tbl, vectorLiteral(vector))
	args := []any{
		int64(p.ChunkID),
		p.RelPath,
		p.DocType,
		p.Modality,
		p.StartMS,
		p.EndMS,
		p.Language,
		p.Speaker,
		payloadJSON,
	}
	return sql, args, nil
}

// DeleteSQL returns the statement and args to delete rows by chunk ID.
func DeleteSQL(schema, table string, chunkIDs []uint64) (string, []any) {
	ids := make([]int64, len(chunkIDs))
	for i, id := range chunkIDs {
		ids[i] = int64(id)
	}
	sql := fmt.Sprintf(`DELETE FROM %s WHERE chunk_id = ANY($1)`, qualifiedTable(schema, table))
	return sql, []any{ids}
}

// BuildFilterPredicates translates the SQL-pushable predicates of a
// model.Filter into WHERE clauses plus their positional args, starting
// placeholders at startArg ($startArg, $startArg+1, ...). It returns the
// individual predicate clauses (to be AND-joined by the caller) and the args.
//
// PathGlob is intentionally NOT translated here: it has no faithful SQL
// equivalent (path.Match semantics differ from LIKE), so CanFilter reports
// false when it is set and retrieval falls back to Go-side filtering. The
// caller (SearchSQL) only invokes this for the pushable subset.
func BuildFilterPredicates(f model.Filter, startArg int) ([]string, []any) {
	var (
		clauses []string
		args    []any
	)
	arg := startArg
	next := func(v any) string {
		args = append(args, v)
		ph := "$" + strconv.Itoa(arg)
		arg++
		return ph
	}

	if f.ExcludeOrphans {
		// Reject empty / whitespace-only rel_path, mirroring Filter.Match.
		clauses = append(clauses, "btrim(rel_path) <> ''")
	}
	if f.PathPrefix != "" {
		// rel_path LIKE prefix || '%' — escape LIKE metacharacters in the
		// prefix so a literal % or _ in a path is matched verbatim.
		ph := next(escapeLikePrefix(f.PathPrefix) + "%")
		clauses = append(clauses, fmt.Sprintf("rel_path LIKE %s ESCAPE '\\'", ph))
	}
	if len(f.DocTypes) > 0 {
		// Case-insensitive set membership: lower(doc_type) = ANY(lowered set).
		lowered := make([]string, 0, len(f.DocTypes))
		for _, dt := range f.DocTypes {
			lowered = append(lowered, strings.ToLower(strings.TrimSpace(dt)))
		}
		ph := next(lowered)
		clauses = append(clauses, fmt.Sprintf("lower(doc_type) = ANY(%s)", ph))
	}
	return clauses, args
}

// escapeLikePrefix escapes the LIKE metacharacters (backslash, percent,
// underscore) in a literal prefix so it matches verbatim under
// "LIKE ... ESCAPE '\'".
func escapeLikePrefix(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// SearchSQL returns the KNN query and its args. The pushable filter predicates
// (see BuildFilterPredicates) are AND-joined into a WHERE clause; results are
// ordered by cosine distance (embedding <=> query) ascending and limited to k.
// The query vector is interpolated as a pgvector literal. Score is computed as
// 1 - distance (cosine similarity) in SQL so callers receive higher-is-better
// values matching the in-memory backend.
func SearchSQL(schema, table string, vector []float32, k int, f model.Filter) (string, []any) {
	clauses, args := BuildFilterPredicates(f, 1)
	where := ""
	if len(clauses) > 0 {
		where = "\nWHERE " + strings.Join(clauses, " AND ")
	}
	limitPH := "$" + strconv.Itoa(len(args)+1)
	args = append(args, k)
	q := vectorLiteral(vector)
	sql := fmt.Sprintf(`SELECT chunk_id, rel_path, doc_type, modality, start_ms, end_ms, language, speaker, payload_json,
	1 - (embedding <=> '%s'::vector) AS score
FROM %s%s
ORDER BY embedding <=> '%s'::vector
LIMIT %s`, q, qualifiedTable(schema, table), where, q, limitPH)
	return sql, args
}

// CanFilterFilter reports whether every active predicate of the filter is
// expressible in SQL. PathGlob, Speaker, and Languages are the unsupported
// predicates; when any is set the backend cannot push down and retrieval must
// filter in Go. Speaker (diarized transcript filter, SPEC §8.6.8) and Languages
// (per-language retrieval filter, SPEC §9.5) are intentionally left to the
// Go-side matchFilters re-check so they stay on the single, authoritative
// model.Filter.Match path (the BCP-47 primary-subtag matching lives there).
// This is the pure decision function behind the FilteringIndex.CanFilter method.
func CanFilterFilter(f model.Filter) bool {
	return f.PathGlob == "" && strings.TrimSpace(f.Speaker) == "" && len(f.Languages) == 0
}

// MarshalPayload serialises the full IndexPayload (including the nested Span)
// to JSON for the payload_json column, so retrieval can reconstruct every field
// even though only a subset has dedicated columns.
func MarshalPayload(p model.IndexPayload) ([]byte, error) {
	return json.Marshal(p)
}

// UnmarshalPayload reconstructs a full IndexPayload from the payload_json blob.
func UnmarshalPayload(data []byte) (model.IndexPayload, error) {
	var p model.IndexPayload
	if len(data) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return model.IndexPayload{}, err
	}
	return p, nil
}

// RowToHit reconstructs a model.IndexHit from a scanned row. The full payload
// is taken from payloadJSON; the scalar columns are authoritative overrides for
// the queryable fields (so a manual SQL edit to a column is reflected) and
// chunkID/score come from the row directly.
func RowToHit(chunkID int64, score float64, payloadJSON []byte, rel, docType, modality, language, speaker string, startMS, endMS int) (model.IndexHit, error) {
	p, err := UnmarshalPayload(payloadJSON)
	if err != nil {
		return model.IndexHit{}, err
	}
	id := uint64(chunkID)
	p.ChunkID = id
	p.RelPath = rel
	p.DocType = docType
	p.Modality = modality
	p.Language = language
	p.Speaker = speaker
	p.StartMS = startMS
	p.EndMS = endMS
	return model.IndexHit{ChunkID: id, Score: float32(score), Payload: p}, nil
}

package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/dirstral/dir2mcp/internal/model"
)

// routeFallbackMeta is the part of a transcript meta_json the §8.2.4 records
// read.
type routeFallbackMeta struct {
	RouteFallbackFrom    string            `json:"route_fallback_from"`
	RouteFallbackAt      string            `json:"route_fallback_at"`
	RouteFallbackBy      string            `json:"route_fallback_by"`
	RouteFallbackDropped []json.RawMessage `json:"route_fallback_dropped"`
	DroppedCached        bool              `json:"route_fallback_dropped_cached"`
	SkipReason           string            `json:"route_fallback_skip_reason"`
	Coverage             *struct {
		Languages []struct {
			FallbackFrom string `json:"fallback_from"`
		} `json:"languages"`
	} `json:"coverage"`
}

// routeFallbackQuery lists the live source and per-track transcripts whose
// meta_json may hold a fallback record. The whole transcript rep_type family
// is walked, as the §7.7 coverage report does, because an additional audio
// track (§8.6.12) is a separate decode with its own fallback record. The LIKE
// predicate is a cheap pre-filter (a route_fallback_dropped entry carries
// fallback_from too); the JSON decode decides.
const routeFallbackQuery = `
		SELECT d.rel_path, r.rep_type, r.meta_json
		FROM representations r
		JOIN documents d ON d.doc_id = r.doc_id
		WHERE r.deleted = 0 AND d.deleted = 0
		  AND (r.rep_type = 'transcript'
		       OR r.rep_type LIKE 'transcript@%'
		       OR r.rep_type LIKE 'transcript-%')
		  AND r.meta_json LIKE '%fallback_from%'`

// RouteFallbackTranscripts lists the live transcripts that hold a SPEC §8.2.4
// fallback result (spec 0.82.0, dir2mcp #1122). Ingest reads it once per scan
// for the re-derivation gate.
func (s *SQLiteStore) RouteFallbackTranscripts(ctx context.Context) ([]model.RouteFallbackRecord, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()
	return routeFallbackTranscripts(ctx, db, "")
}

// RouteFallbackTranscriptsFor is RouteFallbackTranscripts for one document,
// for a document processed outside a scan.
func (s *SQLiteStore) RouteFallbackTranscriptsFor(ctx context.Context, relPath string) ([]model.RouteFallbackRecord, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()
	return routeFallbackTranscripts(ctx, db, relPath)
}

// ReadRouteFallbackTranscripts lists the fallback records of the index at
// path through a read-only (query_only) connection, for `dir2mcp doctor`. It
// does not initialise, migrate or write the database, so a diagnostic never
// changes or locks a live index.
func ReadRouteFallbackTranscripts(ctx context.Context, path string) ([]model.RouteFallbackRecord, error) {
	db, err := sql.Open("sqlite", sqliteReadOnlyDSN(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	return routeFallbackTranscripts(ctx, db, "")
}

func routeFallbackTranscripts(ctx context.Context, db *sql.DB, relPath string) ([]model.RouteFallbackRecord, error) {
	query, args := routeFallbackQuery+` ORDER BY d.rel_path, r.rep_type`, []any{}
	if relPath != "" {
		query, args = routeFallbackQuery+` AND d.rel_path = ? ORDER BY r.rep_type`, []any{relPath}
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.RouteFallbackRecord
	for rows.Next() {
		var path, repType, raw string
		if err := rows.Scan(&path, &repType, &raw); err != nil {
			return nil, err
		}
		var meta routeFallbackMeta
		if err := json.Unmarshal([]byte(raw), &meta); err != nil || !meta.holdsFallback() {
			continue
		}
		dropped := len(meta.RouteFallbackDropped) > 0
		out = append(out, model.RouteFallbackRecord{RelPath: path, RepType: repType, At: meta.RouteFallbackAt, By: meta.RouteFallbackBy,
			Dropped: dropped, DroppedCached: dropped && meta.DroppedCached, SkipReason: meta.SkipReason})
	}
	return out, rows.Err()
}

// holdsFallback reports whether the meta records a fallback result under
// either scope, or a fallback window dropped under on_route_error: fail.
func (m routeFallbackMeta) holdsFallback() bool {
	if m.RouteFallbackFrom != "" || len(m.RouteFallbackDropped) > 0 {
		return true
	}
	if m.Coverage == nil {
		return false
	}
	for _, l := range m.Coverage.Languages {
		if l.FallbackFrom != "" {
			return true
		}
	}
	return false
}

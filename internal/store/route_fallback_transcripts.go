package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/dirstral/dir2mcp/internal/model"
)

// routeFallbackMeta is the part of a transcript meta_json the §8.2.4 report
// reads.
type routeFallbackMeta struct {
	RouteFallbackFrom string `json:"route_fallback_from"`
	RouteFallbackAt   string `json:"route_fallback_at"`
	RouteFallbackBy   string `json:"route_fallback_by"`
	Coverage          *struct {
		Languages []struct {
			FallbackFrom string `json:"fallback_from"`
		} `json:"languages"`
	} `json:"coverage"`
}

// RouteFallbackTranscripts lists the live transcripts that hold a SPEC §8.2.4
// fallback result (spec 0.82.0, dir2mcp #1122), for the `doctor` stt_routes
// check: how many there are and when the next is due.
//
// The whole transcript rep_type family is walked, as the §7.7 coverage report
// does, because an additional audio track (§8.6.12) is a separate decode with
// its own fallback record. The LIKE predicate is a cheap pre-filter; the JSON
// decode decides. A meta_json that does not parse holds no record.
func (s *SQLiteStore) RouteFallbackTranscripts(ctx context.Context) ([]model.RouteFallbackRecord, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()
	return routeFallbackTranscripts(ctx, db)
}

func routeFallbackTranscripts(ctx context.Context, db *sql.DB) ([]model.RouteFallbackRecord, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT d.rel_path, r.rep_type, r.meta_json
		FROM representations r
		JOIN documents d ON d.doc_id = r.doc_id
		WHERE r.deleted = 0 AND d.deleted = 0
		  AND (r.rep_type = 'transcript'
		       OR r.rep_type LIKE 'transcript@%'
		       OR r.rep_type LIKE 'transcript-%')
		  AND r.meta_json LIKE '%fallback_from%'
		ORDER BY d.rel_path, r.rep_type`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.RouteFallbackRecord
	for rows.Next() {
		var relPath, repType, raw string
		if err := rows.Scan(&relPath, &repType, &raw); err != nil {
			return nil, err
		}
		var meta routeFallbackMeta
		if err := json.Unmarshal([]byte(raw), &meta); err != nil || !meta.holdsFallback() {
			continue
		}
		out = append(out, model.RouteFallbackRecord{RelPath: relPath, RepType: repType, At: meta.RouteFallbackAt, By: meta.RouteFallbackBy})
	}
	return out, rows.Err()
}

// holdsFallback reports whether the meta records a fallback result under
// either scope.
func (m routeFallbackMeta) holdsFallback() bool {
	if m.RouteFallbackFrom != "" {
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

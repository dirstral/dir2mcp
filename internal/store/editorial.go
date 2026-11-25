package store

import (
	"context"
	"strings"
)

// EditorialDocument is the view of one document that the editorial import
// apply step (SPEC §7.11.3) resolves keys against: its identity, its type and
// its index state.
type EditorialDocument struct {
	DocID       int64
	RelPath     string
	DocType     string
	Status      string
	ContentHash string
}

// ListEditorialDocuments returns every document that is not deleted, ordered
// by rel_path. A deleted document is not a document any more, so a key that
// names only deleted documents is unmatched.
func (s *SQLiteStore) ListEditorialDocuments(ctx context.Context) ([]EditorialDocument, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()
	rows, err := db.QueryContext(ctx,
		`SELECT doc_id, rel_path, doc_type, status, content_hash
		   FROM documents
		  WHERE deleted = 0
		  ORDER BY rel_path ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []EditorialDocument
	for rows.Next() {
		var d EditorialDocument
		if err := rows.Scan(&d.DocID, &d.RelPath, &d.DocType, &d.Status, &d.ContentHash); err != nil {
			return nil, err
		}
		d.Status = strings.TrimSpace(d.Status)
		out = append(out, d)
	}
	return out, rows.Err()
}

// EditorialRepresentation is the stored state of one editorial representation.
type EditorialRepresentation struct {
	RepID    int64
	DocID    int64
	RepType  string
	RepHash  string
	MetaJSON string
	Deleted  bool
}

// ListEditorialRepresentations returns every editorial representation, active
// or deleted, keyed by (doc_id, rep_type). The apply step reads them once per
// pass to decide which representations must be built again.
func (s *SQLiteStore) ListEditorialRepresentations(ctx context.Context) (map[int64]map[string]EditorialRepresentation, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()
	rows, err := db.QueryContext(ctx,
		`SELECT rep_id, doc_id, rep_type, rep_hash, COALESCE(meta_json, ''), deleted
		   FROM representations
		  WHERE rep_type LIKE 'editorial@%'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]map[string]EditorialRepresentation{}
	for rows.Next() {
		var r EditorialRepresentation
		var deleted int
		if err := rows.Scan(&r.RepID, &r.DocID, &r.RepType, &r.RepHash, &r.MetaJSON, &deleted); err != nil {
			return nil, err
		}
		r.Deleted = deleted != 0
		if out[r.DocID] == nil {
			out[r.DocID] = map[string]EditorialRepresentation{}
		}
		out[r.DocID][r.RepType] = r
	}
	return out, rows.Err()
}

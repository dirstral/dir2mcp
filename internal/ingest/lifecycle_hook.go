package ingest

import "github.com/dirstral/dir2mcp/internal/model"

// observeDocumentTransition records one documents.status write against
// model.DocumentLifecycle. It only logs a write the table does not list; it
// never refuses one. It is called only where the previous row is already in
// hand (the store's UpsertDocument is a blind upsert and cannot see it), so it
// adds no database read. previous is the zero Document when no row existed.
func (s *Service) observeDocumentTransition(previous model.Document, written model.Document) {
	from := model.DocumentStatus("")
	if previous.RelPath != "" {
		// Normalize the stored value too: a row that an older version wrote can
		// hold a non-canonical status, which is not a table key.
		from = model.NormalizeDocumentStatus(previous.Status)
	}
	to := model.NormalizeDocumentStatus(written.Status)
	model.DocumentLifecycle.Observe(s.getLogger().Printf, written.RelPath, from, to)
}

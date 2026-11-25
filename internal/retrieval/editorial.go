package retrieval

import (
	"strings"

	"github.com/dirstral/dir2mcp/internal/editorial"
	"github.com/dirstral/dir2mcp/internal/model"
)

// SetEditorialNameFolds sets the `annotations.name_folds` list (SPEC §7.11.4)
// the entities filter applies to a requested value before it compares the
// value with the person ids of an `editorial` hit (§9.9). The list was
// validated with the config; an invalid list is returned as an error and
// leaves the previous folds in place.
func (s *Service) SetEditorialNameFolds(list []string) error {
	folds, err := editorial.ParseFolds(list)
	if err != nil {
		return err
	}
	s.metaMu.Lock()
	s.editorialFolds = folds
	s.metaMu.Unlock()
	return nil
}

// withEditorialEntities fills query.EditorialEntities: the requested entity
// values after the person id normalization with the configured folds. It is
// applied once at the entry of a search, so every candidate check of that
// search compares the same normalized values.
func (s *Service) withEditorialEntities(query model.SearchQuery) model.SearchQuery {
	if len(query.Entities) == 0 || query.EditorialEntities != nil {
		return query
	}
	s.metaMu.RLock()
	folds := s.editorialFolds
	s.metaMu.RUnlock()
	query.EditorialEntities = model.EditorialEntityIDs(query.Entities, folds)
	return query
}

// FormatHitCitation is FormatCitation for a hit of the given rep_type. An
// `editorial` hit renders as an editor annotation from its source (SPEC §9.3):
// the base form of the span, then " › editor annotation (<source>, match
// <score>)". The match part is left out when the span has no match_score. Any
// other hit renders exactly as FormatCitation.
func FormatHitCitation(relPath, repType string, span model.Span) string {
	base := FormatCitation(relPath, span)
	if !editorial.IsRepType(strings.TrimSpace(repType)) {
		return base
	}
	source := ""
	if sources := model.NormalizeSources(span.Sources); len(sources) > 0 {
		source = sources[0]
	}
	label := editorial.CitationLabel(source, span.Attributes["match_score"])
	return strings.TrimSuffix(base, "]") + " › " + label + "]"
}

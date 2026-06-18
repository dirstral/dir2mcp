package qdrantindex

import (
	"strings"

	"github.com/qdrant/go-client/qdrant"

	"github.com/dirstral/dir2mcp/internal/model"
)

// canFilter reports whether the Qdrant backend can evaluate the whole filter
// itself (issue #247 FilteringIndex contract). The contract is exact: when
// CanFilter is true, retrieval trusts the backend-filtered results without a
// Go-side re-check, so we may only return true for predicates we can translate
// to an *equivalent* Qdrant filter.
//
//   - DocTypes (case-insensitive set membership) and ExcludeOrphans (empty
//     rel_path drop) map exactly to Qdrant keyword match / is-empty conditions.
//   - PathPrefix has no exact native equivalent: Qdrant's keyword match is
//     whole-value and its full-text match is token/substring, neither of which
//     is a faithful string-prefix. We therefore decline push-down so retrieval
//     applies model.Filter.Match in Go.
//   - PathGlob (path.Match) likewise has no native equivalent.
//
// Declining is always safe — retrieval falls back to overfetch-then-filter — so
// correctness is preserved at the cost of a wider candidate pull for the two
// path predicates.
func canFilter(filter model.Filter) bool {
	if filter.IsZero() {
		return true
	}
	if filter.PathPrefix != "" || filter.PathGlob != "" {
		return false
	}
	// Speaker (diarized transcript filter, SPEC §8.6.8) is evaluated by the
	// Go-side matchFilters re-check, mirroring the path predicates: decline
	// push-down so retrieval applies it after materialisation. Declining is
	// always safe (overfetch-then-filter), and it keeps speaker filtering on the
	// single, authoritative model.Filter.Match path.
	if strings.TrimSpace(filter.Speaker) != "" {
		return false
	}
	// Languages (per-language retrieval filter, SPEC §9.5) is likewise evaluated
	// by the Go-side matchFilters re-check: BCP-47 primary-subtag matching has no
	// faithful native Qdrant equivalent (keyword match is whole-value, so "en"
	// would miss "en-US"). Decline push-down so it stays on the single,
	// authoritative model.Filter.Match path. Declining is always safe
	// (overfetch-then-filter).
	if len(filter.Languages) != 0 {
		return false
	}
	return true
}

// toQdrantFilter translates the pushable predicates of a model.Filter into a
// Qdrant *Filter, or nil when there is nothing to push down. It only emits
// conditions for predicates canFilter accepts; the path predicates are left to
// the Go-side fallback and are intentionally ignored here. Returning nil for an
// all-empty (or path-only) filter lets the caller omit the filter field
// entirely.
//
// DocTypes are matched case-insensitively to mirror model.Filter.Match: each
// point stores a lower-cased doc_type_lc copy and we lower-case the queried doc
// types, so a Qdrant keyword match-any over doc_type_lc is exactly equivalent
// to model.Filter's case-insensitive set membership.
func toQdrantFilter(filter model.Filter) *qdrant.Filter {
	if !canFilter(filter) || filter.IsZero() {
		return nil
	}

	var must []*qdrant.Condition

	if len(filter.DocTypes) > 0 {
		docTypes := normalizeDocTypes(filter.DocTypes)
		if len(docTypes) > 0 {
			must = append(must, qdrant.NewMatchKeywords(fieldDocTypeLC, docTypes...))
		}
	}

	if len(must) == 0 && !filter.ExcludeOrphans {
		return nil
	}

	qf := &qdrant.Filter{Must: must}
	if filter.ExcludeOrphans {
		// An orphaned/evicted chunk has an empty rel_path; drop it by excluding
		// points whose rel_path is empty.
		qf.MustNot = append(qf.MustNot, qdrant.NewIsEmpty(fieldRelPath))
	}
	return qf
}

// normalizeDocTypes trims, lower-cases, and drops empty doc-type tokens,
// preserving order and de-duplicating, so the emitted keyword match-any over
// doc_type_lc mirrors model.Filter.Match's case-insensitive membership set.
func normalizeDocTypes(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, dt := range in {
		t := strings.ToLower(strings.TrimSpace(dt))
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

package mcp

import (
	"sort"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// failedChunksForStats builds the SPEC §15.6 `indexing.failed_chunks` object:
// the STANDING count of chunks sitting in a terminal embed-failure state
// across the whole corpus, from ANY run.
//
// It exists because `errors` cannot answer the question an operator actually
// asks. `errors` counts what the CURRENT run observed, so it returns to 0 on
// the next run while previously-failed chunks stay failed — and a failed chunk
// is absent from BOTH retrieval paths and raises nothing at query time. That
// combination lost a quarter of the pilot corpus in silence: 406 chunks marked
// failed by one exhausted quota, `errors: 0`, an entire topic gone from search
// and no surface saying so (#932/#939).
//
// The store already had the number. model.FailureSummary is explicitly "the
// chunks that are CURRENTLY in a failed state, not the failures observed
// during the run" (#783) — only the MCP surface never carried it.
//
// derivable says whether the corpus-stats path actually ran. It is the whole
// difference between "zero failed chunks" and "this server cannot tell you",
// which SPEC §15.6 requires a client to distinguish: when the counts ARE
// derivable the object is emitted even at zero (silence must never be the way
// to say zero, since that is the exact misreading this field prevents), and
// when they are not it is omitted so absence reads as unknown.
func failedChunksForStats(summary *model.FailureSummary, derivable bool) map[string]interface{} {
	if !derivable {
		return nil
	}
	byCategory := make([]map[string]interface{}, 0, len(summaryCategories(summary)))
	var total, retryable int64
	for _, entry := range summaryCategories(summary) {
		// A zero-count category is omitted rather than emitted as 0, so an
		// intact corpus carries an empty array instead of a list of zeros.
		if entry.count <= 0 {
			continue
		}
		canRetry := store.IsRequeueableCategory(entry.category)
		total += entry.count
		if canRetry {
			retryable += entry.count
		}
		byCategory = append(byCategory, map[string]interface{}{
			"category": entry.category,
			"count":    entry.count,
			// Stated per category by the SERVER so a client never hard-codes a
			// category-to-retryable mapping: which failures a bare retry can
			// clear is implementation policy (store.requeueableCategories) and
			// may change, and a client that had copied it would mis-advise an
			// operator after an upgrade.
			"retryable": canRetry,
		})
	}
	// total and retryable are summed from the same entries that are emitted,
	// never counted separately, so the §15.6 invariants (by_category sums to
	// total; retryable is the sum of the retryable entries; retryable <= total)
	// hold by construction rather than by assertion.
	return map[string]interface{}{
		"total":       total,
		"retryable":   retryable,
		"by_category": byCategory,
	}
}

type failedCategoryEntry struct {
	category string
	count    int64
}

// summaryCategories flattens the summary's category map into a deterministic
// order: commonest first, ties broken by name. Map iteration order would make
// the payload differ between two identical corpora, which turns a diff of two
// stats snapshots into noise.
func summaryCategories(summary *model.FailureSummary) []failedCategoryEntry {
	if summary == nil || len(summary.Categories) == 0 {
		return nil
	}
	out := make([]failedCategoryEntry, 0, len(summary.Categories))
	for category, count := range summary.Categories {
		out = append(out, failedCategoryEntry{category: category, count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].category < out[j].category
	})
	return out
}

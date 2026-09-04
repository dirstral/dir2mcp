package tests

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
)

// SPEC §15.6 (spec 0.60.0) `indexing.failed_chunks`, from dir2mcp #932/#939.
//
// The failure it exists for, measured: a provider quota ran out mid-run, 406
// chunks were marked terminally failed, and `dir2mcp_stats` answered
// `errors: 0`. Both were true — `errors` counts what the CURRENT run saw, and
// the run that did the damage had ended. A failed chunk is absent from BOTH
// retrieval paths and raises nothing at query time, so an entire topic had
// vanished from search while every surface looked healthy.

// failedChunksRetriever is a Retriever whose Stats carries a chosen
// FailureSummary, which is the corpus-standing aggregate the store already
// computes (model.FailureSummary: "chunks that are CURRENTLY in a failed
// state, not the failures observed during the run", #783).
type failedChunksRetriever struct {
	summary *model.FailureSummary
	err     error
}

func (r *failedChunksRetriever) Search(context.Context, model.SearchQuery) ([]model.SearchHit, error) {
	return nil, nil
}

func (r *failedChunksRetriever) Ask(context.Context, string, model.SearchQuery) (model.AskResult, error) {
	return model.AskResult{}, nil
}

func (r *failedChunksRetriever) OpenFile(context.Context, string, model.Span, int) (string, error) {
	return "", nil
}

func (r *failedChunksRetriever) IndexingComplete(context.Context) (bool, error) { return true, nil }

func (r *failedChunksRetriever) Stats(context.Context) (model.Stats, error) {
	if r.err != nil {
		return model.Stats{}, r.err
	}
	return model.Stats{
		CorpusStats: model.CorpusStats{
			DocCounts:      map[string]int64{"video": 1},
			ChunksTotal:    1570,
			EmbeddedOK:     1164,
			FailureSummary: r.summary,
		},
	}, nil
}

// statsIndexingWith calls dir2mcp_stats against a server backed by the given
// retriever and returns the structured `indexing` object.
func statsIndexingWith(t *testing.T, retriever model.Retriever) map[string]interface{} {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.MCPPath = protocol.DefaultMCPPath
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sc := callStatsTool(t, server.URL+cfg.MCPPath)
	indexing, ok := sc["indexing"].(map[string]interface{})
	if !ok {
		t.Fatalf("indexing object missing: %#v", sc)
	}
	return indexing
}

func failedChunksOf(t *testing.T, indexing map[string]interface{}) map[string]interface{} {
	t.Helper()
	raw, present := indexing["failed_chunks"]
	if !present {
		t.Fatalf("failed_chunks missing: indexing=%#v", indexing)
	}
	obj, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("failed_chunks is not an object: %#v", raw)
	}
	return obj
}

// The headline case, in the pilot's own shape: a standing 406 while the
// current run reports zero errors. Both numbers appear side by side, which is
// the entire point — the operator can see that "this run was clean" and "the
// corpus is missing 406 chunks" are both true.
func TestStatsSurfacesStandingFailures_939(t *testing.T) {
	indexing := statsIndexingWith(t, &failedChunksRetriever{summary: &model.FailureSummary{
		Categories: map[string]int64{"rate_limit": 406},
	}})

	if got := indexing["errors"]; got != float64(0) {
		t.Fatalf("errors = %#v, want 0 (this run saw nothing)", got)
	}
	fc := failedChunksOf(t, indexing)
	if fc["total"] != float64(406) {
		t.Fatalf("total = %#v, want 406", fc["total"])
	}
	if fc["retryable"] != float64(406) {
		t.Fatalf("retryable = %#v, want 406 (a rate limit clears on retry)", fc["retryable"])
	}
	entries, ok := fc["by_category"].([]interface{})
	if !ok || len(entries) != 1 {
		t.Fatalf("by_category = %#v, want one entry", fc["by_category"])
	}
	entry := entries[0].(map[string]interface{})
	if entry["category"] != "rate_limit" || entry["count"] != float64(406) || entry["retryable"] != true {
		t.Fatalf("entry = %#v, want rate_limit/406/retryable", entry)
	}
}

// Retryability is stated PER CATEGORY by the server, so a client never
// hard-codes a mapping that is implementation policy. parse_error is a
// property of the stored bytes: re-sending them re-fails, so it is not
// retryable and must not inflate the retryable count.
func TestRetryableCountExcludesTerminalCategories_939(t *testing.T) {
	fc := failedChunksOf(t, statsIndexingWith(t, &failedChunksRetriever{summary: &model.FailureSummary{
		Categories: map[string]int64{"rate_limit": 10, "parse_error": 5, "auth": 2},
	}}))

	if fc["total"] != float64(17) {
		t.Fatalf("total = %#v, want 17", fc["total"])
	}
	// rate_limit + auth are provider/environment faults; parse_error is not.
	if fc["retryable"] != float64(12) {
		t.Fatalf("retryable = %#v, want 12 (17 minus the 5 parse_error)", fc["retryable"])
	}
	for _, raw := range fc["by_category"].([]interface{}) {
		entry := raw.(map[string]interface{})
		if entry["category"] == "parse_error" && entry["retryable"] != false {
			t.Fatalf("parse_error must be reported non-retryable: %#v", entry)
		}
	}
}

// The §15.6 invariants, checked on the payload itself rather than assumed:
// by_category sums to total, retryable is the sum of the retryable entries,
// and therefore retryable <= total. Draft-07 cannot express a comparison
// between sibling fields, so this is the executable half promised in the spec
// review — the schema cannot catch a producer that contradicts itself.
func TestFailedChunkCountsAreSelfConsistent_939(t *testing.T) {
	fc := failedChunksOf(t, statsIndexingWith(t, &failedChunksRetriever{summary: &model.FailureSummary{
		Categories: map[string]int64{"rate_limit": 9, "transient_net": 4, "embedding_failure": 3, "unknown": 1},
	}}))

	var sum, retrySum float64
	for _, raw := range fc["by_category"].([]interface{}) {
		entry := raw.(map[string]interface{})
		count := entry["count"].(float64)
		if count < 1 {
			t.Fatalf("a zero-count category must be omitted: %#v", entry)
		}
		sum += count
		if entry["retryable"] == true {
			retrySum += count
		}
	}
	total := fc["total"].(float64)
	retryable := fc["retryable"].(float64)
	if sum != total {
		t.Fatalf("by_category sums to %v, total says %v", sum, total)
	}
	if retrySum != retryable {
		t.Fatalf("retryable entries sum to %v, retryable says %v", retrySum, retryable)
	}
	if retryable > total {
		t.Fatalf("retryable %v exceeds total %v", retryable, total)
	}
}

// An intact corpus STATES zero rather than staying silent. "Zero failed
// chunks" and "this server does not report failed chunks" are different
// facts, and reading the second as the first is the exact misreading the
// field exists to prevent — so silence must never be how zero is said.
func TestIntactCorpusStatesZeroRatherThanOmitting_939(t *testing.T) {
	// A store with no failures returns a nil summary (loadFailureSummary
	// returns nil when there are no categories), which must still be reported
	// as a derived zero because the corpus-stats path DID run.
	fc := failedChunksOf(t, statsIndexingWith(t, &failedChunksRetriever{summary: nil}))
	if fc["total"] != float64(0) || fc["retryable"] != float64(0) {
		t.Fatalf("want an explicit zero, got %#v", fc)
	}
	entries, ok := fc["by_category"].([]interface{})
	if !ok || len(entries) != 0 {
		t.Fatalf("by_category = %#v, want an empty array rather than a list of zeros", fc["by_category"])
	}
}

// When the counts are not derivable the field is OMITTED, so absence reads as
// "unknown" rather than as a healthy corpus. This is the ListFiles-only
// fallback path: no retriever stats, so nothing counted the chunks.
func TestOmittedWhenNotDerivable_939(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.MCPPath = protocol.DefaultMCPPath
	cfg.AuthMode = "none"
	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	sc := callStatsTool(t, server.URL+cfg.MCPPath)
	indexing := sc["indexing"].(map[string]interface{})
	if _, present := indexing["failed_chunks"]; present {
		t.Fatalf("failed_chunks must be omitted when not derivable: %#v", indexing)
	}
}

// Map iteration order must not reach the wire: two identical corpora have to
// produce byte-identical payloads, or a diff of two snapshots is noise.
// Commonest first, ties broken by name.
func TestByCategoryOrderIsDeterministic_939(t *testing.T) {
	var first []interface{}
	for i := 0; i < 25; i++ {
		fc := failedChunksOf(t, statsIndexingWith(t, &failedChunksRetriever{summary: &model.FailureSummary{
			Categories: map[string]int64{"auth": 5, "rate_limit": 5, "transient_net": 9, "unknown": 1},
		}}))
		entries := fc["by_category"].([]interface{})
		if first == nil {
			first = entries
			continue
		}
		for j := range entries {
			a := entries[j].(map[string]interface{})
			b := first[j].(map[string]interface{})
			if a["category"] != b["category"] {
				t.Fatalf("run %d position %d: %v != %v (order flapped)", i, j, a["category"], b["category"])
			}
		}
	}
	names := make([]string, 0, len(first))
	for _, raw := range first {
		names = append(names, raw.(map[string]interface{})["category"].(string))
	}
	want := []string{"transient_net", "auth", "rate_limit", "unknown"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("order = %v, want %v (count desc, then name)", names, want)
		}
	}
}

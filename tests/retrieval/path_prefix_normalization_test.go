package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// TestSearch_PathPrefixNormalization is the regression test for issue #286
// Bug B. list_files normalizes the path_prefix (strips leading "./" and "/",
// path.Clean, slash) before its store LIKE query, so "./acts", "/acts" and
// "acts/" all list acts/foo.pdf. But search/ask applied the RAW prefix via a
// case-sensitive strings.HasPrefix, so the very prefixes that worked in
// list_files silently matched nothing in search.
//
// After the fix both call sites share one normalizer, so a prefix that lists a
// file in list_files also matches it in search/ask.
func TestSearch_PathPrefixNormalization(t *testing.T) {
	newSvc := func() *retrieval.Service {
		idx := index.NewHNSWIndex("")
		// addVecP sets the payload rel_path so the FilteringIndex evaluates the
		// pushed-down path_prefix predicate (model.Filter.Match), exercising the
		// same normalization the store applies in list_files.
		addVecP(t, idx, 1, []float32{1, 0}, "acts/foo.pdf", "pdf")
		svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
			"mistral-embed":   {1, 0},
			"codestral-embed": {0, 1},
		}}, nil)
		svc.SetChunkMetadata(1, model.SearchHit{RelPath: "acts/foo.pdf", DocType: "pdf", Snippet: "alpha"})
		return svc
	}

	// These all list acts/foo.pdf in list_files (store LIKE prefix%, which is
	// case-insensitive for ASCII and matches across sub-segments); search/ask
	// must agree. "act" is included because the store's LIKE matches it.
	matchingPrefixes := []string{"acts", "acts/", "./acts", "/acts", "acts/foo.pdf", "ACTS", "act"}
	for _, prefix := range matchingPrefixes {
		t.Run("match/"+prefix, func(t *testing.T) {
			svc := newSvc()
			hits, err := svc.Search(context.Background(), model.SearchQuery{
				Query:      "alpha",
				K:          5,
				PathPrefix: prefix,
			})
			if err != nil {
				t.Fatalf("Search failed: %v", err)
			}
			if len(hits) != 1 || hits[0].RelPath != "acts/foo.pdf" {
				t.Fatalf("prefix %q should match acts/foo.pdf; got %#v", prefix, hits)
			}
		})
	}

	// A prefix that does not match must return nothing (no over-matching).
	nonMatchingPrefixes := []string{"xyz", "other/", "acts/foo.pdf/extra"}
	for _, prefix := range nonMatchingPrefixes {
		t.Run("nomatch/"+prefix, func(t *testing.T) {
			svc := newSvc()
			hits, err := svc.Search(context.Background(), model.SearchQuery{
				Query:      "alpha",
				K:          5,
				PathPrefix: prefix,
			})
			if err != nil {
				t.Fatalf("Search failed: %v", err)
			}
			if len(hits) != 0 {
				t.Fatalf("prefix %q should match nothing; got %#v", prefix, hits)
			}
		})
	}
}

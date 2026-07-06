package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// TestSearchWithAxis_AutoRoutesCodeQueryToCode covers the SPEC §15.2 requirement
// that index_used reflects the index a query is ACTUALLY routed to. Under the
// default "auto" mode a code-shaped query routes to the code index, so
// SearchWithAxis — the source of the tool layer's index_used — must report
// "code" rather than the requested-name default of "text". Because it reads the
// axis back from the real dispatch, the reported value can never diverge from
// the index actually searched.
func TestSearchWithAxis_AutoRoutesCodeQueryToCode(t *testing.T) {
	idx := &fakeRetrievalIndex{lastK: -1}
	emb := &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{"mistral-embed": {1, 0}}}
	svc := retrieval.NewService(nil, idx, emb, nil)

	cases := []struct {
		name  string
		query model.SearchQuery
		want  string
	}{
		{
			name:  "auto+code-shaped query resolves to code",
			query: model.SearchQuery{Index: "auto", Query: "func handleSearch() {"},
			want:  "code",
		},
		{
			name:  "empty index defaults to auto, prose resolves to text",
			query: model.SearchQuery{Index: "", Query: "when did the meeting happen"},
			want:  "text",
		},
		{
			name:  "explicit text is honored",
			query: model.SearchQuery{Index: "text", Query: "func handleSearch() {"},
			want:  "text",
		},
		{
			name:  "explicit code is honored",
			query: model.SearchQuery{Index: "code", Query: "when did the meeting happen"},
			want:  "code",
		},
		{
			name:  "explicit both is honored",
			query: model.SearchQuery{Index: "both", Query: "anything"},
			want:  "both",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got, err := svc.SearchWithAxis(context.Background(), tc.query)
			if err != nil {
				t.Fatalf("SearchWithAxis(%+v): %v", tc.query, err)
			}
			if got != tc.want {
				t.Fatalf("SearchWithAxis(%+v) axis = %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}

// TestSearchWithAxis_HyDEReplace_ReportsHypothesisAxis pins the divergence fix:
// in HyDE "replace" mode retrieval dispatches on the generated hypothesis, not
// the original query. When the original query is prose (routes "text") but the
// hypothesis is code-shaped (routes "code"), SearchWithAxis must report the axis
// ACTUALLY searched ("code"). Re-deriving index_used from the original query
// would wrongly report "text".
func TestSearchWithAxis_HyDEReplace_ReportsHypothesisAxis(t *testing.T) {
	// The hypothesis is code-shaped (keyword + punctuation ⇒ routes "code") and
	// carries the "hyde-doc" marker so the embedder still routes it to chunk 2.
	gen := &recordingGenerator{out: "func handleSearch() { return hyde-doc }"}
	svc := newHyDEService(t, gen)
	svc.SetHyDE(true, "replace")

	// Original query is prose ⇒ resolves to "text" on its own.
	query := model.SearchQuery{Index: "auto", Query: "when did the meeting happen", K: 10}
	_, axis, err := svc.SearchWithAxis(context.Background(), query)
	if err != nil {
		t.Fatalf("SearchWithAxis: %v", err)
	}
	if gen.calls != 1 {
		t.Fatalf("HyDE replace must call the generator once; calls=%d", gen.calls)
	}
	if axis != "code" {
		t.Fatalf("index_used = %q, want \"code\" (HyDE replace routes on the hypothesis, not the original query)", axis)
	}
}

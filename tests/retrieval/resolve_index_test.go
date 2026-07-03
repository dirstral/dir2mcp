package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// TestResolveIndex_AutoRoutesCodeQueryToCode covers the SPEC §15.2 requirement
// that index_used reflects the index a query is ACTUALLY routed to. Under the
// default "auto" mode a code-shaped query routes to the code index, so
// ResolveIndex — the resolver behind the tool layer's index_used — must report
// "code" rather than the requested-name default of "text".
func TestResolveIndex_AutoRoutesCodeQueryToCode(t *testing.T) {
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
			if got := svc.ResolveIndex(tc.query); got != tc.want {
				t.Fatalf("ResolveIndex(%+v) = %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}

package retrieval

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// An out-of-range editor annotation tag is clamped to the editorial block, not
// to a transcript block with the same span, so the rewrite keeps its
// editorial attribution (SPEC §9.3).
func TestClampCitationTagSpans_KeepsEditorialProvenance(t *testing.T) {
	span := model.Span{Kind: "time", StartMS: 10000, EndMS: 20000}
	editorialSpan := span
	editorialSpan.Sources = []string{"example-catalogue"}
	editorialSpan.Attributes = map[string]string{"match_score": "1.25"}
	citations := []model.Citation{
		{RelPath: "news.mp4", Span: span, RepType: "transcript"},
		{RelPath: "news.mp4", Span: editorialSpan, RepType: "editorial"},
	}
	answer := "Claim [news.mp4@t=00:10-00:21 › editor annotation (example-catalogue, match 1.25)]."
	out, n := clampCitationTagSpans(answer, citations)
	if n != 1 {
		t.Fatalf("clamped %d tags, want 1: %s", n, out)
	}
	if !strings.Contains(out, "[news.mp4@t=00:10-00:20 › editor annotation (example-catalogue, match 1.25)]") {
		t.Fatalf("the rewrite lost the editorial attribution: %s", out)
	}

	plain := "Claim [news.mp4@t=00:10-00:21]."
	out, _ = clampCitationTagSpans(plain, citations)
	if !strings.Contains(out, "[news.mp4@t=00:10-00:20]") {
		t.Fatalf("a transcript tag was rewritten as an editor annotation: %s", out)
	}
}

// With two editorial sources on one span, the tag's source name picks the
// block, so the rewrite keeps the right attribution.
func TestClampCitationTagSpans_KeepsEditorialSource(t *testing.T) {
	a := model.Span{Kind: "time", StartMS: 10000, EndMS: 20000, Sources: []string{"source-a"}}
	b := model.Span{Kind: "time", StartMS: 10000, EndMS: 20000, Sources: []string{"source-b"}}
	citations := []model.Citation{
		{RelPath: "news.mp4", Span: a, RepType: "editorial"},
		{RelPath: "news.mp4", Span: b, RepType: "editorial"},
	}
	out, _ := clampCitationTagSpans("Claim [news.mp4@t=00:10-00:21 › editor annotation (source-b)].", citations)
	if !strings.Contains(out, "editor annotation (source-b)]") {
		t.Fatalf("the rewrite moved the text to another source: %s", out)
	}
}

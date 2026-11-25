package retrieval

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// A plain tag that lands in an editorial-only block is rewritten to the editor
// annotation form: an editorial hit is never cited as a transcript citation
// alone (SPEC §9.3).
func TestClampCitationTagSpans_PlainTagOnEditorialBlockGetsLabel(t *testing.T) {
	span := model.Span{Kind: "time", StartMS: 10000, EndMS: 20000, Sources: []string{"source-a"},
		Attributes: map[string]string{"match_score": "1.25"}}
	citations := []model.Citation{{RelPath: "news.mp4", Span: span, RepType: "editorial"}}
	out, n := clampCitationTagSpans("Claim [news.mp4@t=00:12-00:15].", citations)
	if n != 1 || !strings.Contains(out, "[news.mp4@t=00:12-00:15 › editor annotation (source-a, match 1.25)]") {
		t.Fatalf("a plain tag on an editorial block was not given its label (n=%d): %s", n, out)
	}
}

// An editor tag that names a source no shown block has is rewritten to the
// source of the block it lands in, and to a plain tag when only a transcript
// block was shown.
func TestClampCitationTagSpans_EditorTagWithUnshownSource(t *testing.T) {
	a := model.Span{Kind: "time", StartMS: 10000, EndMS: 20000, Sources: []string{"source-a"}}
	out, n := clampCitationTagSpans("Claim [news.mp4@t=00:12-00:15 › editor annotation (source-b)].",
		[]model.Citation{{RelPath: "news.mp4", Span: a, RepType: "editorial"}})
	if n != 1 || !strings.Contains(out, "[news.mp4@t=00:12-00:15 › editor annotation (source-a)]") {
		t.Fatalf("an editor tag kept an unshown source (n=%d): %s", n, out)
	}
	plain := model.Span{Kind: "time", StartMS: 10000, EndMS: 20000}
	out, n = clampCitationTagSpans("Claim [news.mp4@t=00:12-00:15 › editor annotation (source-b)].",
		[]model.Citation{{RelPath: "news.mp4", Span: plain, RepType: "transcript"}})
	if n != 1 || !strings.Contains(out, "[news.mp4@t=00:12-00:15]") || strings.Contains(out, "editor annotation") {
		t.Fatalf("an editor tag on a transcript block kept the editor label (n=%d): %s", n, out)
	}
	// A tag of its own source inside the block stays as written.
	kept := "Claim [news.mp4@t=00:12-00:15 › editor annotation (source-a)]."
	if out, n := clampCitationTagSpans(kept, []model.Citation{{RelPath: "news.mp4", Span: a, RepType: "editorial"}}); n != 0 || out != kept {
		t.Fatalf("a correct editor tag was rewritten (n=%d): %s", n, out)
	}
}

// Hits of different provenance never share a moment block, so editor text is
// never read under a recognition or transcript header.
func TestGroupMoments_SeparatesProvenance(t *testing.T) {
	span := model.Span{Kind: "time", StartMS: 0, EndMS: 60000, Event: "segment", Sources: []string{"source-a"}}
	recog := model.Span{Kind: "time", StartMS: 0, EndMS: 60000, Event: "segment", Sources: []string{"face"}}
	otherSource := span
	otherSource.Sources = []string{"source-b"}
	hits := []model.SearchHit{
		{RelPath: "news.mp4", RepType: "recognition", Span: recog},
		{RelPath: "news.mp4", RepType: "editorial", Span: span},
		{RelPath: "news.mp4", RepType: "editorial", Span: otherSource},
		{RelPath: "news.mp4", RepType: "editorial", Span: span},
	}
	moments := groupMoments(hits)
	if len(moments) != 3 {
		t.Fatalf("moments = %d, want 3 (recognition, source-a, source-b)", len(moments))
	}
	if got := moments[1].members; len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("the two source-a hits did not share one moment: %v", got)
	}
}

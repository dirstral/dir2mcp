package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// SPEC 9.3 spells a line-range citation [path:L<start>-L<end>]. The emitter
// wrote [path@L<start>-<end>] instead, reusing the TIME separator, and nothing
// caught it because the tag parser has always been lenient — the classic
// shape where a lenient reader hides a wrong writer, and the same mechanism
// that hid the @p=/#p= page drift fixed in #935. Since #934 these tags are
// model-visible in every block header, so the model was learning the wrong
// form (#942).

func TestLineCitationUsesTheSpecSeparator_942(t *testing.T) {
	got := retrieval.FormatCitation("pkg/file.go", model.Span{Kind: "lines", StartLine: 12, EndLine: 48})
	if got != "[pkg/file.go:L12-L48]" {
		t.Fatalf("FormatCitation lines = %q, want [pkg/file.go:L12-L48] (SPEC 9.3)", got)
	}
	// The L is on BOTH bounds. "[path:L12-48]" is a different, non-conforming
	// spelling that a reader could accept without the writer being right.
	if strings.Contains(got, "-48]") && !strings.Contains(got, "-L48]") {
		t.Fatalf("end bound lacks its L: %q", got)
	}
	// And it is not the time separator.
	if strings.Contains(got, "@") {
		t.Fatalf("a line citation must not use the time separator: %q", got)
	}
}

// The parser must still resolve what the OLD emitter produced, or answers and
// stored transcripts generated before the fix would have their citations
// stripped as hallucinations by the attribution checks (#403). The leniency
// that hid the bug is now the compatibility path, and it is pinned as such.
func TestOldLineFormStillResolves_942(t *testing.T) {
	old := "Line 12 declares it [game.mp4@L12-48]."
	svc := tagsService(t, &tagRecordingGenerator{answer: old},
		model.Span{Kind: "lines", StartLine: 12, EndLine: 48})
	got, err := svc.Ask(t.Context(), "q", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !strings.Contains(got.Answer, "[game.mp4@L12-48]") {
		t.Fatalf("an answer citing the pre-fix form was stripped: %q", got.Answer)
	}
}

// The canonical form the emitter now writes must resolve too — otherwise the
// fix would trade a wrong writer for a stripped reader.
func TestCanonicalLineFormResolves_942(t *testing.T) {
	canon := "Line 12 declares it [game.mp4:L12-L48]."
	svc := tagsService(t, &tagRecordingGenerator{answer: canon},
		model.Span{Kind: "lines", StartLine: 12, EndLine: 48})
	got, err := svc.Ask(t.Context(), "q", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !strings.Contains(got.Answer, "[game.mp4:L12-L48]") {
		t.Fatalf("an answer citing the canonical form was stripped: %q", got.Answer)
	}
}

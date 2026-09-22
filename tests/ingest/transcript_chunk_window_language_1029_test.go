package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// SPEC §8.2.2 adds a fourth close rule to the §8.6.1 transcript chunk window: a
// window closes at a language change, so a chunk has one language and the §9.5
// filter never matches a chunk on a language half its text is not in.

func segLang(startMS, endMS int, text, lang string) ingest.ChunkSegment {
	return ingest.ChunkSegment{
		Text: text,
		Span: model.Span{Kind: "time", StartMS: startMS, EndMS: endMS, Language: lang},
	}
}

// TestChunkWindowClosesAtLanguageChange: four consecutive segments well inside
// the 40 s window and with no silence between them would merge into one chunk.
// A language mark on the last two splits them into two chunks, and each chunk
// keeps its members' language.
//
// Mutant killed: dropping the sameSpanLanguage check (one chunk of four segments
// in two languages).
func TestChunkWindowClosesAtLanguageChange(t *testing.T) {
	segs := []ingest.ChunkSegment{
		segLang(0, 8000, "Сегодня мы говорим о выборах.", ""),
		segLang(8000, 15000, "Это важный вопрос.", ""),
		segLang(15000, 23000, "Так, це дуже важливо.", "uk"),
		segLang(23000, 31000, "Ми продовжимо.", "uk"),
	}
	got := ingest.MergeTranscriptChunkWindows(segs, 40, 6)
	if len(got) != 2 {
		t.Fatalf("merged into %d chunks, want 2 split at the language change: %+v", len(got), got)
	}
	if got[0].Span.Language != "" || got[0].Span.EndMS != 15000 {
		t.Errorf("first chunk = %+v, want the two unmarked segments ending at 15000", got[0].Span)
	}
	if got[1].Span.Language != "uk" || got[1].Span.StartMS != 15000 || got[1].Span.EndMS != 31000 {
		t.Errorf("second chunk = %+v, want the two uk segments over [15000,31000]", got[1].Span)
	}
}

// TestChunkWindowUnmarkedSegmentsStillMerge pins that the rule is inert for a
// transcript with no language marks: the merge behaves exactly as before.
func TestChunkWindowUnmarkedSegmentsStillMerge(t *testing.T) {
	segs := []ingest.ChunkSegment{
		segLang(0, 8000, "one", ""),
		segLang(8000, 15000, "two", ""),
		segLang(15000, 23000, "three", ""),
	}
	got := ingest.MergeTranscriptChunkWindows(segs, 40, 6)
	if len(got) != 1 {
		t.Fatalf("merged into %d chunks, want 1", len(got))
	}
}

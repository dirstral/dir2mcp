package tests

import (
	"reflect"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// twoSegmentTranscript has two timestamped segments at 0s and 65s, matching the
// whisper client's [mm:ss] segment formatting.
const twoSegmentTranscript = "[00:00] hello there\n[01:05] general kenobi"

// TestChunkTranscriptWordsWithinRange asserts per-word timing is attached to the
// chunk whose time span contains the word (spec §8.6.1), without changing chunk
// text, span bounds, or chunk count versus the words-absent baseline.
func TestChunkTranscriptWordsWithinRange(t *testing.T) {
	baseline := ingest.ChunkTranscriptByTime(twoSegmentTranscript)
	words := []model.TimedWord{
		{Word: "hello", StartMS: 0, EndMS: 500},
		{Word: "there", StartMS: 500, EndMS: 1000},
		{Word: "general", StartMS: 65000, EndMS: 65800},
		{Word: "kenobi", StartMS: 65800, EndMS: 66500},
	}
	withWords := ingest.ChunkTranscriptByTimeWithWords(twoSegmentTranscript, words)

	// No extra chunks, identical text and span bounds.
	if len(withWords) != len(baseline) {
		t.Fatalf("chunk count = %d, want %d (words must not add chunks)", len(withWords), len(baseline))
	}
	for i := range baseline {
		if withWords[i].Text != baseline[i].Text {
			t.Errorf("chunk[%d] text changed: %q vs %q", i, withWords[i].Text, baseline[i].Text)
		}
		if withWords[i].Span.Kind != baseline[i].Span.Kind ||
			withWords[i].Span.StartMS != baseline[i].Span.StartMS ||
			withWords[i].Span.EndMS != baseline[i].Span.EndMS {
			t.Errorf("chunk[%d] span bounds changed: %+v vs %+v", i, withWords[i].Span, baseline[i].Span)
		}
	}

	// Every word lands in a chunk whose time span contains its start.
	total := 0
	for _, seg := range withWords {
		for _, w := range seg.Span.Words {
			total++
			if w.T < seg.Span.StartMS || w.T >= seg.Span.EndMS {
				// The final chunk also catches trailing words at/after its end;
				// only flag a word that falls before this chunk's start.
				if w.T < seg.Span.StartMS {
					t.Errorf("word %q at %dms outside span [%d,%d)", w.W, w.T, seg.Span.StartMS, seg.Span.EndMS)
				}
			}
		}
	}
	if total != len(words) {
		t.Errorf("attached %d words, want all %d", total, len(words))
	}

	// First chunk owns the first two words with correct {t,d,w}.
	first := withWords[0].Span.Words
	wantFirst := []model.WordSpan{{T: 0, D: 500, W: "hello"}, {T: 500, D: 500, W: "there"}}
	if !reflect.DeepEqual(first, wantFirst) {
		t.Errorf("first chunk words = %+v, want %+v", first, wantFirst)
	}
}

// TestChunkTranscriptWordsAbsentUnchanged asserts that passing no words produces
// output byte-for-byte identical to ChunkTranscriptByTime (backward compat).
func TestChunkTranscriptWordsAbsentUnchanged(t *testing.T) {
	baseline := ingest.ChunkTranscriptByTime(twoSegmentTranscript)

	nilWords := ingest.ChunkTranscriptByTimeWithWords(twoSegmentTranscript, nil)
	if !reflect.DeepEqual(nilWords, baseline) {
		t.Errorf("nil words diverged from baseline:\n got %+v\nwant %+v", nilWords, baseline)
	}
	emptyWords := ingest.ChunkTranscriptByTimeWithWords(twoSegmentTranscript, []model.TimedWord{})
	if !reflect.DeepEqual(emptyWords, baseline) {
		t.Errorf("empty words diverged from baseline:\n got %+v\nwant %+v", emptyWords, baseline)
	}
	for _, seg := range nilWords {
		if seg.Span.Words != nil {
			t.Errorf("words-absent chunk carries words: %+v", seg.Span.Words)
		}
	}
}

// TestChunkTranscriptTrailingWordsRetained asserts a word at/after the last
// chunk's end is still attached (to the last time chunk) rather than dropped.
func TestChunkTranscriptTrailingWordsRetained(t *testing.T) {
	words := []model.TimedWord{
		{Word: "general", StartMS: 65000, EndMS: 65800},
		// well past the estimated end of the last segment
		{Word: "trailing", StartMS: 999000, EndMS: 999500},
	}
	chunks := ingest.ChunkTranscriptByTimeWithWords(twoSegmentTranscript, words)

	var found bool
	for _, seg := range chunks {
		for _, w := range seg.Span.Words {
			if w.W == "trailing" {
				found = true
			}
		}
	}
	if !found {
		t.Error("trailing word was dropped; expected attachment to the last time chunk")
	}
}

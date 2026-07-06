package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// TestBuildCuesForSegmentationUsesWordTimings pins that in broadcast mode a
// transcript carrying per-word timings re-segments from them (BuildBroadcastCues):
// two well-separated spoken words with a real pause between them must split into
// two cues, which the word-timed path does and a chunk/reflow path would not.
func TestBuildCuesForSegmentationUsesWordTimings(t *testing.T) {
	chunks := []subtitle.TranscriptChunk{{
		Text: "Hello. Later.",
		Span: model.Span{Kind: "time", StartMS: 0, EndMS: 12000, Words: []model.WordSpan{
			{T: 0, D: 400, W: "Hello."},
			{T: 10000, D: 400, W: "Later."},
		}},
	}}
	cues := buildCuesForSegmentation(chunks, "broadcast", false)
	if len(cues) != 2 {
		t.Fatalf("word-timed path should split at the 10 s pause into 2 cues, got %d: %+v", len(cues), cues)
	}
}

// TestBuildCuesForSegmentationReflowsWhenNoWordTimings pins the fallback: a
// broadcast-mode transcript with NO per-word timings (e.g. a line-by-line chat
// translation) reflows its chunk cues into broadcast-legible ones rather than
// emitting the raw over-long chunk. A single long, sub-second chunk must be
// broken up and read comfortably (<= 22 cps) after reflow.
func TestBuildCuesForSegmentationReflowsWhenNoWordTimings(t *testing.T) {
	chunks := []subtitle.TranscriptChunk{{
		Text: "We have submitted a formal request to the ministry today.",
		Span: model.Span{Kind: "time", StartMS: 0, EndMS: 6000}, // no Words
	}}
	cues := buildCuesForSegmentation(chunks, "broadcast", false)
	if len(cues) == 0 {
		t.Fatal("expected reflowed cues, got none")
	}
	worst := 0.0
	for _, c := range cues {
		dur := float64(c.EndMS-c.StartMS) / 1000.0
		if dur <= 0 {
			continue
		}
		if cps := float64(len([]rune(strings.ReplaceAll(c.Text, "\n", "")))) / dur; cps > worst {
			worst = cps
		}
		for _, line := range strings.Split(c.Text, "\n") {
			if n := len([]rune(line)); n > 42 {
				t.Errorf("reflowed line %q is %d chars, exceeds 42", line, n)
			}
		}
	}
	if worst > 22 {
		t.Errorf("reflow reading speed %.1f cps exceeds 22", worst)
	}
}

// TestBuildCuesForSegmentationTranslationAlwaysReflows pins the routing hardening:
// a translation is reflowed even when it carries per-word timings (fabricated —
// piled at the cue start), so it must NOT reuse BuildBroadcastCues' word-timed
// segmentation and must equal the reflow path. Guards against a future translate
// provider emitting word timings and silently bypassing reflow (the 56 cps regression).
func TestBuildCuesForSegmentationTranslationAlwaysReflows(t *testing.T) {
	text := "We have submitted a formal request to the relevant ministry earlier today."
	var words []model.WordSpan
	for _, w := range strings.Fields(text) {
		words = append(words, model.WordSpan{T: 0, D: 0, W: w}) // fabricated: piled at 0
	}
	chunks := []subtitle.TranscriptChunk{{
		Text: text,
		Span: model.Span{Kind: "time", StartMS: 0, EndMS: 6000, Words: words},
	}}
	// Sanity: these word timings ARE honored by the native path (non-nil), so the
	// contrast below is meaningful.
	native := subtitle.BuildBroadcastCues(chunks)
	if native == nil {
		t.Fatal("expected BuildBroadcastCues to honor the (fabricated) word timings")
	}
	got := buildCuesForSegmentation(chunks, "broadcast", true)
	if reflect.DeepEqual(got, native) {
		t.Fatal("translation must not reuse the fabricated word-timed segmentation")
	}
	if want := subtitle.ReflowChunkCues(subtitle.BuildCues(chunks)); !reflect.DeepEqual(got, want) {
		t.Fatalf("translation should take the reflow path; got %+v want %+v", got, want)
	}
}

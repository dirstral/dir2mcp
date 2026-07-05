package cli

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/subtitle"
)

func TestTranscriptRepIsTranslation(t *testing.T) {
	cases := []struct {
		name string
		meta string
		want bool
	}{
		{"translation source", `{"source":"translation","language":"en"}`, true},
		{"case-insensitive", `{"source":"Translation"}`, true},
		{"stt source", `{"source":"stt","language":"tk"}`, false},
		{"missing source", `{"language":"en"}`, false},
		{"empty meta", ``, false},
		{"malformed json", `{not valid`, false},
	}
	for _, tc := range cases {
		if got := transcriptRepIsTranslation(tc.meta); got != tc.want {
			t.Errorf("%s: transcriptRepIsTranslation(%q) = %v, want %v", tc.name, tc.meta, got, tc.want)
		}
	}
}

// maxSegCPS is the highest characters-per-second over a cue slice.
func maxSegCPS(cues []subtitle.Cue) float64 {
	worst := 0.0
	for _, c := range cues {
		dur := float64(c.EndMS-c.StartMS) / 1000.0
		if dur <= 0 {
			continue
		}
		if cps := float64(len([]rune(strings.ReplaceAll(c.Text, "\n", "")))) / dur; cps > worst {
			worst = cps
		}
	}
	return worst
}

// TestBuildCuesForSegmentationTranslationIgnoresWordTimings pins the routing:
// given a segment whose fabricated word timings pile every word at the segment
// start with zero duration (the translator failure mode), the native path
// honors them and crushes the text into a dense cue, while the translation path
// reflows from the reliable 5 s segment span into a comfortably legible cue.
func TestBuildCuesForSegmentationTranslationIgnoresWordTimings(t *testing.T) {
	text := "we have submitted a formal request to the ministry today"
	var words []model.WordSpan
	for _, w := range strings.Fields(text) {
		words = append(words, model.WordSpan{T: 1000, D: 0, W: w}) // all piled at 1000 ms
	}
	chunks := []subtitle.TranscriptChunk{{
		Text: text,
		Span: model.Span{Kind: "time", StartMS: 1000, EndMS: 6000, Words: words},
	}}

	native := buildCuesForSegmentation(chunks, "broadcast", false)
	trans := buildCuesForSegmentation(chunks, "broadcast", true)
	if len(native) == 0 || len(trans) == 0 {
		t.Fatalf("expected cues from both paths: native=%d trans=%d", len(native), len(trans))
	}

	nativeCPS, transCPS := maxSegCPS(native), maxSegCPS(trans)
	if transCPS >= nativeCPS {
		t.Errorf("translation reflow should read slower than honoring piled timings: native=%.1f cps trans=%.1f cps", nativeCPS, transCPS)
	}
	if transCPS > 22 {
		t.Errorf("translation reflow reading speed %.1f cps exceeds 22", transCPS)
	}
}

// TestBuildCuesForSegmentationNativeUsesWordTimings pins that a native transcript
// (isTranslation=false) still re-segments from its per-word timings — spread
// (non-piled) word timings must produce word-timed broadcast cues, not a reflow.
func TestBuildCuesForSegmentationNativeUsesWordTimings(t *testing.T) {
	// Two well-separated spoken words: a real pause between them must split into
	// two cues under the word-timed path.
	chunks := []subtitle.TranscriptChunk{{
		Text: "Hello. Later.",
		Span: model.Span{Kind: "time", StartMS: 0, EndMS: 12000, Words: []model.WordSpan{
			{T: 0, D: 400, W: "Hello."},
			{T: 10000, D: 400, W: "Later."},
		}},
	}}
	native := buildCuesForSegmentation(chunks, "broadcast", false)
	if len(native) != 2 {
		t.Fatalf("native word-timed path should split at the 10 s pause into 2 cues, got %d: %+v", len(native), native)
	}
}

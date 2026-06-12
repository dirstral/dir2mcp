package tests

import (
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/quality"
)

// cleanTranscript is a plausible, varied transcript that should pass every
// default detector.
const cleanTranscript = "Welcome back to the show. Today we are talking about " +
	"the history of typesetting and how movable type changed the spread of " +
	"ideas across Europe. Our guest has spent twenty years studying early " +
	"printed books and will walk us through several rare examples from the " +
	"fifteenth century before we open the floor to questions from listeners."

// repetitionLoop is a degenerate STT hallucination loop. A four-word phrase
// only reaches roughly a quarter of all trigrams, which sits under the
// default 0.30 threshold, so we use a two-word phrase whose top trigram
// dominates the output (~50%). See the deviation note in the PR body.
var repetitionLoop = strings.Repeat("thank you ", 60)

// cyrillicText is a short run of Cyrillic letters.
const cyrillicText = "Привет, это пример текста на русском языке для проверки."

// gibberishText is dominated by the Unicode replacement character.
var gibberishText = strings.Repeat("�", 50) + " ok"

func TestGate_Evaluate(t *testing.T) {
	gate := quality.New(quality.DefaultConfig())

	tests := []struct {
		name       string
		text       string
		ctx        quality.Context
		wantOK     bool
		wantReason quality.Reason
	}{
		{
			name:   "clean transcript passes",
			text:   cleanTranscript,
			ctx:    quality.Context{Modality: quality.ModalityTranscript},
			wantOK: true,
		},
		{
			name:       "repetition loop",
			text:       repetitionLoop,
			ctx:        quality.Context{Modality: quality.ModalityTranscript},
			wantOK:     false,
			wantReason: quality.ReasonRepetitionLoop,
		},
		{
			name:       "empty output",
			text:       "",
			ctx:        quality.Context{Modality: quality.ModalityText},
			wantOK:     false,
			wantReason: quality.ReasonEmptyOutput,
		},
		{
			name:       "whitespace only output",
			text:       "   \n\t  ",
			ctx:        quality.Context{Modality: quality.ModalityText},
			wantOK:     false,
			wantReason: quality.ReasonEmptyOutput,
		},
		{
			name:       "cyrillic with expected english",
			text:       cyrillicText,
			ctx:        quality.Context{ExpectedLanguage: "en"},
			wantOK:     false,
			wantReason: quality.ReasonLanguageMismatch,
		},
		{
			name:   "cyrillic with expected russian",
			text:   cyrillicText,
			ctx:    quality.Context{ExpectedLanguage: "ru"},
			wantOK: true,
		},
		{
			name:   "cyrillic with no expected language",
			text:   cyrillicText,
			ctx:    quality.Context{},
			wantOK: true,
		},
		{
			name:       "tiny text over long duration",
			text:       "Okay.",
			ctx:        quality.Context{Modality: quality.ModalityTranscript, Duration: 60 * time.Minute},
			wantOK:     false,
			wantReason: quality.ReasonLowDensity,
		},
		{
			name:       "too few segments",
			text:       "One line only.",
			ctx:        quality.Context{SourceSegmentCount: 30},
			wantOK:     false,
			wantReason: quality.ReasonLowDensity,
		},
		{
			name:       "gibberish ocr output",
			text:       gibberishText,
			ctx:        quality.Context{Modality: quality.ModalityOCR},
			wantOK:     false,
			wantReason: quality.ReasonGibberish,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := gate.Evaluate(tc.text, tc.ctx)
			if v.OK() != tc.wantOK {
				t.Fatalf("OK() = %v, want %v; findings=%+v", v.OK(), tc.wantOK, v.Findings)
			}
			if tc.wantOK {
				return
			}
			p := v.Primary()
			if p == nil {
				t.Fatalf("Primary() = nil, want reason %q", tc.wantReason)
			}
			if p.Reason != tc.wantReason {
				t.Fatalf("Primary().Reason = %q, want %q; findings=%+v",
					p.Reason, tc.wantReason, v.Findings)
			}
		})
	}
}

func TestGate_OptOut(t *testing.T) {
	cfg := quality.DefaultConfig()
	cfg.Repetition.Enabled = false
	gate := quality.New(cfg)

	v := gate.Evaluate(repetitionLoop, quality.Context{Modality: quality.ModalityTranscript})
	if !v.OK() {
		t.Fatalf("expected OK() with repetition disabled, got findings=%+v", v.Findings)
	}
}

func TestRepetition_ShortTextSkipped(t *testing.T) {
	gate := quality.New(quality.DefaultConfig())

	v := gate.Evaluate("yes yes yes", quality.Context{Modality: quality.ModalityTranscript})
	for _, f := range v.Findings {
		if f.Reason == quality.ReasonRepetitionLoop {
			t.Fatalf("did not expect ReasonRepetitionLoop for short text; findings=%+v", v.Findings)
		}
	}
}

func TestConfig_WithDefaults_FillsThresholds(t *testing.T) {
	got := quality.Config{}.WithDefaults()
	want := quality.DefaultConfig()

	if got.Empty.MinChars != want.Empty.MinChars {
		t.Errorf("Empty.MinChars = %d, want %d", got.Empty.MinChars, want.Empty.MinChars)
	}
	if got.Repetition.NGram != want.Repetition.NGram {
		t.Errorf("Repetition.NGram = %d, want %d", got.Repetition.NGram, want.Repetition.NGram)
	}
	if got.Repetition.MaxRepeatFraction != want.Repetition.MaxRepeatFraction {
		t.Errorf("Repetition.MaxRepeatFraction = %v, want %v",
			got.Repetition.MaxRepeatFraction, want.Repetition.MaxRepeatFraction)
	}
	if got.Repetition.MinWords != want.Repetition.MinWords {
		t.Errorf("Repetition.MinWords = %d, want %d", got.Repetition.MinWords, want.Repetition.MinWords)
	}
	if got.Gibberish.MaxNonPrintableFraction != want.Gibberish.MaxNonPrintableFraction {
		t.Errorf("Gibberish.MaxNonPrintableFraction = %v, want %v",
			got.Gibberish.MaxNonPrintableFraction, want.Gibberish.MaxNonPrintableFraction)
	}
	if got.Language.MaxOffScriptFraction != want.Language.MaxOffScriptFraction {
		t.Errorf("Language.MaxOffScriptFraction = %v, want %v",
			got.Language.MaxOffScriptFraction, want.Language.MaxOffScriptFraction)
	}
	if got.Density.MinCharsPerMinute != want.Density.MinCharsPerMinute {
		t.Errorf("Density.MinCharsPerMinute = %v, want %v",
			got.Density.MinCharsPerMinute, want.Density.MinCharsPerMinute)
	}
	if got.Density.MinSegmentRatio != want.Density.MinSegmentRatio {
		t.Errorf("Density.MinSegmentRatio = %v, want %v",
			got.Density.MinSegmentRatio, want.Density.MinSegmentRatio)
	}
}

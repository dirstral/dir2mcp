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

// repetitionLoop is a degenerate space-delimited STT hallucination loop: a
// short phrase repeated until it dominates the whole output.
var repetitionLoop = strings.Repeat("thank you ", 60)

// cjkRepetitionLoop is the same failure mode in a script without whitespace
// word boundaries (a looping "thanks for watching"). A whitespace-tokenized
// detector is blind to it; a script-agnostic one is not.
var cjkRepetitionLoop = strings.Repeat("感谢观看", 40)

// cyrillicText is a short run of Cyrillic letters.
const cyrillicText = "Привет, это пример текста на русском языке для проверки."

// sparseMusicTranscript is a legitimately sparse transcript (music with long
// instrumental stretches). It is well under any reasonable chars-per-minute
// floor but is valid content, not a failure.
const sparseMusicTranscript = "[Music]\n" +
	"We are the champions, my friends.\n" +
	"[Music]\n" +
	"And we'll keep on fighting till the end.\n" +
	"[Music]"

// gibberishText is dominated by the Unicode replacement character (corrupted
// decoding).
var gibberishText = strings.Repeat("�", 50) + " ok"

// asciiGibberish is printable-ASCII non-language garbage: a consonant mash
// with no vowels. It decodes cleanly, so the non-printable check passes it;
// the coherence check must catch it.
const asciiGibberish = "kzrtbqxwvlmnpdfghjklzxcvbnmqwrtpsdgfhbcgjklmnpqrstvwxzkgbrmntpl"

// symbolSoup is printable-ASCII garbage made mostly of symbols rather than
// letters.
const symbolSoup = "^=~^=~<>|^=~<>|^=` `^~<>|=^~ ^=<>|~^=<>|`^~=<>|^=~<>`|^=~<>"

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
			name:       "space-delimited repetition loop",
			text:       repetitionLoop,
			ctx:        quality.Context{Modality: quality.ModalityTranscript},
			wantOK:     false,
			wantReason: quality.ReasonRepetitionLoop,
		},
		{
			name:       "cjk repetition loop",
			text:       cjkRepetitionLoop,
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
			name:   "sparse music transcript over long duration passes",
			text:   sparseMusicTranscript,
			ctx:    quality.Context{Modality: quality.ModalityTranscript, Duration: 60 * time.Minute},
			wantOK: true,
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
		{
			name:       "printable ascii consonant mash",
			text:       asciiGibberish,
			ctx:        quality.Context{Modality: quality.ModalityTranscript},
			wantOK:     false,
			wantReason: quality.ReasonGibberish,
		},
		{
			name:       "printable ascii symbol soup",
			text:       symbolSoup,
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

// TestDensity_DurationFloorOptIn documents that the duration-based density
// floor is disabled by default (sparse audio passes) but can be re-enabled
// per corpus via config, at which point the same sparse transcript trips.
func TestDensity_DurationFloorOptIn(t *testing.T) {
	ctx := quality.Context{Modality: quality.ModalityTranscript, Duration: 60 * time.Minute}

	def := quality.New(quality.DefaultConfig())
	if v := def.Evaluate(sparseMusicTranscript, ctx); !v.OK() {
		t.Fatalf("default gate should not quarantine sparse audio; findings=%+v", v.Findings)
	}

	cfg := quality.DefaultConfig()
	cfg.Density.MinCharsPerMinute = 30
	strict := quality.New(cfg)
	v := strict.Evaluate(sparseMusicTranscript, ctx)
	if v.OK() {
		t.Fatal("gate with duration floor enabled should quarantine sparse audio")
	}
	if p := v.Primary(); p == nil || p.Reason != quality.ReasonLowDensity {
		t.Fatalf("expected ReasonLowDensity, got %+v", v.Findings)
	}
}

func TestConfig_WithDefaults_FillsThresholds(t *testing.T) {
	got := quality.Config{}.WithDefaults()
	want := quality.DefaultConfig()

	if got.Empty.MinChars != want.Empty.MinChars {
		t.Errorf("Empty.MinChars = %d, want %d", got.Empty.MinChars, want.Empty.MinChars)
	}
	if got.Repetition.MaxRepeatFraction != want.Repetition.MaxRepeatFraction {
		t.Errorf("Repetition.MaxRepeatFraction = %v, want %v",
			got.Repetition.MaxRepeatFraction, want.Repetition.MaxRepeatFraction)
	}
	if got.Repetition.MaxPeriod != want.Repetition.MaxPeriod {
		t.Errorf("Repetition.MaxPeriod = %d, want %d", got.Repetition.MaxPeriod, want.Repetition.MaxPeriod)
	}
	if got.Repetition.MinRunes != want.Repetition.MinRunes {
		t.Errorf("Repetition.MinRunes = %d, want %d", got.Repetition.MinRunes, want.Repetition.MinRunes)
	}
	if got.Gibberish.MaxNonPrintableFraction != want.Gibberish.MaxNonPrintableFraction {
		t.Errorf("Gibberish.MaxNonPrintableFraction = %v, want %v",
			got.Gibberish.MaxNonPrintableFraction, want.Gibberish.MaxNonPrintableFraction)
	}
	if got.Gibberish.MaxOtherFraction != want.Gibberish.MaxOtherFraction {
		t.Errorf("Gibberish.MaxOtherFraction = %v, want %v",
			got.Gibberish.MaxOtherFraction, want.Gibberish.MaxOtherFraction)
	}
	if got.Gibberish.MinVowelFraction != want.Gibberish.MinVowelFraction {
		t.Errorf("Gibberish.MinVowelFraction = %v, want %v",
			got.Gibberish.MinVowelFraction, want.Gibberish.MinVowelFraction)
	}
	if got.Gibberish.MinVowelSampleLetters != want.Gibberish.MinVowelSampleLetters {
		t.Errorf("Gibberish.MinVowelSampleLetters = %d, want %d",
			got.Gibberish.MinVowelSampleLetters, want.Gibberish.MinVowelSampleLetters)
	}
	if got.Language.MaxOffScriptFraction != want.Language.MaxOffScriptFraction {
		t.Errorf("Language.MaxOffScriptFraction = %v, want %v",
			got.Language.MaxOffScriptFraction, want.Language.MaxOffScriptFraction)
	}
	if got.Density.MinSegmentRatio != want.Density.MinSegmentRatio {
		t.Errorf("Density.MinSegmentRatio = %v, want %v",
			got.Density.MinSegmentRatio, want.Density.MinSegmentRatio)
	}
}

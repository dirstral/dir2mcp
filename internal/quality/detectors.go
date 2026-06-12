package quality

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// emptyDetector flags output that is effectively empty after trimming
// surrounding whitespace.
type emptyDetector struct {
	minChars int
}

func (emptyDetector) Name() string { return "empty" }

func (d emptyDetector) Inspect(text string, _ Context) *Finding {
	trimmed := strings.TrimSpace(text)
	n := utf8.RuneCountInString(trimmed)
	if n < d.minChars {
		return &Finding{
			Reason: ReasonEmptyOutput,
			Detail: fmt.Sprintf("output has %d characters (minimum %d)", n, d.minChars),
			Score:  1,
		}
	}
	return nil
}

// repetitionDetector flags output dominated by a single repeated word
// n-gram, the classic STT "thank you for watching" hallucination loop.
type repetitionDetector struct {
	n        int
	maxFrac  float64
	minWords int
}

func (repetitionDetector) Name() string { return "repetition" }

func (d repetitionDetector) Inspect(text string, _ Context) *Finding {
	words := strings.Fields(strings.ToLower(text))
	if len(words) < d.minWords || len(words) < d.n {
		return nil
	}
	counts := make(map[string]int)
	total := 0
	var top string
	topCount := 0
	for i := 0; i+d.n <= len(words); i++ {
		gram := strings.Join(words[i:i+d.n], " ")
		counts[gram]++
		total++
		if counts[gram] > topCount {
			topCount = counts[gram]
			top = gram
		}
	}
	if total == 0 {
		return nil
	}
	frac := float64(topCount) / float64(total)
	if frac > d.maxFrac {
		return &Finding{
			Reason: ReasonRepetitionLoop,
			Detail: fmt.Sprintf("n-gram %q repeats %d/%d (%.0f%%)",
				truncate(top, 40), topCount, total, frac*100),
			Score: frac,
		}
	}
	return nil
}

// gibberishDetector flags output with too high a fraction of replacement,
// control, or private-use runes, a signal of corrupted decoding.
type gibberishDetector struct {
	maxNonPrintable float64
}

func (gibberishDetector) Name() string { return "gibberish" }

func (d gibberishDetector) Inspect(text string, _ Context) *Finding {
	total := 0
	bad := 0
	for _, r := range text {
		if unicode.IsSpace(r) {
			continue
		}
		total++
		if r == utf8.RuneError || r == '�' ||
			unicode.Is(unicode.C, r) || unicode.Is(unicode.Co, r) {
			bad++
		}
	}
	if total == 0 {
		return nil
	}
	frac := float64(bad) / float64(total)
	if frac > d.maxNonPrintable {
		return &Finding{
			Reason: ReasonGibberish,
			Detail: fmt.Sprintf("%d/%d non-printable runes (%.0f%%)", bad, total, frac*100),
			Score:  frac,
		}
	}
	return nil
}

// languageDetector flags output whose script does not match the expected
// language's script. It is conservative: it only fires when the expected
// language maps to a known single script and a majority of letters are
// off-script.
type languageDetector struct {
	maxOffScript float64
}

func (languageDetector) Name() string { return "language" }

func (d languageDetector) Inspect(text string, ctx Context) *Finding {
	expected := scriptForLanguage(ctx.ExpectedLanguage)
	if expected == scriptUnknown {
		return nil
	}
	total := 0
	off := 0
	for _, r := range text {
		if !unicode.IsLetter(r) {
			continue
		}
		total++
		if scriptOf(r) != expected {
			off++
		}
	}
	if total == 0 {
		return nil
	}
	frac := float64(off) / float64(total)
	if frac > d.maxOffScript {
		return &Finding{
			Reason: ReasonLanguageMismatch,
			Detail: fmt.Sprintf("%d/%d letters off expected script %q (%.0f%%)",
				off, total, expected, frac*100),
			Score: frac,
		}
	}
	return nil
}

// densityDetector flags output that is implausibly sparse relative to the
// source media duration or the number of source segments.
type densityDetector struct {
	minCharsPerMinute float64
	minSegmentRatio   float64
}

func (densityDetector) Name() string { return "density" }

func (d densityDetector) Inspect(text string, ctx Context) *Finding {
	chars := float64(utf8.RuneCountInString(strings.TrimSpace(text)))

	if ctx.Duration > 0 {
		minutes := ctx.Duration.Minutes()
		if minutes >= 1 {
			cpm := chars / minutes
			if cpm < d.minCharsPerMinute {
				return &Finding{
					Reason: ReasonLowDensity,
					Detail: fmt.Sprintf("%.0f chars over %.1f min = %.1f chars/min (minimum %.0f)",
						chars, minutes, cpm, d.minCharsPerMinute),
					Score: cpm,
				}
			}
		}
	}

	if ctx.SourceSegmentCount > 0 {
		segments := countSegments(text)
		ratio := float64(segments) / float64(ctx.SourceSegmentCount)
		if ratio < d.minSegmentRatio {
			return &Finding{
				Reason: ReasonLowDensity,
				Detail: fmt.Sprintf("%d/%d source segments (ratio %.2f, minimum %.2f)",
					segments, ctx.SourceSegmentCount, ratio, d.minSegmentRatio),
				Score: ratio,
			}
		}
	}

	return nil
}

// countSegments counts the number of non-empty lines in text, used as a
// proxy for the number of output segments.
func countSegments(text string) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// truncate shortens s to at most max runes, appending an ellipsis when it
// trims. It is rune-aware so multibyte content is not split mid-rune.
func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + "…"
}

// script identifies a writing system.
type script string

// Recognised scripts. scriptUnknown means "no single script could be
// determined".
const (
	scriptUnknown    script = ""
	scriptLatin      script = "Latin"
	scriptCyrillic   script = "Cyrillic"
	scriptGreek      script = "Greek"
	scriptArabic     script = "Arabic"
	scriptHebrew     script = "Hebrew"
	scriptHan        script = "Han"
	scriptHiragana   script = "Hiragana"
	scriptKatakana   script = "Katakana"
	scriptHangul     script = "Hangul"
	scriptDevanagari script = "Devanagari"
)

// scriptRange pairs a script with its Unicode range table.
type scriptRange struct {
	script script
	table  *unicode.RangeTable
}

// scriptRanges enumerates the scripts scriptOf can recognise, in lookup
// order.
var scriptRanges = []scriptRange{
	{scriptLatin, unicode.Latin},
	{scriptCyrillic, unicode.Cyrillic},
	{scriptGreek, unicode.Greek},
	{scriptArabic, unicode.Arabic},
	{scriptHebrew, unicode.Hebrew},
	{scriptHan, unicode.Han},
	{scriptHiragana, unicode.Hiragana},
	{scriptKatakana, unicode.Katakana},
	{scriptHangul, unicode.Hangul},
	{scriptDevanagari, unicode.Devanagari},
}

// scriptOf returns the script of rune r, or scriptUnknown if none match.
func scriptOf(r rune) script {
	for _, sr := range scriptRanges {
		if unicode.Is(sr.table, r) {
			return sr.script
		}
	}
	return scriptUnknown
}

// languageScript maps primary language subtags to their dominant single
// script. Languages with intrinsically mixed scripts (notably "ja", which
// blends Han, Hiragana, and Katakana) are intentionally omitted so the
// language detector stays silent rather than producing false positives.
var languageScript = map[string]script{
	// Latin-script languages.
	"en": scriptLatin,
	"de": scriptLatin,
	"fr": scriptLatin,
	"es": scriptLatin,
	"it": scriptLatin,
	"pt": scriptLatin,
	"nl": scriptLatin,
	"pl": scriptLatin,
	"tr": scriptLatin,
	"id": scriptLatin,
	"vi": scriptLatin,
	// Cyrillic-script languages.
	"ru": scriptCyrillic,
	"uk": scriptCyrillic,
	"bg": scriptCyrillic,
	"sr": scriptCyrillic,
	// Other single-script languages.
	"el": scriptGreek,
	"ar": scriptArabic,
	"fa": scriptArabic,
	"ur": scriptArabic,
	"he": scriptHebrew,
	"zh": scriptHan,
	"ko": scriptHangul,
	"hi": scriptDevanagari,
}

// scriptForLanguage returns the dominant script for a language tag, or
// scriptUnknown if the language is unknown or has mixed scripts. Only the
// primary subtag (before any '-' or '_') is considered.
func scriptForLanguage(lang string) script {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if lang == "" {
		return scriptUnknown
	}
	if i := strings.IndexAny(lang, "-_"); i >= 0 {
		lang = lang[:i]
	}
	return languageScript[lang]
}

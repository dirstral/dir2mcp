package quality

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
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

// repetitionDetector flags output dominated by a single short repeating unit,
// the classic STT "thank you for watching" hallucination loop. It is
// script-agnostic: instead of counting whitespace-delimited words (which is
// blind to scripts without word boundaries, such as a looping CJK phrase), it
// measures the largest share of the normalized rune stream that a single
// repeating period explains. A period-p loop makes rune r[i] equal r[i+p]
// almost everywhere; natural language has no such dominant period at any lag.
type repetitionDetector struct {
	maxFrac   float64
	maxPeriod int
	minRunes  int
}

func (repetitionDetector) Name() string { return "repetition" }

func (d repetitionDetector) Inspect(text string, _ Context) *Finding {
	rs := normalizeRunes(text)
	n := len(rs)
	if n < d.minRunes {
		return nil
	}
	// Only consider periods whose repeated span covers at least half the text,
	// so a genuine dominant loop qualifies while short high-lag windows cannot
	// inflate the fraction by chance.
	maxLag := d.maxPeriod
	if maxLag > n/2 {
		maxLag = n / 2
	}
	if maxLag < 1 {
		return nil
	}
	bestFrac := 0.0
	bestLag := 0
	for p := 1; p <= maxLag; p++ {
		overlap := n - p
		matches := 0
		for i := 0; i < overlap; i++ {
			if rs[i] == rs[i+p] {
				matches++
			}
		}
		frac := float64(matches) / float64(overlap)
		if frac > bestFrac {
			bestFrac = frac
			bestLag = p
		}
	}
	if bestFrac > d.maxFrac {
		// Detail is intentionally content-free: report only the repetition
		// metrics (the period length and coverage), never the repeated text.
		return &Finding{
			Reason: ReasonRepetitionLoop,
			Detail: fmt.Sprintf("dominant period %d runes covers %.0f%% of content",
				bestLag, bestFrac*100),
			Score: bestFrac,
		}
	}
	return nil
}

// normalizeRunes lowercases text and collapses every run of whitespace into a
// single space, returning the result as a rune slice. This makes repetition
// detection insensitive to spacing and case while preserving the character
// sequence that a loop repeats, in any script.
func normalizeRunes(text string) []rune {
	rs := make([]rune, 0, len(text))
	prevSpace := false
	for _, r := range strings.ToLower(text) {
		if unicode.IsSpace(r) {
			if !prevSpace && len(rs) > 0 {
				rs = append(rs, ' ')
			}
			prevSpace = true
			continue
		}
		rs = append(rs, r)
		prevSpace = false
	}
	// Drop a trailing collapsed space, if any.
	if len(rs) > 0 && rs[len(rs)-1] == ' ' {
		rs = rs[:len(rs)-1]
	}
	return rs
}

// gibberishDetector flags incoherent output using three script-agnostic
// signals, in order of confidence:
//
//  1. non-printable runes (replacement/control/private-use) — corrupted
//     decoding;
//  2. symbol density — a high fraction of visible runes that are neither
//     letters, digits, nor punctuation (printable-ASCII symbol soup);
//  3. vowel deficiency — among vowel-bearing alphabetic letters
//     (Latin/Cyrillic/Greek), a vowel share below a low floor (consonant
//     mash such as "qwrtplkjhg"). This signal self-skips for scripts without a
//     vowel/consonant distinction (Han, Arabic, Hebrew, …) and for samples too
//     small to judge, so legitimate multilingual text is never flagged.
type gibberishDetector struct {
	maxNonPrintable float64
	maxOther        float64
	minVowel        float64
	minVowelSample  int
}

func (gibberishDetector) Name() string { return "gibberish" }

// coherenceStats tallies, over the visible (non-whitespace) runes of some
// text, the counts each gibberish signal needs.
type coherenceStats struct {
	visible     int // all non-whitespace runes
	bad         int // replacement/control/private-use runes
	other       int // visible runes that are not letter, digit, or punctuation
	vowelScript int // letters in a vowel-bearing script (Latin/Cyrillic/Greek)
	vowels      int // vowels among vowelScript letters
}

// classify folds a single visible rune into the stats.
func (s *coherenceStats) classify(r rune) {
	s.visible++
	switch {
	case r == utf8.RuneError || r == '�' ||
		unicode.Is(unicode.C, r) || unicode.Is(unicode.Co, r):
		s.bad++
	case unicode.IsLetter(r):
		if isVowelBearingLetter(r) {
			s.vowelScript++
			if isVowel(r) {
				s.vowels++
			}
		}
	case unicode.IsDigit(r) || unicode.IsPunct(r):
		// Digits and punctuation are ordinary in prose; ignore.
	default:
		s.other++
	}
}

func gatherCoherenceStats(text string) coherenceStats {
	var s coherenceStats
	for _, r := range text {
		if unicode.IsSpace(r) {
			continue
		}
		s.classify(r)
	}
	return s
}

func (d gibberishDetector) Inspect(text string, _ Context) *Finding {
	s := gatherCoherenceStats(text)
	if s.visible == 0 {
		return nil
	}

	if f := float64(s.bad) / float64(s.visible); f > d.maxNonPrintable {
		return gibberishFinding(fmt.Sprintf("%d/%d non-printable runes (%.0f%%)", s.bad, s.visible, f*100), f)
	}
	if f := float64(s.other) / float64(s.visible); f > d.maxOther {
		return gibberishFinding(fmt.Sprintf("%d/%d non-language symbols (%.0f%%)", s.other, s.visible, f*100), f)
	}
	if s.vowelScript >= d.minVowelSample {
		if f := float64(s.vowels) / float64(s.vowelScript); f < d.minVowel {
			return gibberishFinding(fmt.Sprintf("%d/%d vowels among alphabetic letters (%.0f%%)",
				s.vowels, s.vowelScript, f*100), 1-f)
		}
	}
	return nil
}

// gibberishFinding builds a ReasonGibberish finding from an already
// content-free detail string and score.
func gibberishFinding(detail string, score float64) *Finding {
	return &Finding{Reason: ReasonGibberish, Detail: detail, Score: score}
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
// number of source segments (a clear signal of dropped content). The
// duration-based floor is opt-in: a low chars-per-minute rate is a legitimate
// outcome for music or long silences, so it is applied only when
// minCharsPerMinute is configured above zero, and never quarantines sparse
// audio on its own by default.
type densityDetector struct {
	minCharsPerMinute float64
	minSegmentRatio   float64
}

func (densityDetector) Name() string { return "density" }

func (d densityDetector) Inspect(text string, ctx Context) *Finding {
	chars := float64(utf8.RuneCountInString(strings.TrimSpace(text)))

	if d.minCharsPerMinute > 0 && ctx.Duration > 0 {
		minutes := ctx.Duration.Minutes()
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
	// "sr" (Serbian) is deliberately omitted: it is digraphic (written in
	// both Cyrillic and Latin), so a bare "sr" tag stays scriptUnknown and
	// valid Latin Serbian output is not flagged as a language mismatch.
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

// baseVowels holds the lowercase base (diacritic-stripped) vowel letters for
// the alphabetic scripts that distinguish vowels from consonants: Latin,
// Greek, and Cyrillic. Accented forms fold to these bases via [foldBase], so
// the set stays small yet covers heavily accented languages.
var baseVowels = func() map[rune]struct{} {
	m := make(map[rune]struct{})
	for _, r := range "aeiou" + "αεηιουω" + "аеиоуыэюяіїєў" {
		m[r] = struct{}{}
	}
	return m
}()

// isVowelBearingLetter reports whether r belongs to an alphabetic script that
// distinguishes vowels from consonants. Scripts without that distinction
// (Han, Kana, Arabic, Hebrew, Devanagari, …) return false so the vowel-floor
// signal self-skips for them.
func isVowelBearingLetter(r rune) bool {
	switch scriptOf(r) {
	case scriptLatin, scriptCyrillic, scriptGreek:
		return true
	default:
		return false
	}
}

// isVowel reports whether r is a vowel in its script, tolerating diacritics
// (e.g. "é", "ü", "ậ", "ή"). ASCII letters take a fast path.
func isVowel(r rune) bool {
	lr := unicode.ToLower(r)
	if lr < utf8.RuneSelf {
		_, ok := baseVowels[lr]
		return ok
	}
	_, ok := baseVowels[foldBase(lr)]
	return ok
}

// foldBase returns the base letter of r with combining diacritics removed, by
// taking the first rune of its canonical (NFD) decomposition.
func foldBase(r rune) rune {
	for _, b := range norm.NFD.String(string(r)) {
		return b
	}
	return r
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

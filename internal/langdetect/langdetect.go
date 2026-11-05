// Package langdetect provides a pure-Go, deterministic best-effort language
// detector used to record a representation's language (SPEC §8.8). It wraps the
// trigram detector github.com/abadojack/whatlanggo and maps its result to a
// BCP-47 primary subtag (ISO 639-1). Detection is best-effort and MUST NOT make
// ingestion fail: callers treat a not-ok result as "unknown language", a
// first-class non-error state.
package langdetect

import (
	"unicode"
	"unicode/utf8"

	"github.com/abadojack/whatlanggo"
)

// minLetters is the minimum number of letter runes the input must contain before
// detection is attempted. Trigram detection on very short or punctuation/number
// heavy text is unreliable and tends to produce confident-looking garbage, so we
// decline (unknown) rather than record a spurious language (§8.8 graceful
// degradation).
const minLetters = 20

// maxDetectBytes caps how much input one detector pass reads. Trigram detection
// is reliable from a few hundred characters, so scanning a multi-megabyte
// document adds latency without improving accuracy. A text up to this size is
// read whole; a longer text is read as maxDetectSamples samples of this size,
// spread over its whole length (SPEC §8.8, dir2mcp #1135).
const maxDetectBytes = 4096

// maxDetectSamples bounds how many samples of a long text are read. A
// transcript's first decode window can hold a jingle, an intro or music in
// another language; a prefix read lets that window decide the language of the
// whole item. Five samples put at most one in each fifth of the text, so one
// leading passage can win only when the rest of the text does not outvote it.
const maxDetectSamples = 5

// DefaultMinConfidence is the default confidence floor (SPEC §8.8): a detected
// language below this is discarded (left unknown) rather than recorded. The
// floor is spec-optional (MAY); exposing it as a config knob is a follow-up.
const DefaultMinConfidence = 0.65

// Detect returns the BCP-47 primary subtag (ISO 639-1, e.g. "en", "fr") of the
// dominant language in text together with the detector's confidence in [0,1].
//
// ok is false — meaning "unknown language", a first-class non-error state
// (SPEC §8.8) — when any of the following hold:
//   - text has too little letter content to detect reliably (< minLetters);
//   - the detector cannot map its result to an ISO 639-1 primary subtag;
//   - the confidence is below minConfidence (the optional confidence floor).
//
// A non-positive minConfidence disables the floor (any detected tag passes).
// Detection is deterministic for identical input + detector, so the recorded
// language is stable across re-indexing (§8.8 stability), and the detected tag
// is never folded into a representation's derivation identity by callers.
//
// A text longer than maxDetectBytes is not read from its start only. Up to
// maxDetectSamples samples spread over its whole length are read, a sample with
// too little letter signal or a confidence below the floor is skipped, and the
// language is the confidence-weighted vote of the rest (SPEC §8.8). The returned
// confidence is the letter-weighted mean confidence of the samples that voted for
// the winner. When no sample passes, the result is unknown and the confidence is
// the highest one seen, as for a single read below the floor.
func Detect(text string, minConfidence float64) (tag string, confidence float64, ok bool) {
	if len(text) <= maxDetectBytes {
		return detectOne(text, minConfidence)
	}
	return detectSampled(text, minConfidence)
}

// detectOne runs one detector pass over text, which fits in one sample.
func detectOne(text string, minConfidence float64) (string, float64, bool) {
	tag, conf, _, ok := detectSample(text, minConfidence)
	return tag, conf, ok
}

// detectSample runs one detector pass and also returns the sample's letter
// count, the weight of its vote. conf is set when the detector found a language
// but the floor rejected it.
func detectSample(text string, minConfidence float64) (tag string, conf float64, letters int, ok bool) {
	letters = countLetters(text)
	if letters < minLetters {
		return "", 0, letters, false
	}
	info := whatlanggo.Detect(text)
	tag = info.Lang.Iso6391()
	if tag == "" {
		// The detector returned no language it can express as an ISO 639-1
		// primary subtag: treat as unknown.
		return "", 0, letters, false
	}
	if minConfidence > 0 && info.Confidence < minConfidence {
		// Below the configured floor: decline to record a low-confidence guess.
		return "", info.Confidence, letters, false
	}
	return tag, info.Confidence, letters, true
}

// detectSampled is the confidence-weighted vote over the samples of a long text.
// The sample positions depend only on the text length, and a tie goes to the
// lower tag, so the result is deterministic for identical input.
func detectSampled(text string, minConfidence float64) (string, float64, bool) {
	// weight is the sum of confidence x letters per tag: the vote, and also the
	// numerator of the winner's letter-weighted mean confidence.
	weight := map[string]float64{}
	letterSum := map[string]float64{}
	best := 0.0
	for _, sample := range textSamples(text) {
		tag, conf, letters, ok := detectSample(sample, minConfidence)
		if !ok {
			if conf > best {
				best = conf
			}
			continue
		}
		weight[tag] += conf * float64(letters)
		letterSum[tag] += float64(letters)
	}
	winner := ""
	for tag, w := range weight {
		if winner == "" || w > weight[winner] || (w == weight[winner] && tag < winner) {
			winner = tag
		}
	}
	if winner == "" {
		return "", best, false
	}
	return winner, weight[winner] / letterSum[winner], true
}

// textSamples cuts up to maxDetectSamples windows of maxDetectBytes from text,
// with the first at the start, the last at the end and the rest evenly spaced
// between them. Each cut is moved to a rune boundary, so no sample starts or
// ends inside a multi-byte rune.
func textSamples(text string) []string {
	n := (len(text) + maxDetectBytes - 1) / maxDetectBytes
	if n > maxDetectSamples {
		n = maxDetectSamples
	}
	if n < 2 {
		n = 2
	}
	span := len(text) - maxDetectBytes
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		start := runeStart(text, span*i/(n-1))
		end := runeStart(text, start+maxDetectBytes)
		out = append(out, text[start:end])
	}
	return out
}

// runeStart moves i back to the start of the rune that holds byte i, or returns
// len(s) when i is at or past the end.
func runeStart(s string, i int) int {
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// countLetters returns the number of Unicode letter runes in s. Letters (not raw
// length) gauge whether there is enough linguistic signal to detect, so a string
// dominated by digits/punctuation/whitespace is correctly judged too sparse.
func countLetters(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			n++
		}
	}
	return n
}

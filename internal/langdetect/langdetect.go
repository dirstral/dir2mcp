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

// maxDetectBytes caps how much input is fed to the detector. Trigram detection
// is reliable from a few hundred characters, so scanning a multi-megabyte
// document adds latency without improving accuracy; we detect on a prefix.
const maxDetectBytes = 4096

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
func Detect(text string, minConfidence float64) (tag string, confidence float64, ok bool) {
	if len(text) > maxDetectBytes {
		text = text[:maxDetectBytes]
		// The byte cap may split a trailing multi-byte rune; trim back to a
		// well-formed boundary (detection tolerates it, but keep input clean).
		for len(text) > 0 && !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	if countLetters(text) < minLetters {
		return "", 0, false
	}
	info := whatlanggo.Detect(text)
	tag = info.Lang.Iso6391()
	if tag == "" {
		// The detector returned no language it can express as an ISO 639-1
		// primary subtag: treat as unknown.
		return "", 0, false
	}
	if minConfidence > 0 && info.Confidence < minConfidence {
		// Below the configured floor: decline to record a low-confidence guess.
		return "", info.Confidence, false
	}
	return tag, info.Confidence, true
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

package store

import (
	"errors"
	"net"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrorCategory is a coarse classification for chunk-embedding failures.
// Categories exist so the indexing-error count surfaced by
// `dir2mcp status` and `dir2mcp doctor` can be grouped by likely cause,
// rather than just shown as a single "errors: N" total. The classifier
// is deliberately permissive: callers receive ErrorCategoryUnknown for
// any error they cannot pattern-match, and consumers must treat
// unknown as the universal default.
type ErrorCategory string

const (
	// ErrorCategoryUnknown is used when an error cannot be confidently
	// classified. Always safe; never wrong.
	ErrorCategoryUnknown ErrorCategory = "unknown"
	// ErrorCategoryRateLimit indicates the upstream provider returned a
	// 429 / quota-exceeded response. Retryable after backoff.
	ErrorCategoryRateLimit ErrorCategory = "rate_limit"
	// ErrorCategoryPayloadTooLarge indicates the upstream rejected the
	// input because of size limits (HTTP 413, "payload too large",
	// "file too large"). Not retryable without splitting the input.
	ErrorCategoryPayloadTooLarge ErrorCategory = "payload_too_large"
	// ErrorCategoryParseError indicates the document/representation
	// could not be decoded or extracted at the source (corrupt PDF,
	// unsupported format, OCR refused). Not retryable.
	ErrorCategoryParseError ErrorCategory = "parse_error"
	// ErrorCategoryTransientNet indicates a network-level failure
	// (timeout, connection refused, DNS) that is generally retryable.
	ErrorCategoryTransientNet ErrorCategory = "transient_net"
	// ErrorCategoryAuth indicates an authentication / authorization
	// failure (HTTP 401, 403, bad API key). Not retryable without
	// updating the credential.
	ErrorCategoryAuth ErrorCategory = "auth"
	// ErrorCategoryEmbeddingFailure indicates the embed pipeline
	// rejected the input for a reason other than the above (vector
	// dimension mismatch, model output malformed). Not retryable.
	ErrorCategoryEmbeddingFailure ErrorCategory = "embedding_failure"
	// ErrorCategoryQualityGate indicates the output quality gate
	// (spec 0.16.0) rejected the generated transcript/OCR text as
	// degenerate (repetition loop, empty output, off-script, low
	// density, gibberish) before it was embedded. The chunk is
	// quarantined at insert time so the embedding worker never picks
	// it up. Not retryable without a better extraction/transcription.
	ErrorCategoryQualityGate ErrorCategory = "quality_gate"
)

// classifierRule pairs a category with the keyword set that triggers
// it. Order matters: the first matching rule wins, so the more
// specific categories (rate-limit, auth, payload) are listed before
// catch-alls (parse, embedding).
type classifierRule struct {
	category ErrorCategory
	keywords []string
}

// classifierRules is the keyword-driven classification table consumed
// by ClassifyError. Split out as a package var so the function below
// stays under the cyclomatic-complexity budget and so new keywords
// can be added with a single-line edit rather than a new switch case.
var classifierRules = []classifierRule{
	{ErrorCategoryRateLimit, []string{"429", "rate limit", "rate-limit", "quota exceeded", "too many requests"}},
	{ErrorCategoryPayloadTooLarge, []string{"413", "payload too large", "request entity too large", "file too large", "exceeds maximum size"}},
	{ErrorCategoryAuth, []string{"401", "403", "unauthorized", "forbidden", "invalid api key", "authentication"}},
	{ErrorCategoryTransientNet, []string{"timeout", "connection refused", "connection reset", "no such host", "context deadline exceeded", "i/o timeout"}},
	{ErrorCategoryParseError, []string{"parse", "decode", "ocr", "extract", "corrupt", "unsupported"}},
	{ErrorCategoryEmbeddingFailure, []string{"embedding", "embed", "vector", "dimension"}},
}

// ClassifyError returns the ErrorCategory that best describes err.
// Classification is keyword/pattern-based against the error message
// and well-known wrapped types (net errors, http status codes folded
// into the string). Returns ErrorCategoryUnknown when no rule matches
// so callers never need to special-case nil/empty input.
func ClassifyError(err error) ErrorCategory {
	if err == nil {
		return ErrorCategoryUnknown
	}
	if isNetTransient(err) {
		return ErrorCategoryTransientNet
	}
	msg := strings.ToLower(err.Error())
	for _, rule := range classifierRules {
		if containsAny(msg, rule.keywords) {
			return rule.category
		}
	}
	return ErrorCategoryUnknown
}

// containsAny reports whether s contains any of the needles. Pulled
// out of ClassifyError so the rule loop stays a single concern
// (which keyword set matched).
func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// sanitizedReasonMaxBytes bounds the length of error reason strings
// persisted into the chunks table. The cap is small enough that even
// an attacker-controlled error body can't smuggle a useful secret or
// payload chunk through the diagnostics surface, large enough to
// preserve the prose prefix that humans actually read.
const sanitizedReasonMaxBytes = 256

// SanitizeReason produces a chunks.embedding_error payload that is
// safe to surface through dir2mcp status / doctor / support-bundle.
// Upstream error strings can interpolate raw request/response bodies
// (OCR returning a PDF stream excerpt; HTTP clients including a
// failing input fragment; etc.), and those bodies are exactly the
// payloads the "no secrets in diagnostics" contract forbids us from
// persisting. The sanitizer therefore:
//
//  1. Replaces every non-printable byte with a space so binary
//     payload fragments collapse to whitespace runs instead of
//     leaking through as raw bytes.
//  2. Collapses whitespace runs so the previous step doesn't
//     produce sprawling blank ranges.
//  3. Truncates to sanitizedReasonMaxBytes with an elision marker
//     so long payloads can't fill the column.
//
// Callers should pass the original err.Error() output; the returned
// string is what they hand to MarkFailedWithCategory.
func SanitizeReason(raw string) string {
	if raw == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		switch {
		case r == utf8.RuneError:
			// Invalid UTF-8 byte: drop it. These typically come
			// from binary payload fragments that the upstream
			// interpolated into the error string.
			continue
		case unicode.IsPrint(r):
			b.WriteRune(r)
		default:
			// Whitespace and control bytes collapse to a single
			// space; the strings.Fields pass below removes
			// resulting runs.
			b.WriteByte(' ')
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	const ellipsis = "…" // 3 bytes in UTF-8
	if len(out) > sanitizedReasonMaxBytes {
		cut := sanitizedReasonMaxBytes - len(ellipsis)
		// Walk back to a rune boundary so we never split a
		// multi-byte UTF-8 sequence mid-character.
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut] + ellipsis
	}
	return out
}

// isNetTransient reports whether err unwraps to a net.Error that
// claims itself to be a timeout. Separated so the keyword check above
// stays readable.
func isNetTransient(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}

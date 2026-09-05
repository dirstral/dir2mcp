package store

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dirstral/dir2mcp/internal/model"
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

// allErrorCategories lists every category the classifier can produce, in a
// stable order. It is the validation set for operator-supplied category names
// (`reindex --embeddings-only --error-category=…`) so a typo is rejected with
// the real vocabulary instead of silently matching nothing.
var allErrorCategories = []ErrorCategory{
	ErrorCategoryAuth,
	ErrorCategoryRateLimit,
	ErrorCategoryTransientNet,
	ErrorCategoryUnknown,
	ErrorCategoryPayloadTooLarge,
	ErrorCategoryParseError,
	ErrorCategoryEmbeddingFailure,
	ErrorCategoryQualityGate,
}

// requeueableCategories is the set of failures a bare re-run of the embed step
// can plausibly clear, with the chunk's stored bytes untouched (issue #783).
//
// The dividing line is WHERE the fault lives. auth / rate_limit /
// transient_net are provider-side or environmental: the chunk was never the
// problem, and a rotated credential, a quota window that rolled over, or an
// upstream that came back turns the identical request into a success. The
// categories left out — payload_too_large, parse_error, embedding_failure,
// quality_gate — are properties of the input as stored, so re-sending the same
// bytes to the same provider re-fails deterministically and only spends quota.
// Those stay terminal until ingestion produces a different chunk.
//
// unknown is on the retryable side even though, by definition, we cannot say
// where its fault lives. It is the classifier's universal default: it covers
// every message the keyword table did not recognise (5xx-shaped provider
// errors, provider-specific prose) plus every failure recorded through the
// unclassified MarkFailed entry point or written before the category column
// existed. Excluding it would leave that whole class permanently stranded —
// the exact bug this distinction exists to fix — while including it costs at
// most one re-failure that the operator explicitly asked for.
var requeueableCategories = map[ErrorCategory]bool{
	ErrorCategoryAuth:         true,
	ErrorCategoryRateLimit:    true,
	ErrorCategoryTransientNet: true,
	ErrorCategoryUnknown:      true,
}

// NormalizeErrorCategory folds a persisted or operator-supplied category
// string into the canonical enum value. An empty category (a legacy row, or
// one written through MarkFailed without a classification) reads as
// ErrorCategoryUnknown, matching how the failure aggregates in
// loadFailureCategories normalize it in SQL — so "unknown" means the same
// thing on both the reporting and the retry side.
func NormalizeErrorCategory(raw string) ErrorCategory {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return ErrorCategoryUnknown
	}
	return ErrorCategory(trimmed)
}

// IsKnownErrorCategory reports whether raw names a category this binary can
// produce. Used to reject a mistyped --error-category rather than run a retry
// that would silently match zero rows.
func IsKnownErrorCategory(raw string) bool {
	category := NormalizeErrorCategory(raw)
	for _, known := range allErrorCategories {
		if known == category {
			return true
		}
	}
	return false
}

// IsRequeueableCategory reports whether a failed chunk in this category should
// be moved back to pending by a plain embed retry. See requeueableCategories
// for the reasoning behind the split.
func IsRequeueableCategory(raw string) bool {
	return requeueableCategories[NormalizeErrorCategory(raw)]
}

// RequeueableErrorCategories returns the default retry set as strings, in the
// stable order of allErrorCategories.
func RequeueableErrorCategories() []string {
	return filterCategoryStrings(func(c ErrorCategory) bool { return requeueableCategories[c] })
}

// TerminalErrorCategories returns the categories a plain embed retry cannot
// clear, in the stable order of allErrorCategories. Callers use it to explain
// why a category was not retried.
func TerminalErrorCategories() []string {
	return filterCategoryStrings(func(c ErrorCategory) bool { return !requeueableCategories[c] })
}

// KnownErrorCategories returns every category this binary can produce, in a
// stable order, for error messages that need to show the vocabulary.
func KnownErrorCategories() []string {
	return filterCategoryStrings(func(ErrorCategory) bool { return true })
}

// filterCategoryStrings projects allErrorCategories through a predicate,
// preserving declaration order so CLI output is byte-identical across runs.
func filterCategoryStrings(keep func(ErrorCategory) bool) []string {
	out := make([]string, 0, len(allErrorCategories))
	for _, c := range allErrorCategories {
		if keep(c) {
			out = append(out, string(c))
		}
	}
	return out
}

// classifierRule pairs a category with the keyword set that triggers
// it. Order matters: the first matching rule wins, so the more
// specific categories (rate-limit, auth, payload) are listed before
// catch-alls (parse, embedding).
type classifierRule struct {
	category ErrorCategory
	keywords []string
}

// transientNetKeywords is the canonical keyword set for a transient,
// retryable network/upstream failure. It is the single source of truth
// shared by ClassifyError (which labels these transient_net) and
// IsTransientError (which the embedding worker uses to decide whether to
// leave a chunk PENDING for retry rather than permanently failing it,
// issue #412), so the retry decision and the diagnostics label never
// disagree. Beyond the classic socket/DNS failures it also covers the
// transient upstream statuses providers return under load — 503 Service
// Unavailable, 529 Overloaded (Anthropic), and a bare EOF / "connection
// reset" mid-response — all of which a later cycle routinely recovers from.
var transientNetKeywords = []string{
	"timeout", "connection refused", "connection reset", "no such host",
	"context deadline exceeded", "i/o timeout", "503", "529",
	"service unavailable", "overloaded", "eof",
}

// classifierRules is the keyword-driven classification table consumed
// by ClassifyError. Split out as a package var so the function below
// stays under the cyclomatic-complexity budget and so new keywords
// can be added with a single-line edit rather than a new switch case.
var classifierRules = []classifierRule{
	// The spellings here are what providers ACTUALLY emit, not what the phrase
	// looks like in prose. `rate_limit` is the code form every adapter in this
	// repo uses (GEMINI_RATE_LIMIT, MISTRAL_RATE_LIMIT, OPENAI_RATE_LIMIT), and
	// a lowercased code never contains "rate limit" with a space. The quota
	// wordings are Gemini's and OpenAI's own: both answer an exhausted quota
	// with 429 and the sentence "You exceeded your current quota", which
	// reverses "quota exceeded" and so matched nothing (#932). Getting this set
	// wrong is not cosmetic: IsRateLimitError below is the embed worker's retry
	// gate, and a rate limit misread as permanent marks the whole remaining
	// corpus embedding_status=error.
	{ErrorCategoryRateLimit, []string{
		"429", "rate limit", "rate-limit", "rate_limit",
		"quota exceeded", "exceeded your current quota", "insufficient_quota",
		"too many requests",
	}},
	{ErrorCategoryPayloadTooLarge, []string{"413", "payload too large", "request entity too large", "file too large", "exceeds maximum size"}},
	{ErrorCategoryAuth, []string{"401", "403", "unauthorized", "forbidden", "invalid api key", "authentication"}},
	{ErrorCategoryTransientNet, transientNetKeywords},
	{ErrorCategoryParseError, []string{"parse", "decode", "ocr", "extract", "corrupt", "unsupported"}},
	{ErrorCategoryEmbeddingFailure, []string{"embedding", "embed", "vector", "dimension"}},
}

// IsTransientError reports whether err is a transient, retryable failure:
// a net.Error timeout, or a message matching the shared transient_net
// keyword set (connection refused/reset, DNS, 503/529, service
// unavailable, overloaded, EOF). It exists so the embedding worker's
// retry gate (issue #412) and ClassifyError's transient_net label use the
// exact same definition — a failure the worker leaves PENDING is the same
// class the diagnostics surface reports as transient_net. nil ⇒ false.
func IsTransientError(err error) bool {
	if err == nil {
		return false
	}
	if isNetTransient(err) {
		return true
	}
	// A structured 5xx is transient by its status, whatever its prose says.
	// This keeps the promise the doc comment above makes: the retry gate and
	// the transient_net LABEL are the same set. Without it they had already
	// split — ClassifyError calls a structured 500 transient_net (it reads the
	// status) while this returned false, because the keyword set lists 503 and
	// 529 but not 500/502/504 and a provider's prose need not name any of
	// them. That gap is the #932 failure shape exactly: an error every
	// diagnostic reports as retryable, which the embed worker nonetheless
	// treats as permanent and bisects into dead chunks.
	if categoryForStatus(err) == ErrorCategoryTransientNet {
		return true
	}
	return containsAny(strings.ToLower(err.Error()), transientNetKeywords)
}

// IsRateLimitError reports whether err is an upstream rate limit or exhausted
// quota. It is deliberately the SAME keyword set ClassifyError labels
// rate_limit with, for the reason IsTransientError shares transientNetKeywords
// (#412): the embed worker's retry gate and the category the diagnostics
// surface reports must never disagree about one error. A rate limit is not
// transient_net — it is a property of the ACCOUNT, not the connection or the
// chunk — so it needs its own predicate rather than a keyword bolted onto the
// network set. nil ⇒ false.
func IsRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	return ClassifyError(err) == ErrorCategoryRateLimit
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
	// A ProviderError already carries the upstream HTTP status, which is a
	// FACT about the failure, while everything below is a guess about its
	// prose. Read the fact first (#932). Measured: Gemini answers a bad key
	// with "API key not valid. Please pass a valid API key." — the keyword set
	// looks for "invalid api key", the other word order, so a 401 classified
	// as `unknown` and the embed worker's auth guard (which exists to stop a
	// corpus-wide rejection from exploding into O(n) bisection calls) never
	// fired.
	if cat := categoryForStatus(err); cat != "" {
		return cat
	}
	msg := strings.ToLower(err.Error())
	for _, rule := range classifierRules {
		if containsAny(msg, rule.keywords) {
			return rule.category
		}
	}
	return ErrorCategoryUnknown
}

// categoryForStatus maps a structured *model.ProviderError's HTTP status onto
// its category, or "" when err carries no provider status to read. Only the
// statuses whose meaning is unambiguous are mapped; anything else falls
// through to the keyword table, so a provider that returns a 400 with a
// descriptive body still classifies on that body.
//
// This exists because a status code cannot be paraphrased and a message can:
// every provider spells its own rate limit and auth failure differently, and
// three of those spellings had already drifted past the keyword table (#932).
func categoryForStatus(err error) ErrorCategory {
	var provErr *model.ProviderError
	if !errors.As(err, &provErr) || provErr == nil {
		return ""
	}
	switch provErr.StatusCode {
	case http.StatusTooManyRequests:
		return ErrorCategoryRateLimit
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrorCategoryAuth
	case http.StatusRequestEntityTooLarge:
		return ErrorCategoryPayloadTooLarge
	}
	if provErr.StatusCode >= http.StatusInternalServerError {
		return ErrorCategoryTransientNet
	}
	return ""
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

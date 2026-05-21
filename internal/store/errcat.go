package store

import (
	"errors"
	"net"
	"strings"
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

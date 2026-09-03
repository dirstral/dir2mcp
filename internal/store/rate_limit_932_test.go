package store

import (
	"errors"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// The exact strings the providers emit. They are quoted from the real
// responses rather than paraphrased, because #932 was caused by a keyword that
// matched a PARAPHRASE of the message and not the message: "rate limit" with a
// space against a code spelled GEMINI_RATE_LIMIT, and "quota exceeded" against
// a sentence that says "exceeded your current quota".
const (
	geminiQuota = `GEMINI_RATE_LIMIT: {"error":{"code":429,"message":"You exceeded ` +
		`your current quota, please check your plan and billing details.",` +
		`"status":"RESOURCE_EXHAUSTED"}}`
	mistralRateLimit   = `MISTRAL_RATE_LIMIT: {"message":"Requests rate limit exceeded"}`
	openaiInsufficient = `OPENAI_RATE_LIMIT: {"error":{"message":"You exceeded your ` +
		`current quota","type":"insufficient_quota","code":"insufficient_quota"}}`
)

func TestIsRateLimitError_RealProviderShapes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"gemini quota exhausted", errors.New(geminiQuota), true},
		{"mistral rate limit", errors.New(mistralRateLimit), true},
		{"openai insufficient quota", errors.New(openaiInsufficient), true},
		// The code form alone, with no status code and no prose: an adapter
		// that returns only its error code must still be understood.
		{"bare code form", errors.New("gemini_rate_limit"), true},
		{"prose form", errors.New("rate limit exceeded"), true},
		{"429 status", errors.New("upstream returned 429"), true},
		// Not rate limits. An auth failure in particular must stay
		// non-retryable: it rejects every input identically and retrying it
		// forever would hide a wrong key behind an endless pending queue.
		{"auth", errors.New("401 unauthorized"), false},
		{"poison input", errors.New("400 bad request: input exceeds maximum context length"), false},
		{"transient net", errors.New("503 service unavailable"), false},
		{"unrelated", errors.New("something else"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRateLimitError(tc.err); got != tc.want {
				t.Errorf("IsRateLimitError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// The retry gate and the reported category are one fact, not two (#412's
// argument, which #932 proved by counterexample: the classifier said
// rate_limit while the worker said permanent). Whatever IsRateLimitError
// accepts must also be LABELLED rate_limit, or an operator reading diagnostics
// sees a different story than the worker acted on.
func TestRateLimitVerdictAndLabelAgree(t *testing.T) {
	for _, msg := range []string{geminiQuota, mistralRateLimit, openaiInsufficient,
		"gemini_rate_limit", "rate limit exceeded", "upstream returned 429"} {
		err := errors.New(msg)
		if !IsRateLimitError(err) {
			t.Fatalf("IsRateLimitError(%q) = false", msg)
		}
		if got := ClassifyError(err); got != ErrorCategoryRateLimit {
			t.Errorf("ClassifyError(%q) = %q, want %q", msg, got, ErrorCategoryRateLimit)
		}
	}
}

// The status code is a fact; the prose is not. Found while testing #932: Gemini
// answers a bad key with "API key not valid. Please pass a valid API key.",
// which the keyword table looks for as "invalid api key" — the other word
// order — so a real 401 classified as `unknown`. That is not cosmetic: the
// embed worker skips bisection for auth precisely because a corpus-wide
// rejection isolates nothing, and an `unknown` 401 sent the whole batch into
// O(n) halving calls against a provider that had already refused the key.
func TestClassifyError_ReadsProviderStatusBeforeProse(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorCategory
	}{
		{"gemini bad key, prose the table misses",
			&model.ProviderError{Code: "GEMINI_AUTH", Message: "API key not valid. Please pass a valid API key.",
				StatusCode: 401}, ErrorCategoryAuth},
		{"403 forbidden",
			&model.ProviderError{Code: "X_AUTH", Message: "no access", StatusCode: 403}, ErrorCategoryAuth},
		{"429 with prose that names no limit",
			&model.ProviderError{Code: "X_LIMIT", Message: "slow down", StatusCode: 429}, ErrorCategoryRateLimit},
		{"413 payload",
			&model.ProviderError{Code: "X_BIG", Message: "nope", StatusCode: 413}, ErrorCategoryPayloadTooLarge},
		{"503 upstream",
			&model.ProviderError{Code: "X_DOWN", Message: "nope", StatusCode: 503}, ErrorCategoryTransientNet},
		// A 400 is NOT unambiguous: it is the provider rejecting THIS input, and
		// the body says why. It must fall through to the keyword table so a
		// poison chunk still classifies on its message.
		{"400 falls through to the prose",
			&model.ProviderError{Code: "X_FAILED", Message: "input exceeds maximum context length; cannot embed vector",
				StatusCode: 400}, ErrorCategoryEmbeddingFailure},
		// No status at all: prose is all there is.
		{"no status, prose decides",
			&model.ProviderError{Code: "X_FAILED", Message: "connection refused"}, ErrorCategoryTransientNet},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyError(tc.err); got != tc.want {
				t.Errorf("ClassifyError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// A rate limit is a property of the ACCOUNT, not of the connection, so it must
// not be folded into transient_net: the two are reported separately and a
// operator diagnosing "why did indexing stall" needs to know which one it was.
func TestRateLimitIsNotClassifiedAsTransientNet(t *testing.T) {
	err := errors.New(geminiQuota)
	if IsTransientError(err) {
		t.Error("a quota exhaustion must not be labelled transient_net")
	}
	if !IsRateLimitError(err) {
		t.Error("a quota exhaustion must be a rate limit")
	}
}

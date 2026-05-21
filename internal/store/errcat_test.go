package store

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestClassifyError_KnownPatterns(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorCategory
	}{
		{"nil", nil, ErrorCategoryUnknown},
		{"rate-limit-429", errors.New("upstream returned 429 too many requests"), ErrorCategoryRateLimit},
		{"rate-limit-text", errors.New("rate limit exceeded for embedding endpoint"), ErrorCategoryRateLimit},
		{"payload-413", errors.New("HTTP 413 request entity too large"), ErrorCategoryPayloadTooLarge},
		{"payload-text", errors.New("file too large for OCR: 75MB"), ErrorCategoryPayloadTooLarge},
		{"auth-401", errors.New("401 unauthorized"), ErrorCategoryAuth},
		{"auth-403", errors.New("403 forbidden"), ErrorCategoryAuth},
		{"auth-text", errors.New("invalid api key"), ErrorCategoryAuth},
		{"net-deadline", context.DeadlineExceeded, ErrorCategoryTransientNet},
		{"net-text", errors.New("connection refused"), ErrorCategoryTransientNet},
		{"net-timeout", &fakeTimeoutErr{}, ErrorCategoryTransientNet},
		{"parse", errors.New("ocr extract failed: corrupt PDF"), ErrorCategoryParseError},
		{"embed", errors.New("vector dimension mismatch: got 1024 want 1536"), ErrorCategoryEmbeddingFailure},
		{"unknown", errors.New("something else"), ErrorCategoryUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyError(tc.err); got != tc.want {
				t.Errorf("ClassifyError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

type fakeTimeoutErr struct{}

func (e *fakeTimeoutErr) Error() string   { return "i/o timeout" }
func (e *fakeTimeoutErr) Timeout() bool   { return true }
func (e *fakeTimeoutErr) Temporary() bool { return true }

// Compile-time guarantee that fakeTimeoutErr satisfies net.Error so
// the isNetTransient branch of the classifier is exercised.
var _ net.Error = (*fakeTimeoutErr)(nil)
var _ = time.Now // keep imports stable for follow-on tests

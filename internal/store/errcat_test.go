package store

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
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
		{"net-503", errors.New("embed failed: 503 service unavailable"), ErrorCategoryTransientNet},
		{"net-529-overloaded", errors.New("upstream returned: overloaded"), ErrorCategoryTransientNet},
		{"net-eof", errors.New("post /embeddings: unexpected EOF"), ErrorCategoryTransientNet},
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

// TestIsTransientError pins the shared retry gate (issue #412): the broadened
// transient set — connection refused, 503, overloaded, EOF, net timeouts — must
// be reported transient, while permanent failures (auth, dimension) must not.
func TestIsTransientError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"connection-refused", errors.New("dial tcp: connect: connection refused"), true},
		{"connection-reset", errors.New("read: connection reset by peer"), true},
		{"no-such-host", errors.New("lookup api.example.com: no such host"), true},
		{"http-503", errors.New("embed failed: 503 service unavailable"), true},
		{"overloaded-529", errors.New("upstream returned: overloaded"), true},
		{"eof", errors.New("post /embeddings: unexpected EOF"), true},
		{"wrapped-io-eof", io.EOF, true},
		{"net-timeout", &fakeTimeoutErr{}, true},
		{"deadline", context.DeadlineExceeded, true},
		{"auth-permanent", errors.New("401 unauthorized"), false},
		{"dimension-permanent", errors.New("vector dimension mismatch"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransientError(tc.err); got != tc.want {
				t.Errorf("IsTransientError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestSanitizeReason(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"short prose", "ocr returned 429 rate limit", "ocr returned 429 rate limit"},
		{
			"binary scrubbed",
			"ocr failed: \x00\x01\x02 PDFDATA \xff\xfe",
			"ocr failed: PDFDATA",
		},
		{
			"newlines collapsed",
			"ocr failed:\n  line1\n  line2\n",
			"ocr failed: line1 line2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeReason(tc.in); got != tc.want {
				t.Errorf("SanitizeReason(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeReason_TruncatesLongInput(t *testing.T) {
	// 1000-byte error body simulates an upstream interpolating a
	// large response chunk. The sanitizer must cap the persisted
	// reason regardless of input length so a single failure can't
	// fill the column.
	long := strings.Repeat("a", 1000)
	got := SanitizeReason(long)
	if len(got) > sanitizedReasonMaxBytes {
		t.Fatalf("len(SanitizeReason) = %d, want <= %d", len(got), sanitizedReasonMaxBytes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected truncation marker '…' at end, got %q", got[len(got)-10:])
	}
}

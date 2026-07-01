package openai

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// TestReadLimitedBody_OverLimitErrors pins issue #416: a success body that
// exceeds the cap is rejected with a clear, non-retryable provider error
// instead of being buffered unbounded (OOM). A within-cap body is returned in
// full.
func TestReadLimitedBody_OverLimitErrors(t *testing.T) {
	const n = 100
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(strings.Repeat("a", n)))}
	if _, err := readLimitedBody(resp, 10); err == nil {
		t.Fatal("over-limit body must error, got nil")
	} else {
		var pe *model.ProviderError
		if !errors.As(err, &pe) || pe.Code != "OPENAI_FAILED" || pe.Retryable {
			t.Fatalf("want non-retryable OPENAI_FAILED, got %v", err)
		}
	}

	resp = &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(strings.Repeat("a", n)))}
	got, err := readLimitedBody(resp, 1000)
	if err != nil {
		t.Fatalf("within-limit body must succeed, got %v", err)
	}
	if len(got) != n {
		t.Fatalf("got %d bytes, want %d", len(got), n)
	}
}

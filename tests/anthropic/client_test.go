package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/anthropic"
	"github.com/dirstral/dir2mcp/internal/model"
)

func newClient(url string) *anthropic.Client {
	c := anthropic.NewClient(url, "test-key")
	c.InitialBackoff = time.Millisecond
	c.MaxBackoff = time.Millisecond
	return c
}

func asProviderErr(err error, target **model.ProviderError) bool {
	if err == nil {
		return false
	}
	pe, ok := err.(*model.ProviderError)
	if ok {
		*target = pe
	}
	return ok && strings.HasPrefix(pe.Code, "ANTHROPIC_")
}

func TestGenerate_HappyPathConcatenatesTextParts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Errorf("x-api-key = %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("anthropic-version = %q", got)
		}
		if got := r.Header.Get("content-type"); got != "application/json" {
			t.Errorf("content-type = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "hello "},
				{"type": "text", "text": "world"},
			},
		})
	}))
	defer srv.Close()

	out, err := newClient(srv.URL).Generate(context.Background(), "hi")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if out != "hello world" {
		t.Fatalf("content = %q, want %q", out, "hello world")
	}
}

func TestGenerate_MissingKeyIsNonRetryableAuthNoCall(t *testing.T) {
	var called int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&called, 1)
	}))
	defer srv.Close()

	c := anthropic.NewClient(srv.URL, "")
	_, err := c.Generate(context.Background(), "hi")
	var pe *model.ProviderError
	if !asProviderErr(err, &pe) || pe.Code != "ANTHROPIC_AUTH" || pe.Retryable {
		t.Fatalf("want non-retryable ANTHROPIC_AUTH, got %v", err)
	}
	if n := atomic.LoadInt32(&called); n != 0 {
		t.Fatalf("missing key must not perform an HTTP call, got %d calls", n)
	}
}

func TestGenerate_RateLimitRetriesThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 { // first call: transient 429
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok"}},
		})
	}))
	defer srv.Close()

	out, err := newClient(srv.URL).Generate(context.Background(), "hi")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if out != "ok" {
		t.Fatalf("content = %q, want %q", out, "ok")
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("calls = %d, want 2 (1 retry + success)", n)
	}
}

func TestHTTPErrorMapping(t *testing.T) {
	cases := []struct {
		status    int
		wantCode  string
		retryable bool
	}{
		{http.StatusUnauthorized, "ANTHROPIC_AUTH", false},
		{http.StatusForbidden, "ANTHROPIC_AUTH", false},
		{http.StatusTooManyRequests, "ANTHROPIC_RATE_LIMIT", true},
		{http.StatusBadGateway, "ANTHROPIC_FAILED", true},
		{http.StatusBadRequest, "ANTHROPIC_FAILED", false},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
		}))
		c := newClient(srv.URL)
		c.MaxRetries = 0
		_, err := c.Generate(context.Background(), "hi")
		srv.Close()
		var pe *model.ProviderError
		if !asProviderErr(err, &pe) || pe.Code != tc.wantCode || pe.Retryable != tc.retryable {
			t.Fatalf("status %d: got %v, want %s retryable=%v", tc.status, err, tc.wantCode, tc.retryable)
		}
	}
}

func TestGenerate_NoContentIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"content": []any{}})
	}))
	defer srv.Close()
	_, err := newClient(srv.URL).Generate(context.Background(), "hi")
	var pe *model.ProviderError
	if !asProviderErr(err, &pe) || pe.Code != "ANTHROPIC_FAILED" {
		t.Fatalf("want ANTHROPIC_FAILED, got %v", err)
	}
}

// TestGenerate_UsesGenerationTimeoutNotDefault mirrors the openai
// adapter test: NewClient always sets HTTPClient (30s), so the per-call
// GenerationTimeout must still be applied. With a 50ms GenerationTimeout
// and a 300ms-slow server, Generate must fail fast (~50ms) rather than
// hang toward the 30s default.
func TestGenerate_UsesGenerationTimeoutNotDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": "late"}},
		})
	}))
	defer srv.Close()

	c := anthropic.NewClient(srv.URL, "test-key") // default HTTPClient: 30s
	c.MaxRetries = 0
	c.GenerationTimeout = 50 * time.Millisecond

	start := time.Now()
	_, err := c.Generate(context.Background(), "hi")
	if err == nil {
		t.Fatal("want timeout error, got nil")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Generate did not honor GenerationTimeout (took %s)", elapsed)
	}
}

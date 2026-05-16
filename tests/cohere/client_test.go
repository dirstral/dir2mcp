package tests

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/cohere"
	"github.com/dirstral/dir2mcp/internal/model"
)

func newClient(url string) *cohere.Client {
	c := cohere.NewClient(url, "test-key")
	c.InitialBackoff = time.Millisecond
	c.MaxBackoff = 2 * time.Millisecond
	return c
}

func TestRerank_RequestShapeAndOrdering(t *testing.T) {
	var gotAuth, gotPath string
	var body rerankBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		// deliberately out of order; client must preserve provider order
		_, _ = w.Write([]byte(`{"results":[{"index":2,"relevance_score":0.91},{"index":0,"relevance_score":0.40}]}`))
	}))
	defer srv.Close()

	res, err := newClient(srv.URL).Rerank(context.Background(), "rerank-v3.5", "q", []string{"a", "b", "c"}, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/v2/rerank" {
		t.Fatalf("path = %q, want /v2/rerank", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("auth = %q, want Bearer test-key", gotAuth)
	}
	if body.Model != "rerank-v3.5" || body.Query != "q" || len(body.Documents) != 3 || body.TopN != 2 {
		t.Fatalf("unexpected request body: %+v", body)
	}
	if len(res) != 2 || res[0].Index != 2 || res[1].Index != 0 {
		t.Fatalf("provider order not preserved: %+v", res)
	}
	if res[0].RelevanceScore != 0.91 {
		t.Fatalf("score not parsed: %+v", res[0])
	}
}

func TestRerank_EmptyDocumentsShortCircuits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("API must not be called for empty documents")
	}))
	defer srv.Close()
	res, err := newClient(srv.URL).Rerank(context.Background(), "", "q", nil, 0)
	if err != nil || res != nil {
		t.Fatalf("want (nil,nil), got (%v,%v)", res, err)
	}
}

func TestRerank_MissingKeyIsNonRetryableAuth(t *testing.T) {
	c := cohere.NewClient("http://127.0.0.1:0", "")
	_, err := c.Rerank(context.Background(), "", "q", []string{"a"}, 0)
	assertProviderError(t, err, "COHERE_AUTH", false)
}

func TestRerank_DefaultModelWhenEmpty(t *testing.T) {
	var body rerankBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()
	if _, err := newClient(srv.URL).Rerank(context.Background(), "", "q", []string{"a"}, 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body.Model != cohere.DefaultRerankModel {
		t.Fatalf("model = %q, want default %q", body.Model, cohere.DefaultRerankModel)
	}
}

func TestRerank_StatusMapping(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		wantCode  string
		retryable bool
	}{
		{"unauthorized", http.StatusUnauthorized, "COHERE_AUTH", false},
		{"forbidden", http.StatusForbidden, "COHERE_AUTH", false},
		{"rate limit", http.StatusTooManyRequests, "COHERE_RATE_LIMIT", true},
		{"server error", http.StatusBadGateway, "COHERE_FAILED", true},
		{"bad request", http.StatusBadRequest, "COHERE_FAILED", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
			}))
			defer srv.Close()
			_, err := newClient(srv.URL).Rerank(context.Background(), "m", "q", []string{"a"}, 0)
			assertProviderError(t, err, tc.wantCode, tc.retryable)
		})
	}
}

func TestRerank_RetriesThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"index":0,"relevance_score":1}]}`))
	}))
	defer srv.Close()
	res, err := newClient(srv.URL).Rerank(context.Background(), "m", "q", []string{"a"}, 0)
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if len(res) != 1 || atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("calls=%d res=%+v", calls, res)
	}
}

func TestRerank_NonRetryableStopsImmediately(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	_, err := newClient(srv.URL).Rerank(context.Background(), "m", "q", []string{"a"}, 0)
	assertProviderError(t, err, "COHERE_FAILED", false)
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("non-retryable must not retry; calls=%d", calls)
	}
}

func TestRerank_ContextCancelDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := cohere.NewClient(srv.URL, "k")
	c.InitialBackoff = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	_, err := c.Rerank(ctx, "m", "q", []string{"a"}, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestRerank_OutOfRangeIndexRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"index":9,"relevance_score":1}]}`))
	}))
	defer srv.Close()
	_, err := newClient(srv.URL).Rerank(context.Background(), "m", "q", []string{"a"}, 0)
	assertProviderError(t, err, "COHERE_FAILED", false)
}

type rerankBody struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n"`
}

func assertProviderError(t *testing.T, err error, wantCode string, wantRetryable bool) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s, got nil", wantCode)
	}
	var pErr *model.ProviderError
	if !errors.As(err, &pErr) {
		t.Fatalf("error is not *model.ProviderError: %v", err)
	}
	if pErr.Code != wantCode {
		t.Fatalf("code = %q, want %q (msg=%q)", pErr.Code, wantCode, pErr.Message)
	}
	if pErr.Retryable != wantRetryable {
		t.Fatalf("retryable = %v, want %v", pErr.Retryable, wantRetryable)
	}
	if strings.TrimSpace(pErr.Message) == "" {
		t.Fatalf("provider error message is empty")
	}
}

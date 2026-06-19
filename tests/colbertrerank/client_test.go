package tests

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/colbertrerank"
	"github.com/dirstral/dir2mcp/internal/model"
)

type rerankBody struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n"`
}

func newClient(url, apiKey string) *colbertrerank.Client {
	c := colbertrerank.NewClient(url, apiKey)
	c.InitialBackoff = time.Millisecond
	c.MaxBackoff = 2 * time.Millisecond
	return c
}

// TestRerank_ReordersByReturnedScores verifies the client targets POST
// /rerank, sends the query+documents body, and returns results in the exact
// (best-first) provider order — the candidate-pool reordering contract.
func TestRerank_ReordersByReturnedScores(t *testing.T) {
	var gotAuth, gotPath string
	var body rerankBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		// Deliberately out of input order; the client must preserve the
		// provider's best-first ordering.
		_, _ = w.Write([]byte(`{"results":[{"index":2,"relevance_score":0.91},{"index":0,"relevance_score":0.40}]}`))
	}))
	defer srv.Close()

	res, err := newClient(srv.URL, "test-key").Rerank(context.Background(), "colbert-v2", "q", []string{"a", "b", "c"}, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/rerank" {
		t.Fatalf("path = %q, want /rerank", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("auth = %q, want Bearer test-key", gotAuth)
	}
	if body.Model != "colbert-v2" || body.Query != "q" || len(body.Documents) != 3 || body.TopN != 2 {
		t.Fatalf("unexpected request body: %+v", body)
	}
	if len(res) != 2 || res[0].Index != 2 || res[1].Index != 0 {
		t.Fatalf("provider order not preserved: %+v", res)
	}
	if res[0].RelevanceScore != 0.91 {
		t.Fatalf("score not parsed: %+v", res[0])
	}
}

// TestRerank_CredentialLessOmitsAuthHeader verifies a self-hosted box on a
// private network works without an api_key and that NO Authorization header
// is sent in that case.
func TestRerank_CredentialLessOmitsAuthHeader(t *testing.T) {
	var hadAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]
		_, _ = w.Write([]byte(`{"results":[{"index":0,"relevance_score":0.5}]}`))
	}))
	defer srv.Close()

	res, err := newClient(srv.URL, "").Rerank(context.Background(), "", "q", []string{"a"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hadAuth {
		t.Fatalf("credential-less client must not send an Authorization header")
	}
	if len(res) != 1 || res[0].Index != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

// TestRerank_DefaultModelWhenEmpty verifies an empty model name falls back
// to the package default in the request body.
func TestRerank_DefaultModelWhenEmpty(t *testing.T) {
	var body rerankBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	if _, err := newClient(srv.URL, "").Rerank(context.Background(), "", "q", []string{"a"}, 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body.Model != colbertrerank.DefaultModel {
		t.Fatalf("model = %q, want default %q", body.Model, colbertrerank.DefaultModel)
	}
}

// TestRerank_EmptyDocumentsShortCircuits verifies an empty candidate pool
// never touches the network.
func TestRerank_EmptyDocumentsShortCircuits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("endpoint must not be called for empty documents")
	}))
	defer srv.Close()
	res, err := newClient(srv.URL, "").Rerank(context.Background(), "", "q", nil, 0)
	if err != nil || res != nil {
		t.Fatalf("want (nil,nil), got (%v,%v)", res, err)
	}
}

// TestRerank_MissingBaseURLIsNonRetryable verifies an unconfigured endpoint
// (no base_url) fails fast and non-retryably so the retrieval seam can fail
// open without network access.
func TestRerank_MissingBaseURLIsNonRetryable(t *testing.T) {
	_, err := colbertrerank.NewClient("", "").Rerank(context.Background(), "", "q", []string{"a"}, 0)
	assertProviderError(t, err, "COLBERT_FAILED", false)
}

// TestRerank_HTTPErrorsMapToTypedCodes verifies upstream status codes map to
// the documented error codes/retryability, and that the raw upstream body is
// never echoed into the error message (no doc text / secret leakage).
func TestRerank_HTTPErrorsMapToTypedCodes(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		code      string
		retryable bool
	}{
		{"unauthorized", http.StatusUnauthorized, "COLBERT_AUTH", false},
		{"forbidden", http.StatusForbidden, "COLBERT_AUTH", false},
		{"rate_limited", http.StatusTooManyRequests, "COLBERT_RATE_LIMIT", true},
		{"server_error", http.StatusInternalServerError, "COLBERT_FAILED", true},
		{"bad_request", http.StatusBadRequest, "COLBERT_FAILED", false},
	}
	const secret = "SECRET-DOC-TEXT-should-not-leak"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(secret))
			}))
			defer srv.Close()
			c := newClient(srv.URL, "k")
			c.MaxRetries = 0 // keep retryable cases from looping in the test
			_, err := c.Rerank(context.Background(), "", "q", []string{"a"}, 0)
			assertProviderError(t, err, tc.code, tc.retryable)
			var pErr *model.ProviderError
			if errors.As(err, &pErr) && strings.Contains(pErr.Message, secret) {
				t.Fatalf("error message leaked upstream body: %q", pErr.Message)
			}
		})
	}
}

// TestRerank_OutOfRangeIndexRejected verifies a malformed response (index
// outside the documents slice) is a non-retryable failure rather than a
// silent out-of-bounds, so the seam falls open to the fused order.
func TestRerank_OutOfRangeIndexRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"index":5,"relevance_score":0.9}]}`))
	}))
	defer srv.Close()
	_, err := newClient(srv.URL, "k").Rerank(context.Background(), "", "q", []string{"a", "b"}, 0)
	assertProviderError(t, err, "COLBERT_FAILED", false)
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

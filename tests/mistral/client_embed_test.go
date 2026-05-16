package tests

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/mistral"
	"github.com/dirstral/dir2mcp/internal/model"
)

type embedTestRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

func TestEmbed_BatchesRequestsAndPreservesOrder(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("unexpected authorization header: %q", got)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		atomic.AddInt32(&calls, 1)

		var req embedTestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		resp := map[string]any{
			"data": []map[string]any{
				{"index": 1, "embedding": []float64{2.0, 2.5}},
				{"index": 0, "embedding": []float64{1.0, 1.5}},
			},
		}
		if len(req.Input) == 1 {
			resp = map[string]any{
				"data": []map[string]any{
					{"index": 0, "embedding": []float64{3.0, 3.5}},
				},
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := mistral.NewClient(server.URL, "test-key")
	client.BatchSize = 2
	client.MaxRetries = 1
	client.InitialBackoff = 1 * time.Millisecond
	client.MaxBackoff = 1 * time.Millisecond

	vectors, err := client.Embed(context.Background(), "mistral-embed", model.EmbedDocument, []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("embed failed: %v", err)
	}
	if len(vectors) != 3 {
		t.Fatalf("unexpected vector count: got %d want 3", len(vectors))
	}

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("unexpected call count: got %d want 2", got)
	}

	if vectors[0][0] != 1.0 || vectors[1][0] != 2.0 || vectors[2][0] != 3.0 {
		t.Fatalf("unexpected vector ordering: %#v", vectors)
	}
}

// TestEmbed_SymmetricProviderIgnoresRole pins SPEC 8.1.5: Mistral is a
// symmetric embedder, so the request it sends MUST be identical for
// EmbedDocument and EmbedQuery (the role is accepted and ignored).
func TestEmbed_SymmetricProviderIgnoresRole(t *testing.T) {
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedTestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		raw, _ := json.Marshal(req)
		bodies = append(bodies, string(raw))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float64{1, 2}}},
		})
	}))
	defer server.Close()

	client := mistral.NewClient(server.URL, "test-key")
	for _, role := range []model.EmbedRole{model.EmbedDocument, model.EmbedQuery} {
		if _, err := client.Embed(context.Background(), "mistral-embed", role, []string{"hello"}); err != nil {
			t.Fatalf("embed (%s): %v", role, err)
		}
	}
	if len(bodies) != 2 {
		t.Fatalf("want 2 requests, got %d", len(bodies))
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("symmetric provider must send identical request regardless of role:\n document=%s\n query=%s", bodies[0], bodies[1])
	}
}

func TestEmbed_RetriesOnRateLimitThenSucceeds(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limited"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"index": 0, "embedding": []float64{7.0}},
			},
		})
	}))
	defer server.Close()

	client := mistral.NewClient(server.URL, "test-key")
	client.BatchSize = 8
	client.MaxRetries = 2
	client.InitialBackoff = 1 * time.Millisecond
	client.MaxBackoff = 1 * time.Millisecond

	vectors, err := client.Embed(context.Background(), "mistral-embed", model.EmbedDocument, []string{"retry-me"})
	if err != nil {
		t.Fatalf("embed should have succeeded after retry: %v", err)
	}
	if len(vectors) != 1 || vectors[0][0] != 7.0 {
		t.Fatalf("unexpected vectors: %#v", vectors)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("unexpected call count: got %d want 2", got)
	}
}

func TestEmbed_RetriesOnServerErrorThenSucceeds(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("temporary upstream error"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"index": 0, "embedding": []float64{9.0, 9.5}},
			},
		})
	}))
	defer server.Close()

	client := mistral.NewClient(server.URL, "test-key")
	client.MaxRetries = 2
	client.InitialBackoff = 1 * time.Millisecond
	client.MaxBackoff = 1 * time.Millisecond

	vectors, err := client.Embed(context.Background(), "mistral-embed", model.EmbedDocument, []string{"retry-5xx"})
	if err != nil {
		t.Fatalf("embed should have succeeded after retry: %v", err)
	}
	if len(vectors) != 1 || vectors[0][0] != 9.0 {
		t.Fatalf("unexpected vectors: %#v", vectors)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("unexpected call count: got %d want 2", got)
	}
}

func TestEmbed_MapsAuthErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("invalid key"))
	}))
	defer server.Close()

	client := mistral.NewClient(server.URL, "bad-key")
	client.MaxRetries = 0

	_, err := client.Embed(context.Background(), "mistral-embed", model.EmbedDocument, []string{"x"})
	if err == nil {
		t.Fatal("expected error")
	}

	var providerErr *model.ProviderError
	if ok := errors.As(err, &providerErr); !ok {
		t.Fatalf("expected ProviderError, got %T", err)
	}
	if providerErr.Code != "MISTRAL_AUTH" {
		t.Fatalf("unexpected code: %s", providerErr.Code)
	}
	if providerErr.Retryable {
		t.Fatal("auth errors must not be retryable")
	}
}

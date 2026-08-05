package tests

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dirstral/dir2mcp/internal/mistral"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TestEmbed_RejectsZeroNormVector pins issue #703 at the Mistral provider
// boundary (the DEFAULT embed provider): an all-zero vector in an otherwise
// successful response is malformed output and must fail the call with a
// non-retryable provider error rather than being handed to the index.
func TestEmbed_RejectsZeroNormVector(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float64{0, 0, 0}}},
		})
	}))
	defer server.Close()

	c := mistral.NewClient(server.URL, "test-key")
	vecs, err := c.Embed(context.Background(), "mistral-embed", model.EmbedDocument, []string{"x"})
	if err == nil {
		t.Fatalf("Embed returned %v, want a malformed-output error for the zero vector", vecs)
	}
	var pErr *model.ProviderError
	if !errors.As(err, &pErr) {
		t.Fatalf("error %v is not a *model.ProviderError", err)
	}
	if pErr.Retryable {
		t.Fatalf("malformed provider output must be non-retryable: %v", pErr)
	}
}

// TestEmbed_HealthyVectorStillAccepted guards the other direction: the #703
// check must not reject a normal response. (Non-finite components are covered
// by the shared-validator suite in tests/model — encoding/json rejects a NaN/Inf
// literal before an adapter ever sees it, so that case cannot be staged here.)
func TestEmbed_HealthyVectorStillAccepted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float64{0, 0.0001}}},
		})
	}))
	defer server.Close()

	c := mistral.NewClient(server.URL, "test-key")
	vecs, err := c.Embed(context.Background(), "mistral-embed", model.EmbedDocument, []string{"x"})
	if err != nil {
		t.Fatalf("a tiny but non-zero vector must be accepted: %v", err)
	}
	if len(vecs) != 1 {
		t.Fatalf("vectors = %v, want 1", vecs)
	}
}

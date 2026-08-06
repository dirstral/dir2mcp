package tests

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// TestEmbed_RejectsZeroNormVector pins issue #703 at the Cohere provider
// boundary: an all-zero vector in a 200 OK response is malformed output. It has
// no cosine direction, so indexing it yields a chunk that scores 0 against every
// query while reporting embedding_status=embedded.
func TestEmbed_RejectsZeroNormVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": map[string]any{"float": [][]float64{{1, 0.5}, {0, 0}}},
		})
	}))
	defer srv.Close()

	c := newClient(srv.URL)
	vecs, err := c.Embed(context.Background(), "embed-v4.0", model.EmbedDocument, []string{"healthy", "zero"})
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

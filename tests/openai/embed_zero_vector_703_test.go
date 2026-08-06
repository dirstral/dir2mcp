package tests

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// TestEmbed_RejectsZeroNormVector pins issue #703 at the OpenAI provider
// boundary: a 200 OK response carrying an all-zero vector is malformed output,
// not a usable embedding. A zero vector has no cosine direction, so indexing it
// produces a chunk that scores 0 against every query yet reports
// embedding_status=embedded — a silent, permanent retrieval hole. The adapter
// must fail the call with a NON-RETRYABLE provider error instead of returning
// the vector.
func TestEmbed_RejectsZeroNormVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			vec := []float64{1, 0.5}
			if strings.Contains(req.Input[i], "zero") {
				vec = []float64{0, 0} // the malformed item
			}
			data[i] = map[string]any{"index": i, "embedding": vec}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()

	c := newClient(srv.URL)
	// Both the document (index) and query paths run through Embed, so a zero
	// vector is rejected on either side rather than producing all-zero scores.
	for _, role := range []model.EmbedRole{model.EmbedDocument, model.EmbedQuery} {
		vecs, err := c.Embed(context.Background(), "m", role, []string{"healthy", "zero"})
		if err == nil {
			t.Fatalf("role %v: Embed returned %v, want a malformed-output error for the zero vector", role, vecs)
		}
		var pErr *model.ProviderError
		if !errors.As(err, &pErr) {
			t.Fatalf("role %v: error %v is not a *model.ProviderError", role, err)
		}
		if pErr.Retryable {
			t.Fatalf("role %v: malformed provider output must be non-retryable: %v", role, pErr)
		}
	}

	// A healthy batch still succeeds (the check must not reject real vectors).
	if _, err := c.Embed(context.Background(), "m", model.EmbedDocument, []string{"healthy"}); err != nil {
		t.Fatalf("healthy batch: %v", err)
	}
}

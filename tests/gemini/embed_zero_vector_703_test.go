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

// TestEmbed_RejectsZeroNormVector pins issue #703 at the Gemini provider
// boundary. Gemini is the case that made the gap explicit: l2Normalize returns
// a zero vector UNCHANGED (there is no direction to scale), so a zero vector
// used to pass straight through the SPEC 8.1.6 normalization step and into the
// index.
func TestEmbed_RejectsZeroNormVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeNativeEmbed(t, r)
		embeddings := make([]map[string]any, len(req.Requests))
		for i := range req.Requests {
			embeddings[i] = map[string]any{"values": []float64{0, 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": embeddings})
	}))
	defer srv.Close()

	c := newClient(srv.URL)
	for _, role := range []model.EmbedRole{model.EmbedDocument, model.EmbedQuery} {
		vecs, err := c.Embed(context.Background(), "m", role, []string{"x"})
		if err == nil {
			t.Fatalf("role %v: Embed returned %v, want a malformed-output error", role, vecs)
		}
		var pErr *model.ProviderError
		if !errors.As(err, &pErr) {
			t.Fatalf("role %v: error %v is not a *model.ProviderError", role, err)
		}
		if pErr.Retryable {
			t.Fatalf("role %v: malformed provider output must be non-retryable: %v", role, pErr)
		}
	}

	// The same holds with a requested output dimension (SPEC 8.1.6): the
	// re-normalization step cannot rescue a zero vector.
	c.EmbedTextDim = 2
	if _, err := c.Embed(context.Background(), "m", model.EmbedDocument, []string{"x"}); err == nil {
		t.Fatal("a requested output dimension must not make a zero vector acceptable")
	}
}

// TestEmbed_RejectsShortVectorForRequestedDimension pins the dimension half of
// #703: when the operator requested an output dimensionality (8.1.6), a response
// that carries a different number of components is malformed. Accepting it would
// silently fix the corpus at a dimension nobody asked for.
func TestEmbed_RejectsShortVectorForRequestedDimension(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeNativeEmbed(t, r)
		embeddings := make([]map[string]any, len(req.Requests))
		for i := range req.Requests {
			embeddings[i] = map[string]any{"values": []float64{3, 4}} // 2 components
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": embeddings})
	}))
	defer srv.Close()

	c := newClient(srv.URL)
	c.EmbedTextDim = 8
	if _, err := c.Embed(context.Background(), "m", model.EmbedDocument, []string{"x"}); err == nil {
		t.Fatal("Embed accepted a 2-component vector for a requested dimension of 8")
	}
}

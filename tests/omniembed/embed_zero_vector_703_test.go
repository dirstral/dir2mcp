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

// TestEmbed_RejectsZeroNormVector pins issue #703 at the OmniEmbed provider
// boundary. Self-hosted servers are exactly where an all-zero "successful"
// vector shows up (a model that failed to load, an unsupported input silently
// producing zeros), and both the text and media paths must reject it: an
// undirected vector scores 0 against every query yet indexes as healthy.
func TestEmbed_RejectsZeroNormVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float64{0, 0, 0, 0}}},
		})
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")

	vecs, err := c.Embed(context.Background(), "omniembed", model.EmbedDocument, []string{"x"})
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

	// The media path shares the same response handling, so a zero media vector
	// is rejected identically (SPEC 8.1.7 puts both in ONE vector space).
	items := []model.MediaInput{{MimeType: "image/png", Data: []byte("PNGBYTES")}}
	if _, err := c.EmbedMedia(context.Background(), "omniembed", model.EmbedDocument, items); err == nil {
		t.Fatal("EmbedMedia accepted a zero-norm vector")
	}
}

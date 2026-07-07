package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/provider"
)

// probeStubEmbedder implements model.Embedder for the preflight-probe test: it
// records that it was called and returns the configured error (nil = success).
type probeStubEmbedder struct {
	called bool
	err    error
}

func (e *probeStubEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, inputs []string) ([][]float32, error) {
	e.called = true
	if e.err != nil {
		return nil, e.err
	}
	out := make([][]float32, len(inputs))
	for i := range inputs {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

// TestProbeEmbedProvider pins issue #399 item 3: a present-but-invalid embedding
// credential must be rejected by preflight (a definitive auth/config error
// blocks startup), while a transient/network failure is fail-open (server-first:
// a flaky link must not block a correctly-configured server) and a working
// credential passes.
func TestProbeEmbedProvider(t *testing.T) {
	a := &App{}
	prof := provider.Profile{Name: "test", EmbedTextModel: "some-embed-model"}

	t.Run("invalid credentials rejected", func(t *testing.T) {
		emb := &probeStubEmbedder{err: errors.New("401 unauthorized: invalid api key")}
		if err := a.probeEmbedProvider(emb, prof); err == nil {
			t.Fatal("probe accepted an invalid credential; want a non-nil error to block preflight")
		}
		if !emb.called {
			t.Fatal("probe did not exercise the embedder")
		}
	})

	t.Run("transient error fails open", func(t *testing.T) {
		emb := &probeStubEmbedder{err: errors.New("503 service unavailable")}
		if err := a.probeEmbedProvider(emb, prof); err != nil {
			t.Fatalf("transient error blocked preflight: %v; want fail-open (nil)", err)
		}
	})

	t.Run("valid credentials pass", func(t *testing.T) {
		emb := &probeStubEmbedder{}
		if err := a.probeEmbedProvider(emb, prof); err != nil {
			t.Fatalf("probe rejected a working embedder: %v", err)
		}
		if !emb.called {
			t.Fatal("probe did not exercise the embedder")
		}
	})

	t.Run("nil embedder is a no-op", func(t *testing.T) {
		if err := a.probeEmbedProvider(nil, prof); err != nil {
			t.Fatalf("nil embedder should be a no-op, got %v", err)
		}
	})
}

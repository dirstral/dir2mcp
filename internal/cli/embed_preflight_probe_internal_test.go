package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"syscall"
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

	t.Run("an unreachable local server fails open with a warning", func(t *testing.T) {
		// What the OpenAI-compatible adapter returns when nothing listens on
		// base_url: a retryable provider error wrapping ECONNREFUSED. It used
		// to block startup with advice to set an API key.
		var stderr bytes.Buffer
		aw := &App{stderr: &stderr}
		local := provider.Profile{Name: "local", BaseURL: "http://127.0.0.1:11434/v1", EmbedTextModel: "nomic-embed-text"}
		emb := &probeStubEmbedder{err: &model.ProviderError{Code: "OPENAI_FAILED", Message: "request failed", Retryable: true, Cause: syscall.ECONNREFUSED}}
		if err := aw.probeEmbedProvider(emb, local); err != nil {
			t.Fatalf("an unreachable local server blocked startup: %v", err)
		}
		if !strings.Contains(stderr.String(), "http://127.0.0.1:11434/v1") || !strings.Contains(stderr.String(), "is running") || !strings.Contains(stderr.String(), "connection refused") {
			t.Errorf("warning must name the endpoint and suggest checking the server, got %q", stderr.String())
		}
	})

	t.Run("a non-retryable provider error still blocks", func(t *testing.T) {
		emb := &probeStubEmbedder{err: &model.ProviderError{Code: "OPENAI_AUTH", Message: "invalid api key", Retryable: false, StatusCode: 401}}
		if err := a.probeEmbedProvider(emb, prof); err == nil {
			t.Fatal("a 401 passed preflight")
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

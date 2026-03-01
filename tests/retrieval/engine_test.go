package tests

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dir2mcp/internal/config"
	"dir2mcp/internal/retrieval"
)

func TestEngineAsk_WithEmptyIndexReturnsFallback(t *testing.T) {
	server := newFakeMistralEmbeddingServer()
	t.Cleanup(server.Close)

	stateDir := t.TempDir()
	rootDir := t.TempDir()

	cfg := config.Default()
	cfg.MistralAPIKey = "test-api-key"
	cfg.MistralBaseURL = server.URL

	engine, err := retrieval.NewEngine(context.Background(), stateDir, rootDir, &cfg)
	if err != nil {
		t.Fatalf("NewEngine failed: %v", err)
	}
	t.Cleanup(engine.Close)

	result, err := engine.AskWithContext(context.Background(), "what changed?", retrieval.AskOptions{K: 3})
	if err != nil {
		t.Fatalf("Ask failed: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil AskResult")
	}
	if !strings.Contains(result.Answer, "No relevant context") {
		t.Fatalf("expected empty-context fallback answer, got %q", result.Answer)
	}
	if len(result.Citations) != 0 {
		t.Fatalf("expected no citations for empty index, got %#v", result.Citations)
	}
}

func newTestEngine(t *testing.T) *retrieval.Engine {
	t.Helper()

	// spin up a fake mistral embeddings service for the engine to call
	server := newFakeMistralEmbeddingServer()
	t.Cleanup(server.Close)

	stateDir := t.TempDir()
	rootDir := t.TempDir()

	cfg := config.Default()
	cfg.MistralAPIKey = "test-api-key"
	cfg.MistralBaseURL = server.URL

	engine, err := retrieval.NewEngine(context.Background(), stateDir, rootDir, &cfg)
	if err != nil {
		t.Fatalf("NewEngine failed: %v", err)
	}
	// Close currently returns no error, so we can pass it directly to Cleanup
	t.Cleanup(engine.Close)

	return engine
}

func TestEngineAsk_WithTODOContext(t *testing.T) {
	engine := newTestEngine(t)

	result, err := engine.AskWithContext(context.TODO(), "what changed?", retrieval.AskOptions{K: 1})
	if err != nil {
		t.Fatalf("AskWithContext(context.TODO()) failed: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil AskResult")
	}
}

func TestEngineAsk_RejectsEmptyQuestion(t *testing.T) {
	engine := newTestEngine(t)

	_, err := engine.AskWithContext(context.Background(), "   ", retrieval.AskOptions{})
	if err == nil {
		t.Fatal("expected validation error for empty question")
	}
	if !strings.Contains(err.Error(), "question is required") {
		t.Fatalf("unexpected error for empty question: %v", err)
	}
}

func TestEngineAsk_ZeroValueEngineReportsNotInitialized(t *testing.T) {
	var engine retrieval.Engine
	_, err := engine.AskWithContext(context.Background(), "q", retrieval.AskOptions{})
	if err == nil {
		t.Fatal("expected error from zero-value engine")
	}
	if !strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("unexpected zero-value engine error: %v", err)
	}
}

func TestEngineAsk_ContextCanceled(t *testing.T) {
	engine := newTestEngine(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := engine.AskWithContext(ctx, "foo", retrieval.AskOptions{})
	if err == nil {
		t.Fatal("expected error when context already canceled")
	}
	// we should wrap the cancellation with a clear message; the caller may also
	// see a deadline exceeded or the underlying context.Canceled, but wrapping
	// allows callers to inspect the error text if they don't use errors.Is.
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	if !strings.Contains(err.Error(), "ask canceled") {
		t.Fatalf("expected wrapped cancellation message, got: %v", err)
	}
}

func TestEngine_RuntimeRetrievalTunables(t *testing.T) {
	engine := newTestEngine(t)
	engine.SetSystemPrompt("Use concise output")
	engine.SetMaxContextChars(128)
	engine.SetOversampleFactor(2)

	result, err := engine.AskWithContext(context.Background(), "what changed?", retrieval.AskOptions{K: 1})
	if err != nil {
		t.Fatalf("AskWithContext failed: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil AskResult")
	}
}

func newFakeMistralEmbeddingServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		type item struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		}

		data := make([]item, 0, len(req.Input))
		for i := range req.Input {
			data = append(data, item{Index: i, Embedding: []float64{1, 0}})
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": data})
	}))
}

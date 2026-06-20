package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dirstral/dir2mcp/internal/mistral"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/usage"
)

// TestGenerate_ReportsTokenUsage verifies the Mistral generate path parses the
// OpenAI-style `usage` object and reports it into a context-attached sink.
func TestGenerate_ReportsTokenUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "hi"}},
			},
			"usage": map[string]any{
				"prompt_tokens":     123,
				"completion_tokens": 45,
				"total_tokens":      168,
			},
		})
	}))
	defer server.Close()

	sink := usage.NewSink()
	ctx := usage.WithSink(context.Background(), sink)
	client := mistral.NewClient(server.URL, "test-key")
	if _, err := client.Generate(ctx, "prompt"); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	u, ok := sink.Stage(usage.StageGenerate)
	if !ok {
		t.Fatal("expected generate usage to be reported")
	}
	if u.PromptTokens != 123 || u.CompletionTokens != 45 || u.TotalTokens != 168 {
		t.Fatalf("usage mismatch: %+v", u)
	}
}

// TestEmbed_ReportsTokenUsage verifies the embed path reports prompt-token usage.
func TestEmbed_ReportsTokenUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"index": 0, "embedding": []float64{0.1, 0.2}},
			},
			"usage": map[string]any{"prompt_tokens": 7, "total_tokens": 7},
		})
	}))
	defer server.Close()

	sink := usage.NewSink()
	ctx := usage.WithSink(context.Background(), sink)
	client := mistral.NewClient(server.URL, "test-key")
	if _, err := client.Embed(ctx, "mistral-embed", model.EmbedQuery, []string{"q"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}

	u, ok := sink.Stage(usage.StageEmbed)
	if !ok {
		t.Fatal("expected embed usage to be reported")
	}
	if u.PromptTokens != 7 {
		t.Fatalf("embed prompt tokens=%d, want 7", u.PromptTokens)
	}
}

// TestGenerate_NoUsageNoReport verifies that a response without a usage object
// leaves the sink empty (unknown, not zero) and Generate still succeeds.
func TestGenerate_NoUsageNoReport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "hi"}},
			},
		})
	}))
	defer server.Close()

	sink := usage.NewSink()
	ctx := usage.WithSink(context.Background(), sink)
	client := mistral.NewClient(server.URL, "test-key")
	if _, err := client.Generate(ctx, "prompt"); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, ok := sink.Stage(usage.StageGenerate); ok {
		t.Fatal("no usage object => stage must remain unknown")
	}
}

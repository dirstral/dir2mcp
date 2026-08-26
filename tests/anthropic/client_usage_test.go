package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dirstral/dir2mcp/internal/usage"
)

// TestGenerate_ReportsTokenUsage verifies the Anthropic generate path parses
// the Messages API `usage` object (input_tokens/output_tokens) and reports it
// into a context-attached sink as generate-stage usage (issue #672).
func TestGenerate_ReportsTokenUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "hi"},
			},
			"usage": map[string]any{
				"input_tokens":  123,
				"output_tokens": 45,
			},
		})
	}))
	defer srv.Close()

	sink := usage.NewSink()
	ctx := usage.WithSink(context.Background(), sink)
	if _, err := newClient(srv.URL).Generate(ctx, "prompt"); err != nil {
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

// TestGenerate_NoUsageNoReport verifies that a response without a usage
// object leaves the sink empty (unknown, not zero) and Generate still
// succeeds. This pins graceful degradation when usage fields are missing.
func TestGenerate_NoUsageNoReport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "hi"},
			},
		})
	}))
	defer srv.Close()

	sink := usage.NewSink()
	ctx := usage.WithSink(context.Background(), sink)
	if _, err := newClient(srv.URL).Generate(ctx, "prompt"); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, ok := sink.Stage(usage.StageGenerate); ok {
		t.Fatal("no usage object => stage must remain unknown")
	}
}

// TestGenerate_NoSinkStillSucceeds verifies the report path is inert when no
// sink is attached to the context.
func TestGenerate_NoSinkStillSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "hi"},
			},
			"usage": map[string]any{
				"input_tokens":  7,
				"output_tokens": 3,
			},
		})
	}))
	defer srv.Close()

	out, err := newClient(srv.URL).Generate(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if out != "hi" {
		t.Fatalf("text = %q, want %q", out, "hi")
	}
}

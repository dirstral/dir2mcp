package tests

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
)

func clearProviderEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"MISTRAL_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY",
		"GEMINI_API_KEY", "COHERE_API_KEY", "ANTHROPIC_API_KEY", "ELEVENLABS_API_KEY",
	} {
		t.Setenv(k, "")
	}
}

// No embed provider resolves -> generalized CONFIG_INVALID preflight.
func TestAskPreflight_NoProviderRejected(t *testing.T) {
	tmp := t.TempDir()
	clearProviderEnv(t)
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{})
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(),
			[]string{"ask", "--non-interactive", "--mode", "search_only", "q"})
		if code == 0 {
			t.Fatalf("expected non-zero exit, stderr=%s", stderr.String())
		}
	})
	if !strings.Contains(stderr.String(), "no embedding provider configured") {
		t.Fatalf("stderr should mention no embedding provider; got: %s", stderr.String())
	}
}

// A non-Mistral embed credential satisfies the preflight (no longer
// Mistral-specific): the command proceeds past it and search succeeds.
func TestAskPreflight_NonMistralCredentialPasses(t *testing.T) {
	tmp := t.TempDir()
	clearProviderEnv(t)
	t.Setenv("OPENAI_API_KEY", "ok")

	stub := &commandTestRetrieverStub{
		searchHits: []model.SearchHit{
			{ChunkID: 1, RelPath: "a.md", DocType: "md", RepType: "raw_text", Score: 0.9, Snippet: "alpha"},
		},
	}
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewStore:     func(config.Config) model.Store { return &commandTestNoopStore{} },
		NewRetriever: func(config.Config, model.Store) model.Retriever { return stub },
	})
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(),
			[]string{"ask", "--non-interactive", "--mode", "search_only", "alpha"})
		if code != 0 {
			t.Fatalf("non-Mistral credential should pass preflight; code=%d stderr=%s", code, stderr.String())
		}
	})
	if strings.Contains(stderr.String(), "no embedding provider configured") {
		t.Fatalf("preflight wrongly rejected a non-Mistral credential: %s", stderr.String())
	}
}

// An explicit, incapable embed binding surfaces the resolver's
// actionable *provider.ConfigError verbatim (not the generic message).
func TestAskPreflight_IncapableExplicitBindingSurfacesConfigError(t *testing.T) {
	tmp := t.TempDir()
	clearProviderEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "ak")
	if err := os.WriteFile(filepath.Join(tmp, ".dir2mcp.yaml"),
		[]byte("model:\n  embed:\n    provider: anthropic\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{})
	withWorkingDir(t, tmp, func() {
		if code := app.RunWithContext(context.Background(),
			[]string{"ask", "--non-interactive", "--mode", "search_only", "q"}); code == 0 {
			t.Fatalf("incapable explicit embed binding must fail, stderr=%s", stderr.String())
		}
	})
	out := stderr.String()
	if !strings.Contains(out, "CONFIG_INVALID") || !strings.Contains(out, "embed") {
		t.Fatalf("expected the resolver ConfigError surfaced; got: %s", out)
	}
}

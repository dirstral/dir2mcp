package tests

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// The local ask path builds its retrieval service from the corpus config. Before
// spec 0.93.0 no production site installed security.secret_patterns on it, so
// open_file applied only the built-in defaults. This test runs the real `ask`
// command, keeps the service it built (RuntimeHooks.WrapRetriever), and checks
// that both operator lists reach it. The wrapper answers the question itself,
// so no model provider is called.

// runAskAndKeepRetriever runs `ask` in dir and returns the retrieval service
// the command built. The corpus root is the working directory, so check runs
// there too.
func runAskAndKeepRetriever(t *testing.T, dir string, check func(model.Retriever)) (model.Retriever, int, string) {
	t.Helper()
	var built model.Retriever
	stub := &commandTestRetrieverStub{askResult: model.AskResult{Question: "q", Answer: "a"}}
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewStore: func(config.Config) model.Store { return &commandTestNoopStore{} },
		WrapRetriever: func(r model.Retriever) model.Retriever {
			built = r
			return stub
		},
	})
	var code int
	withWorkingDir(t, dir, func() {
		code = app.RunWithContext(context.Background(), []string{"ask", "q"})
		if check != nil && code == 0 && built != nil {
			check(built)
		}
	})
	return built, code, stderr.String()
}

func TestAskLocal_RetrieverAppliesTheOperatorSecretPolicy(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "test-key")
	dir := testutil.TempDir(t)
	tok := "to" + "ken"
	link := "https://www.youtube.com/redirect?event=video_description&redir_" + tok + "=QUFFLUhqbkJ3T2xYbE5fZ3lQdmZ4&q=x"
	files := map[string]string{
		"operator.txt": "ticket ACME-SECRET-4711 for the vault\n",
		"link.txt":     "source " + link + "\n",
		"clean.txt":    "nothing to hide here\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	// An operator rule, and an empty allow list: the redirect link is then a
	// generic token match that nothing cancels.
	writeAskCorpusConfig(t, dir, ""+
		"security:\n"+
		"  secret_patterns:\n"+
		"    - 'ACME-SECRET-[0-9]{4}'\n"+
		"  secret_allow_patterns: []\n")

	ctx := context.Background()
	built, code, stderr := runAskAndKeepRetriever(t, dir, func(r model.Retriever) {
		for _, name := range []string{"operator.txt", "link.txt"} {
			if _, err := r.OpenFile(ctx, name, model.Span{}, 200); !errors.Is(err, model.ErrForbidden) {
				t.Errorf("open_file %s: err = %v, want ErrForbidden", name, err)
			}
		}
		got, err := r.OpenFile(ctx, "clean.txt", model.Span{}, 200)
		if err != nil || !strings.Contains(got, "nothing to hide") {
			t.Errorf("open_file clean.txt = (%q, %v), want its text", got, err)
		}
	})
	if code != 0 {
		t.Fatalf("ask exit=%d stderr=%s", code, stderr)
	}
	if built == nil {
		t.Fatal("the ask path did not build a retriever")
	}

	// With the default allow list the same redirect link is served.
	writeAskCorpusConfig(t, dir, ""+
		"security:\n"+
		"  secret_patterns:\n"+
		"    - 'ACME-SECRET-[0-9]{4}'\n")
	_, code, stderr = runAskAndKeepRetriever(t, dir, func(r model.Retriever) {
		if got, err := r.OpenFile(ctx, "link.txt", model.Span{}, 200); err != nil || !strings.Contains(got, "source") {
			t.Errorf("open_file link.txt with the default allow list = (%q, %v), want its text", got, err)
		}
	})
	if code != 0 {
		t.Fatalf("ask exit=%d stderr=%s", code, stderr)
	}
}

func TestAskLocal_InvalidSecretAllowPatternIsConfigInvalid(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "test-key")
	dir := testutil.TempDir(t)
	writeAskCorpusConfig(t, dir, ""+
		"security:\n"+
		"  secret_allow_patterns:\n"+
		"    - '(unclosed'\n")
	built, code, stderr := runAskAndKeepRetriever(t, dir, nil)
	if code == 0 {
		t.Fatal("ask succeeded with an allow pattern that does not compile")
	}
	if built != nil {
		t.Fatal("ask built a retriever from an invalid config")
	}
	if !strings.Contains(stderr, "security.secret_allow_patterns") {
		t.Fatalf("stderr does not name security.secret_allow_patterns: %s", stderr)
	}
}

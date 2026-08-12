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

// writeAskCorpusConfig writes a .dir2mcp.yaml carrying the given rag lines into
// dir, so the local ask path loads a real corpus configuration.
func writeAskCorpusConfig(t *testing.T, dir, ragLines string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".dir2mcp.yaml"), []byte(ragLines), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// TestAskLocal_OmittedKUsesConfiguredKDefault pins the CLI half of the SPEC §9.1
// scope: rag.k_default applies to "any CLI surface over" the k-bearing tools.
//
// On main the ask command substituted a client-side constant for an omitted -k,
// so the corpus's configured default never reached retrieval.
func TestAskLocal_OmittedKUsesConfiguredKDefault(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "test-key")
	const configuredK = 23

	tmp := t.TempDir()
	writeAskCorpusConfig(t, tmp, "rag_k_default: 23\n")

	stub := &commandTestRetrieverStub{askResult: model.AskResult{Question: "q", Answer: "a"}}
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewStore:     func(config.Config) model.Store { return &commandTestNoopStore{} },
		NewRetriever: func(config.Config, model.Store) model.Retriever { return stub },
	})

	withWorkingDir(t, tmp, func() {
		if code := app.RunWithContext(context.Background(), []string{"ask", "q"}); code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, stderr.String())
		}
	})
	if !stub.askCalled {
		t.Fatal("expected Ask to be called")
	}
	if stub.lastAskQuery.K != configuredK {
		t.Fatalf("k=%d want the configured rag.k_default=%d", stub.lastAskQuery.K, configuredK)
	}
}

// TestAskLocal_SuppliedKWinsOverConfiguredKDefault keeps step 1 of the
// precedence intact on the CLI: an explicit -k is the caller's request and the
// configured default must not replace it.
func TestAskLocal_SuppliedKWinsOverConfiguredKDefault(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "test-key")

	tmp := t.TempDir()
	writeAskCorpusConfig(t, tmp, "rag_k_default: 23\n")

	stub := &commandTestRetrieverStub{askResult: model.AskResult{Question: "q", Answer: "a"}}
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewStore:     func(config.Config) model.Store { return &commandTestNoopStore{} },
		NewRetriever: func(config.Config, model.Store) model.Retriever { return stub },
	})

	withWorkingDir(t, tmp, func() {
		if code := app.RunWithContext(context.Background(), []string{"ask", "--k", "4", "q"}); code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, stderr.String())
		}
	})
	if stub.lastAskQuery.K != 4 {
		t.Fatalf("k=%d want the supplied 4", stub.lastAskQuery.K)
	}
}

// TestAskLocal_GenerateAnswerFalseIsServedAsSearchOnly pins SPEC §9.4 on the CLI:
// `rag.generate_answer: false` withholds generation whatever the request asks,
// so `--mode answer` returns the hits with an empty answer instead of calling a
// chat provider.
func TestAskLocal_GenerateAnswerFalseIsServedAsSearchOnly(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "test-key")

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "mode omitted", args: []string{"ask", "--json", "q"}},
		{name: "mode=answer", args: []string{"ask", "--json", "--mode", "answer", "q"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			writeAskCorpusConfig(t, tmp, "rag_generate_answer: false\n")

			stub := &commandTestRetrieverStub{
				askResult:  model.AskResult{Question: "q", Answer: "generated answer"},
				searchHits: []model.SearchHit{{ChunkID: 1, RelPath: "a.md", Snippet: "alpha"}},
			}
			var stdout, stderr bytes.Buffer
			app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
				NewStore:     func(config.Config) model.Store { return &commandTestNoopStore{} },
				NewRetriever: func(config.Config, model.Store) model.Retriever { return stub },
			})

			withWorkingDir(t, tmp, func() {
				if code := app.RunWithContext(context.Background(), tc.args); code != 0 {
					t.Fatalf("exit=%d stderr=%s", code, stderr.String())
				}
			})
			if stub.askCalled {
				t.Fatal("Ask ran with rag.generate_answer: false")
			}
			if !stub.searchCalled {
				t.Fatal("retrieval did not run: generation is disabled, search is not")
			}
			out := stdout.String()
			if !strings.Contains(out, `"answer":""`) {
				t.Fatalf("expected an empty answer in the payload, got: %s", out)
			}
			if !strings.Contains(out, `"rel_path":"a.md"`) {
				t.Fatalf("expected the hits in the payload, got: %s", out)
			}
		})
	}
}

package tests

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/mcp"
)

// writeStubPandoc writes an executable shell script that echoes fixed Markdown to
// stdout, standing in for the pandoc binary on the annotation path. Its basename
// is deliberately NOT "pandoc" so the engine's `--version` functional probe is
// skipped (a custom ingest.pandoc.command wrapper is trusted as-is), letting the
// stub activate as the pandoc engine without a real pandoc install.
func writeStubPandoc(t *testing.T, markdown string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "stubpandoc.sh")
	// The extractor invokes `<script> <tmpfile> -t gfm`; ignore the args and emit
	// the fixed Markdown so the on-demand annotation source is deterministic.
	body := "#!/bin/sh\ncat <<'PANDOC_EOF'\n" + markdown + "\nPANDOC_EOF\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub pandoc: %v", err)
	}
	return script
}

// TestMCPToolsCallAnnotate_ODTRoutesThroughPandoc is the regression for #585: a
// born-digital .odt document annotated on demand must have its source text
// produced by the capability-activated pandoc engine (T2, #393), not the primary
// docling/mistral extractor. The stubbed pandoc emits a distinctive marker; the
// annotation prompt sent to the model must contain it, proving the source text
// came from pandoc rather than an extraction error/empty text.
func TestMCPToolsCallAnnotate_ODTRoutesThroughPandoc(t *testing.T) {
	const marker = "PANDOC_ODT_SOURCE_MARKER extracted body text"
	cfg, st, _ := setupMCPToolStore(t, "report.odt", "document", []byte("PK\x03\x04 fake odt bytes"))

	var mu sync.Mutex
	var lastPrompt string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		mu.Lock()
		lastPrompt = string(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"summary\":\"ok\"}"}}]}`)
	}))
	defer upstream.Close()

	cfg = withMistralUpstream(t, cfg, "mistral", upstream.URL)
	// withMistralUpstream reloads config from a fresh file (defaults: extractor
	// auto); point the pandoc engine at the stub so .odt activates+routes through it.
	cfg.IngestPandocCommand = writeStubPandoc(t, marker)

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":85,"method":"tools/call","params":{"name":"dir2mcp_annotate","arguments":{"rel_path":"report.odt","schema_json":{"type":"object","properties":{"summary":{"type":"string"}}}}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}

	mu.Lock()
	prompt := lastPrompt
	mu.Unlock()
	if prompt == "" {
		t.Fatal("model was never called: annotation source text was not produced (pandoc route not taken)")
	}
	if !strings.Contains(prompt, marker) {
		t.Fatalf("annotation prompt did not contain the pandoc-extracted marker %q; got prompt body: %s", marker, prompt)
	}

	// The pandoc extraction was cached (not refused), so a cache entry remains.
	if entries := pandocCacheEntries(t, cfg.StateDir); len(entries) == 0 {
		t.Fatal("expected a pandoc cache entry after a successful .odt annotation")
	}
}

// TestMCPToolsCallAnnotate_ODTPandocSecretGatePurgesCache covers the #407
// secret-gate on the pandoc annotation path: when the pandoc-extracted text
// matches a secret pattern the tool must refuse (FORBIDDEN) AND the refused
// extraction must not be left persisted in the pandoc cache.
func TestMCPToolsCallAnnotate_ODTPandocSecretGatePurgesCache(t *testing.T) {
	const secret = "AKIA_PANDOC_SECRET_TOKEN"
	cfg, st, _ := setupMCPToolStore(t, "report.odt", "document", []byte("PK\x03\x04 fake odt bytes"))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The secret gate refuses before any model call; fail loudly if reached.
		t.Errorf("model must not be called when the source text is refused")
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	cfg = withMistralUpstream(t, cfg, "mistral", upstream.URL)
	cfg.IngestPandocCommand = writeStubPandoc(t, "clearance note: "+secret)
	cfg.SecretPatterns = []string{"AKIA_[A-Z_]+"}

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":86,"method":"tools/call","params":{"name":"dir2mcp_annotate","arguments":{"rel_path":"report.odt","schema_json":{"type":"object","properties":{"summary":{"type":"string"}}}}}}`)
	defer func() { _ = resp.Body.Close() }()
	assertToolCallErrorCode(t, resp, "FORBIDDEN")

	if entries := pandocCacheEntries(t, cfg.StateDir); len(entries) != 0 {
		t.Fatalf("refused pandoc extraction left cache entries behind: %v", entries)
	}
}

// pandocCacheEntries lists the .md files in the pandoc cache dir for stateDir.
func pandocCacheEntries(t *testing.T, stateDir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(stateDir, "cache", "pandoc", "*.md"))
	if err != nil {
		t.Fatalf("glob pandoc cache: %v", err)
	}
	return matches
}

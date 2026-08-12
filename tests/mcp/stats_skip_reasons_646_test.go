package tests

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/store"
)

// canonicalSkipReasons is the closed skip-reason vocabulary the canonical
// stats.json pins for this spec minor. The test reads it from the pinned
// dirstral-spec submodule when the checkout is present, so a spec-side change
// cannot pass unnoticed, and falls back to the model constants otherwise (a
// shallow clone without submodules still runs the rest of the assertions).
func canonicalSkipReasons(t *testing.T) map[string]bool {
	t.Helper()
	fallback := map[string]bool{}
	for _, reason := range []string{
		model.SkipReasonUnsupportedFormat, model.SkipReasonBinaryIgnored,
		model.SkipReasonArchive, model.SkipReasonIgnoreRule,
		model.SkipReasonSecretExcluded, model.SkipReasonPathExcluded,
		model.SkipReasonSizeCap, model.SkipReasonLanguageUncovered,
		model.SkipReasonSymlinkIgnored,
	} {
		fallback[reason] = true
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "dirstral-spec", "spec", "tools", "schemas", "stats.json"))
	if err != nil {
		t.Logf("canonical stats.json unavailable (%v); using the model skip-reason constants", err)
		return fallback
	}
	var doc struct {
		Definitions struct {
			Output struct {
				Properties struct {
					SkipReasons struct {
						Items struct {
							Properties struct {
								Reason struct {
									Enum []string `json:"enum"`
								} `json:"reason"`
							} `json:"properties"`
						} `json:"items"`
					} `json:"skip_reasons"`
				} `json:"properties"`
			} `json:"output"`
		} `json:"definitions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode canonical stats.json: %v", err)
	}
	enum := doc.Definitions.Output.Properties.SkipReasons.Items.Properties.Reason.Enum
	if len(enum) == 0 {
		t.Fatal("canonical stats.json declares no skip_reasons reason enum")
	}
	out := make(map[string]bool, len(enum))
	for _, reason := range enum {
		out[reason] = true
	}
	return out
}

// statsServerOverStore boots an MCP server whose retriever is a real retrieval
// service over st, so the stats payload travels the production chain: store
// aggregate -> retrieval.Stats -> tool serialization.
func statsServerOverStore(t *testing.T, tmp string, st model.Store) (*httptest.Server, config.Config) {
	t.Helper()
	cfg := config.Default()
	cfg.StateDir = tmp
	cfg.MCPPath = protocol.DefaultMCPPath
	cfg.AuthMode = "none"

	retriever := retrieval.NewService(st, nil, nil, nil)
	srv := httptest.NewServer(mcp.NewServer(cfg, retriever, mcp.WithStore(st)).Handler())
	t.Cleanup(srv.Close)
	return srv, cfg
}

func newSkipReasonStore(t *testing.T) (*store.SQLiteStore, string) {
	t.Helper()
	tmp := t.TempDir()
	st := store.NewSQLiteStore(filepath.Join(tmp, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, tmp
}

// toolOutputSchemaFromList returns the outputSchema the server advertises for
// one tool, which is the schema a strict client validates responses against.
func toolOutputSchemaFromList(t *testing.T, mcpURL, toolName string) map[string]interface{} {
	t.Helper()
	sessionID := initializeSession(t, mcpURL)
	resp := postRPC(t, mcpURL, sessionID, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	defer func() { _ = resp.Body.Close() }()
	var envelope struct {
		Result struct {
			Tools []struct {
				Name         string                 `json:"name"`
				OutputSchema map[string]interface{} `json:"outputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	for _, tool := range envelope.Result.Tools {
		if tool.Name == toolName {
			return tool.OutputSchema
		}
	}
	t.Fatalf("tool %q not advertised", toolName)
	return nil
}

// keysOf returns the sorted key set of a decoded JSON object, for readable
// failure messages.
func keysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// decodeSkipReasons validates the emitted skip_reasons array against the
// canonical item contract (reason in the closed enum, count >= 1, no extra keys)
// and returns the per-reason counts.
func decodeSkipReasons(t *testing.T, raw interface{}) map[string]float64 {
	t.Helper()
	entries, ok := raw.([]interface{})
	if !ok {
		t.Fatalf("skip_reasons missing or not an array: %#v", raw)
	}
	allowed := canonicalSkipReasons(t)
	counts := map[string]float64{}
	for _, item := range entries {
		entry, ok := item.(map[string]interface{})
		if !ok {
			t.Fatalf("skip_reasons entry is not an object: %#v", item)
		}
		if len(entry) != 2 {
			t.Errorf("skip_reasons entry carries extra keys (canonical items are additionalProperties:false): %#v", entry)
		}
		reason, _ := entry["reason"].(string)
		if reason == "" {
			t.Fatalf("skip_reasons entry has no reason string: %#v", entry)
		}
		if !allowed[reason] {
			t.Errorf("reason %q is outside the canonical enum %v", reason, allowed)
		}
		count, ok := entry["count"].(float64)
		if !ok || count < 1 {
			t.Fatalf("skip_reasons %q count must be an integer >= 1, got %#v", reason, entry["count"])
		}
		counts[reason] = count
	}
	return counts
}

// TestMCPToolsCallStats_SkipReasonsReportedFromStore pins issue #646: the store
// persists a durable skip aggregate, so dir2mcp_stats MUST serialize it as the
// canonical skip_reasons array (SPEC §15.6 / stats.json).
//
// A client that trusts doc_counts alone reads a corpus with unindexable files as
// fully covered, then answers over evidence that was never indexed. skip_reasons
// is the field that says what is missing and why.
func TestMCPToolsCallStats_SkipReasonsReportedFromStore(t *testing.T) {
	st, tmp := newSkipReasonStore(t)

	seeded := []model.Document{
		{RelPath: "a.odt", DocType: "document", Status: "skipped", SkipReason: model.SkipReasonUnsupportedFormat},
		{RelPath: "b.rtf", DocType: "document", Status: "skipped", SkipReason: model.SkipReasonUnsupportedFormat},
		{RelPath: "c.zip", DocType: "archive", Status: "skipped", SkipReason: model.SkipReasonArchive},
		{RelPath: "d.md", DocType: "md", Status: "ok"},
		{RelPath: "e.pdf", DocType: "pdf", Status: "error", ErrorMessage: "extract failed"},
	}
	for _, doc := range seeded {
		if err := st.UpsertDocument(context.Background(), doc); err != nil {
			t.Fatalf("seed %s: %v", doc.RelPath, err)
		}
	}

	srv, cfg := statsServerOverStore(t, tmp, st)
	sc := callStatsTool(t, srv.URL+cfg.MCPPath)

	counts := decodeSkipReasons(t, sc["skip_reasons"])
	if counts[model.SkipReasonUnsupportedFormat] != 2 {
		t.Errorf("unsupported_format count = %v, want 2", counts[model.SkipReasonUnsupportedFormat])
	}
	if counts[model.SkipReasonArchive] != 1 {
		t.Errorf("archive count = %v, want 1", counts[model.SkipReasonArchive])
	}

	// SPEC §15.6: indexing.skipped equals the sum of skip_reasons[].count.
	indexing, ok := sc["indexing"].(map[string]interface{})
	if !ok {
		t.Fatalf("indexing missing: %#v", sc["indexing"])
	}
	var total float64
	for _, count := range counts {
		total += count
	}
	if skipped, _ := indexing["skipped"].(float64); skipped != total {
		t.Errorf("indexing.skipped = %v, want the skip_reasons total %v", indexing["skipped"], total)
	}
}

// TestMCPToolsCallStats_SkipReasonsOmittedWhenNothingSkipped pins the other
// branch of SPEC §15.6: a corpus with nothing skipped omits the field, so
// "present" always means real coverage gaps exist.
func TestMCPToolsCallStats_SkipReasonsOmittedWhenNothingSkipped(t *testing.T) {
	st, tmp := newSkipReasonStore(t)
	if err := st.UpsertDocument(context.Background(), model.Document{
		RelPath: "ok.md", DocType: "md", Status: "ok",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	srv, cfg := statsServerOverStore(t, tmp, st)
	sc := callStatsTool(t, srv.URL+cfg.MCPPath)
	if _, present := sc["skip_reasons"]; present {
		t.Errorf("skip_reasons must be omitted when nothing was skipped, got %#v", sc["skip_reasons"])
	}
}

// TestMCPToolsCallStats_SkipReasonsAdvertisedInOutputSchema pins the advertised
// half of the contract: a strict client validates structuredContent against the
// outputSchema it read from tools/list, and that schema sets
// additionalProperties:false. An emitted-but-undeclared skip_reasons would make
// every response with coverage gaps invalid.
func TestMCPToolsCallStats_SkipReasonsAdvertisedInOutputSchema(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	srv := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer srv.Close()

	schema := toolOutputSchemaFromList(t, srv.URL+cfg.MCPPath, protocol.ToolNameStats)
	properties, ok := schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("stats outputSchema has no properties: %#v", schema)
	}
	skipReasons, ok := properties["skip_reasons"].(map[string]interface{})
	if !ok {
		t.Fatalf("stats outputSchema does not declare skip_reasons: %v", keysOf(properties))
	}
	items, ok := skipReasons["items"].(map[string]interface{})
	if !ok {
		t.Fatalf("skip_reasons declares no item schema: %#v", skipReasons)
	}
	itemProps, ok := items["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("skip_reasons item schema has no properties: %#v", items)
	}
	for _, field := range []string{"reason", "count"} {
		if _, ok := itemProps[field]; !ok {
			t.Errorf("skip_reasons item schema omits %q: %v", field, keysOf(itemProps))
		}
	}
}

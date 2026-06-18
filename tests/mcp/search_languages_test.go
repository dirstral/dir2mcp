package tests

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TestMCPSearch_LanguagesFilterPassedThrough verifies dir2mcp_search accepts the
// optional languages filter (SPEC §9.5/§15.2) and forwards it verbatim to the
// retriever's SearchQuery.
func TestMCPSearch_LanguagesFilterPassedThrough(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	var got []string
	retriever := &askAudioRetrieverStub{
		indexingComplete: true,
		OnSearch: func(q model.SearchQuery) ([]model.SearchHit, error) {
			got = q.Languages
			return []model.SearchHit{}, nil
		},
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_search","arguments":{"query":"q","languages":["pt-BR","es"]}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(payload))
	}
	if len(got) != 2 || got[0] != "pt-BR" || got[1] != "es" {
		t.Fatalf("languages filter not forwarded to retriever, got %v want [pt-BR es]", got)
	}
}

// TestMCPSearch_LanguagesAbsentIsNoFilter verifies that omitting languages leaves
// SearchQuery.Languages empty (no filtering, behaviour unchanged, SPEC §9.5).
func TestMCPSearch_LanguagesAbsentIsNoFilter(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	got := []string{"sentinel"}
	retriever := &askAudioRetrieverStub{
		indexingComplete: true,
		OnSearch: func(q model.SearchQuery) ([]model.SearchHit, error) {
			got = q.Languages
			return []model.SearchHit{}, nil
		},
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_search","arguments":{"query":"q"}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(payload))
	}
	if len(got) != 0 {
		t.Fatalf("absent languages filter must leave SearchQuery.Languages empty, got %v", got)
	}
}

// TestMCPSearch_InvalidLanguageTagIsInvalidField verifies a syntactically invalid
// BCP-47 tag is reported as INVALID_FIELD (SPEC §9.5/§14) — not silently ignored
// and not a server error.
func TestMCPSearch_InvalidLanguageTagIsInvalidField(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	retriever := &askAudioRetrieverStub{indexingComplete: true}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_search","arguments":{"query":"q","languages":["en","not a tag!"]}}}`)
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if got := string(body); !containsCanonicalCode(got, "INVALID_FIELD") {
		t.Fatalf("invalid BCP-47 tag must yield INVALID_FIELD, body=%s", got)
	}
	if retriever.searchCalled.Load() {
		t.Fatal("retriever.Search must not be called when a language tag is invalid")
	}
}

// TestMCPAsk_LanguagesFilterPassedThrough verifies dir2mcp_ask accepts the
// optional languages filter (SPEC §9.5/§15.3) and forwards it to Ask's query.
func TestMCPAsk_LanguagesFilterPassedThrough(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	var got []string
	retriever := &askAudioRetrieverStub{
		indexingComplete: true,
		askResult:        model.AskResult{Answer: "ok"},
		OnAskQuery:       func(q model.SearchQuery) { got = q.Languages },
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_ask","arguments":{"question":"q","languages":["en"]}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(payload))
	}
	if len(got) != 1 || got[0] != "en" {
		t.Fatalf("ask languages filter not forwarded, got %v want [en]", got)
	}
}

// TestMCPAsk_InvalidLanguageTagIsInvalidField mirrors the search case for ask.
func TestMCPAsk_InvalidLanguageTagIsInvalidField(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	retriever := &askAudioRetrieverStub{indexingComplete: true, askResult: model.AskResult{Answer: "ok"}}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_ask","arguments":{"question":"q","languages":["@@@"]}}}`)
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if got := string(body); !containsCanonicalCode(got, "INVALID_FIELD") {
		t.Fatalf("invalid BCP-47 tag on ask must yield INVALID_FIELD, body=%s", got)
	}
	if retriever.askCalled.Load() {
		t.Fatal("retriever.Ask must not be called when a language tag is invalid")
	}
}

// TestMCPTools_SchemaAdvertisesLanguages confirms both the search and ask tool
// input schemas expose the optional languages array filter (SPEC §15.2/§15.3).
func TestMCPTools_SchemaAdvertisesLanguages(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	retriever := &askAudioRetrieverStub{indexingComplete: true}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(payload))
	}

	var envelope struct {
		Result struct {
			Tools []struct {
				Name        string                 `json:"name"`
				InputSchema map[string]interface{} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]bool{"dir2mcp_search": false, "dir2mcp_ask": false}
	for _, tool := range envelope.Result.Tools {
		if _, ok := want[tool.Name]; !ok {
			continue
		}
		props, _ := tool.InputSchema["properties"].(map[string]interface{})
		langs, ok := props["languages"].(map[string]interface{})
		if !ok {
			t.Errorf("%s input schema missing optional languages filter, props=%v", tool.Name, props)
			continue
		}
		if langs["type"] != "array" {
			t.Errorf("%s languages must be an array, got %#v", tool.Name, langs["type"])
		}
		want[tool.Name] = true
	}
	for name, found := range want {
		if !found {
			t.Errorf("%s tool not advertised with a languages filter", name)
		}
	}
}

// containsCanonicalCode reports whether the JSON-RPC response body carries the
// given canonical error code as a quoted JSON string (tolerating either an
// inline data.code field or a tool-result error envelope).
func containsCanonicalCode(body, code string) bool {
	return strings.Contains(body, `"`+code+`"`)
}

package tests

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TestMCPSearch_DateWindowPassedThrough verifies dir2mcp_search accepts the
// optional date_from/date_to bounds (SPEC §9.6) and forwards them to the
// retriever's SearchQuery as inclusive Unix-second bounds: a bare date_from
// anchors to the start of its UTC day, and a bare date_to to the last whole
// second of its day (end-of-day inclusivity).
func TestMCPSearch_DateWindowPassedThrough(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	var gotFrom, gotTo int64
	retriever := &askAudioRetrieverStub{
		indexingComplete: true,
		OnSearch: func(q model.SearchQuery) ([]model.SearchHit, error) {
			gotFrom, gotTo = q.DateFrom, q.DateTo
			return []model.SearchHit{}, nil
		},
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_search","arguments":{"query":"q","date_from":"2026-04-01","date_to":"2026-04-30"}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(payload))
	}

	wantFrom := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC).Unix()
	wantTo := time.Date(2026, 4, 30, 23, 59, 59, 0, time.UTC).Unix()
	if gotFrom != wantFrom {
		t.Errorf("bare date_from must anchor to start of UTC day: got %d want %d", gotFrom, wantFrom)
	}
	if gotTo != wantTo {
		t.Errorf("bare date_to must be inclusive to end of UTC day: got %d want %d", gotTo, wantTo)
	}
}

// TestMCPSearch_DateWindowRFC3339PassedThrough verifies an RFC 3339 timestamp is
// forwarded verbatim as its Unix second (SPEC §9.6).
func TestMCPSearch_DateWindowRFC3339PassedThrough(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	var gotFrom, gotTo int64
	retriever := &askAudioRetrieverStub{
		indexingComplete: true,
		OnSearch: func(q model.SearchQuery) ([]model.SearchHit, error) {
			gotFrom, gotTo = q.DateFrom, q.DateTo
			return []model.SearchHit{}, nil
		},
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_search","arguments":{"query":"q","date_from":"2026-04-01T12:30:00Z","date_to":"2026-04-01T18:00:00Z"}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(payload))
	}

	wantFrom := time.Date(2026, 4, 1, 12, 30, 0, 0, time.UTC).Unix()
	wantTo := time.Date(2026, 4, 1, 18, 0, 0, 0, time.UTC).Unix()
	if gotFrom != wantFrom || gotTo != wantTo {
		t.Fatalf("RFC 3339 bounds not forwarded: got from=%d to=%d want from=%d to=%d", gotFrom, gotTo, wantFrom, wantTo)
	}
}

// TestMCPSearch_DateWindowAbsentIsNoFilter verifies that omitting date_from/date_to
// leaves both bounds at 0 (open on both sides, behaviour unchanged, SPEC §9.6).
func TestMCPSearch_DateWindowAbsentIsNoFilter(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	gotFrom, gotTo := int64(-1), int64(-1)
	retriever := &askAudioRetrieverStub{
		indexingComplete: true,
		OnSearch: func(q model.SearchQuery) ([]model.SearchHit, error) {
			gotFrom, gotTo = q.DateFrom, q.DateTo
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
	if gotFrom != 0 || gotTo != 0 {
		t.Fatalf("absent date window must leave both bounds 0, got from=%d to=%d", gotFrom, gotTo)
	}
}

// TestMCPSearch_MalformedDateIsInvalidField verifies a value that is neither an
// RFC 3339 timestamp nor a bare YYYY-MM-DD date is INVALID_FIELD (df-008), and the
// retriever is never called.
func TestMCPSearch_MalformedDateIsInvalidField(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	retriever := &askAudioRetrieverStub{indexingComplete: true}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_search","arguments":{"query":"q","date_from":"last tuesday"}}}`)
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if got := string(body); !containsCanonicalCode(got, "INVALID_FIELD") {
		t.Fatalf("malformed date_from must yield INVALID_FIELD, body=%s", got)
	}
	if retriever.searchCalled.Load() {
		t.Fatal("retriever.Search must not be called when date_from is malformed")
	}
}

// TestMCPSearch_InvertedDateWindowIsInvalidField verifies date_from after date_to
// is INVALID_FIELD (df-008), not a silently-empty result.
func TestMCPSearch_InvertedDateWindowIsInvalidField(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	retriever := &askAudioRetrieverStub{indexingComplete: true}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_search","arguments":{"query":"q","date_from":"2026-05-01","date_to":"2026-04-01"}}}`)
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if got := string(body); !containsCanonicalCode(got, "INVALID_FIELD") {
		t.Fatalf("inverted date window must yield INVALID_FIELD, body=%s", got)
	}
	if retriever.searchCalled.Load() {
		t.Fatal("retriever.Search must not be called for an inverted date window")
	}
}

// TestMCPAsk_DateWindowPassedThrough verifies dir2mcp_ask forwards date_from/date_to
// to Ask's query (SPEC §9.6/§15.3).
func TestMCPAsk_DateWindowPassedThrough(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	var gotFrom, gotTo int64
	retriever := &askAudioRetrieverStub{
		indexingComplete: true,
		askResult:        model.AskResult{Answer: "ok"},
		OnAskQuery:       func(q model.SearchQuery) { gotFrom, gotTo = q.DateFrom, q.DateTo },
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_ask","arguments":{"question":"q","date_from":"2026-04-01"}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(payload))
	}
	wantFrom := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC).Unix()
	if gotFrom != wantFrom || gotTo != 0 {
		t.Fatalf("ask date window not forwarded, got from=%d to=%d want from=%d to=0", gotFrom, gotTo, wantFrom)
	}
}

// TestMCPAsk_MalformedDateIsInvalidField mirrors the search malformed case for ask.
func TestMCPAsk_MalformedDateIsInvalidField(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	retriever := &askAudioRetrieverStub{indexingComplete: true, askResult: model.AskResult{Answer: "ok"}}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_ask","arguments":{"question":"q","date_to":"04/30/2026"}}}`)
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if got := string(body); !containsCanonicalCode(got, "INVALID_FIELD") {
		t.Fatalf("malformed date_to on ask must yield INVALID_FIELD, body=%s", got)
	}
	if retriever.askCalled.Load() {
		t.Fatal("retriever.Ask must not be called when date_to is malformed")
	}
}

// TestMCPTools_SchemaAdvertisesDateWindow confirms both the search and ask tool
// input schemas expose the optional date_from/date_to string bounds (SPEC §9.6).
func TestMCPTools_SchemaAdvertisesDateWindow(t *testing.T) {
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
		for _, key := range []string{"date_from", "date_to"} {
			bound, ok := props[key].(map[string]interface{})
			if !ok {
				t.Errorf("%s input schema missing optional %s bound, props=%v", tool.Name, key, props)
				continue
			}
			if bound["type"] != "string" {
				t.Errorf("%s %s must be a string, got %#v", tool.Name, key, bound["type"])
			}
		}
		want[tool.Name] = true
	}
	for name, found := range want {
		if !found {
			t.Errorf("%s tool not advertised with a date window filter", name)
		}
	}
}

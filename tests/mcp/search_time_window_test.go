package tests

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TestMCPSearch_TimeWindowPassedThrough verifies dir2mcp_search accepts the
// optional time_from_ms/time_to_ms bounds (SPEC §9.8) and forwards them to the
// retriever's SearchQuery with explicit presence flags.
func TestMCPSearch_TimeWindowPassedThrough(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	var got model.SearchQuery
	retriever := &askAudioRetrieverStub{
		indexingComplete: true,
		OnSearch: func(q model.SearchQuery) ([]model.SearchHit, error) {
			got = q
			return []model.SearchHit{}, nil
		},
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_search","arguments":{"query":"q","time_from_ms":15000,"time_to_ms":35000}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(payload))
	}
	if !got.HasTimeFrom || got.TimeFromMS != 15000 || !got.HasTimeTo || got.TimeToMS != 35000 {
		t.Fatalf("time window not forwarded: hasFrom=%v from=%d hasTo=%v to=%d",
			got.HasTimeFrom, got.TimeFromMS, got.HasTimeTo, got.TimeToMS)
	}
}

// TestMCPSearch_TimeWindowZeroLowerBoundIsPresent pins the presence design: a
// time_from_ms of 0 is forwarded as PRESENT (HasTimeFrom true), not treated as
// absent — 0 is a valid lower bound (video start).
func TestMCPSearch_TimeWindowZeroLowerBoundIsPresent(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	var got model.SearchQuery
	retriever := &askAudioRetrieverStub{
		indexingComplete: true,
		OnSearch: func(q model.SearchQuery) ([]model.SearchHit, error) {
			got = q
			return []model.SearchHit{}, nil
		},
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_search","arguments":{"query":"q","time_from_ms":0,"time_to_ms":5000}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(payload))
	}
	if !got.HasTimeFrom || got.TimeFromMS != 0 {
		t.Fatalf("time_from_ms=0 must forward as present with value 0, got hasFrom=%v from=%d", got.HasTimeFrom, got.TimeFromMS)
	}
}

// TestMCPSearch_TimeWindowAbsentIsNoFilter verifies omitting the bounds leaves
// both presence flags false (open on both sides, behaviour unchanged, §9.8).
func TestMCPSearch_TimeWindowAbsentIsNoFilter(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	got := model.SearchQuery{HasTimeFrom: true, HasTimeTo: true} // poison to prove reset
	retriever := &askAudioRetrieverStub{
		indexingComplete: true,
		OnSearch: func(q model.SearchQuery) ([]model.SearchHit, error) {
			got = q
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
	if got.HasTimeFrom || got.HasTimeTo {
		t.Fatalf("absent time window must leave both presence flags false, got hasFrom=%v hasTo=%v", got.HasTimeFrom, got.HasTimeTo)
	}
}

// TestMCPSearch_InvertedTimeWindowIsInvalidField verifies time_from_ms after
// time_to_ms is INVALID_FIELD (df-008), not a silently-empty result, and the
// retriever is never called.
func TestMCPSearch_InvertedTimeWindowIsInvalidField(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	retriever := &askAudioRetrieverStub{indexingComplete: true}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_search","arguments":{"query":"q","time_from_ms":5000,"time_to_ms":1000}}}`)
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if got := string(body); !containsCanonicalCode(got, "INVALID_FIELD") {
		t.Fatalf("inverted time window must yield INVALID_FIELD, body=%s", got)
	}
	if retriever.searchCalled.Load() {
		t.Fatal("retriever.Search must not be called for an inverted time window")
	}
}

// TestMCPSearch_NegativeTimeIsInvalidField verifies a negative bound is
// INVALID_FIELD (df-008), and the retriever is never called.
func TestMCPSearch_NegativeTimeIsInvalidField(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	retriever := &askAudioRetrieverStub{indexingComplete: true}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_search","arguments":{"query":"q","time_from_ms":-1}}}`)
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if got := string(body); !containsCanonicalCode(got, "INVALID_FIELD") {
		t.Fatalf("negative time_from_ms must yield INVALID_FIELD, body=%s", got)
	}
	if retriever.searchCalled.Load() {
		t.Fatal("retriever.Search must not be called for a negative bound")
	}
}

// TestMCPAsk_TimeWindowPassedThrough verifies dir2mcp_ask forwards
// time_from_ms/time_to_ms to Ask's query (SPEC §9.8/§15.3).
func TestMCPAsk_TimeWindowPassedThrough(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	var got model.SearchQuery
	retriever := &askAudioRetrieverStub{
		indexingComplete: true,
		askResult:        model.AskResult{Answer: "ok"},
		OnAskQuery:       func(q model.SearchQuery) { got = q },
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_ask","arguments":{"question":"q","time_from_ms":20000}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(payload))
	}
	if !got.HasTimeFrom || got.TimeFromMS != 20000 || got.HasTimeTo {
		t.Fatalf("ask time window not forwarded, got hasFrom=%v from=%d hasTo=%v", got.HasTimeFrom, got.TimeFromMS, got.HasTimeTo)
	}
}

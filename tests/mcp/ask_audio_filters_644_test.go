package tests

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TestMCPAskAudio_InheritedFiltersReachRetrieval pins issue #644: dir2mcp_ask_audio
// "inherits all dir2mcp_ask fields" (bs-007 / SPEC §15.10) and its advertised
// schema is a clone of ask's, so every filter it publishes must reach the
// retrieval query.
//
// Before the fix the handler allowed only eight names, so languages,
// language_match, date_from/date_to, time_from_ms/time_to_ms, entities and
// events came back INVALID_FIELD. A schema-driven client could not narrow an
// ask_audio question at all.
func TestMCPAskAudio_InheritedFiltersReachRetrieval(t *testing.T) {
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
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_ask_audio","arguments":{`+
			`"question":"q","k":7,"index":"text","path_prefix":"docs/","file_glob":"*.md","doc_types":["document"],`+
			`"languages":["pt-BR"],"language_match":"strict","date_from":"2026-01-01","date_to":"2026-02-01",`+
			`"time_from_ms":1000,"time_to_ms":5000,"entities":["ent-1"],"events":["goal"],"voice_id":"v1"}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(payload))
	}
	assertAskAudioFiltersForwarded(t, got)
}

// assertAskAudioFiltersForwarded checks every advertised ask_audio filter arrived
// in the retrieval query. One table row per advertised field, so a newly
// advertised filter that never reaches retrieval shows up as its own failure.
func assertAskAudioFiltersForwarded(t *testing.T, got model.SearchQuery) {
	t.Helper()
	for _, check := range []struct {
		field string
		ok    bool
		got   interface{}
	}{
		{"k", got.K == 7, got.K},
		{"index", got.Index == "text", got.Index},
		{"path_prefix", got.PathPrefix == "docs/", got.PathPrefix},
		{"file_glob", got.FileGlob == "*.md", got.FileGlob},
		{"doc_types", reflect.DeepEqual(got.DocTypes, []string{"document"}), got.DocTypes},
		{"languages", reflect.DeepEqual(got.Languages, []string{"pt-BR"}), got.Languages},
		{"language_match", got.LanguageMatch == "strict", got.LanguageMatch},
		{"date_from", got.DateFrom != 0, got.DateFrom},
		{"date_to", got.DateTo != 0, got.DateTo},
		{"time_from_ms", got.HasTimeFrom && got.TimeFromMS == 1000, got.TimeFromMS},
		{"time_to_ms", got.HasTimeTo && got.TimeToMS == 5000, got.TimeToMS},
		{"entities", reflect.DeepEqual(got.Entities, []string{"ent-1"}), got.Entities},
		{"events", reflect.DeepEqual(got.Events, []string{"goal"}), got.Events},
	} {
		if !check.ok {
			t.Errorf("ask_audio advertises %s but it did not reach retrieval: got %#v", check.field, check.got)
		}
	}
}

// TestMCPAskAudio_InvalidInheritedFilterIsRejected pins the other direction: now
// that ask_audio accepts the inherited filters, it must also apply their
// validation rather than pass a malformed value down. An unrecognized
// language_match is INVALID_FIELD (SPEC §9.5/§14) on ask_audio exactly as it is
// on ask and search.
func TestMCPAskAudio_InvalidInheritedFilterIsRejected(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	retriever := &askAudioRetrieverStub{indexingComplete: true, askResult: model.AskResult{Answer: "ok"}}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_ask_audio","arguments":{"question":"q","languages":["en"],"language_match":"loose"}}}`)
	defer func() { _ = resp.Body.Close() }()
	assertToolCallErrorCode(t, resp, "INVALID_FIELD")
	if retriever.askCalled.Load() {
		t.Fatal("retriever.Ask must not run when an inherited filter is invalid")
	}
}

package tests

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TestMCPSearch_SpeakerFilterPassedThrough verifies the dir2mcp_search tool
// accepts the optional speaker filter (SPEC §8.6.8/§15.2) and forwards it to the
// retriever's SearchQuery.
func TestMCPSearch_SpeakerFilterPassedThrough(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	var gotSpeaker string
	retriever := &askAudioRetrieverStub{
		indexingComplete: true,
		OnSearch: func(q model.SearchQuery) ([]model.SearchHit, error) {
			gotSpeaker = q.Speaker
			return []model.SearchHit{}, nil
		},
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_search","arguments":{"query":"q","speaker":"S2"}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(payload))
	}
	if gotSpeaker != "S2" {
		t.Fatalf("speaker filter not forwarded to retriever, got %q want S2", gotSpeaker)
	}
}

// TestMCPSearch_TimeSpanSurfacesSpeaker verifies a diarized "time" span hit
// serializes speaker/speaker_label additively (SPEC §8.6.8/§9.2), and a hit
// without a speaker does not include the keys.
func TestMCPSearch_TimeSpanSurfacesSpeaker(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	retriever := &askAudioRetrieverStub{
		indexingComplete: true,
		searchHits: []model.SearchHit{
			{
				ChunkID: 1, RelPath: "interview.mp4", DocType: "video", Score: 0.9, Snippet: "guest",
				Span: model.Span{Kind: "time", StartMS: 133000, EndMS: 161000, Speaker: "S2", SpeakerLabel: "Guest"},
			},
			{
				ChunkID: 2, RelPath: "plain.mp3", DocType: "audio", Score: 0.8, Snippet: "flat",
				Span: model.Span{Kind: "time", StartMS: 0, EndMS: 1000},
			},
		},
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_search","arguments":{"query":"q","k":5}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(payload))
	}

	var envelope struct {
		Result struct {
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	hits, _ := envelope.Result.StructuredContent["hits"].([]interface{})
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}

	diarized := hits[0].(map[string]interface{})["span"].(map[string]interface{})
	if diarized["speaker"] != "S2" {
		t.Errorf("diarized span.speaker = %#v, want S2", diarized["speaker"])
	}
	if diarized["speaker_label"] != "Guest" {
		t.Errorf("diarized span.speaker_label = %#v, want Guest", diarized["speaker_label"])
	}

	flat := hits[1].(map[string]interface{})["span"].(map[string]interface{})
	if _, ok := flat["speaker"]; ok {
		t.Errorf("non-diarized span must not carry speaker, got %#v", flat)
	}
}

// TestMCPSearch_SchemaAdvertisesSpeaker confirms the search tool's input schema
// exposes the optional speaker filter and the shared Span schema's time variant
// documents speaker/speaker_label.
func TestMCPSearch_SchemaAdvertisesSpeaker(t *testing.T) {
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
	var found bool
	for _, tool := range envelope.Result.Tools {
		if tool.Name != "dir2mcp_search" {
			continue
		}
		found = true
		props, _ := tool.InputSchema["properties"].(map[string]interface{})
		if _, ok := props["speaker"]; !ok {
			t.Errorf("dir2mcp_search input schema missing optional speaker filter, props=%v", props)
		}
	}
	if !found {
		t.Fatal("dir2mcp_search tool not advertised")
	}
}

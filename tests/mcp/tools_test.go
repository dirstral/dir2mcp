package tests

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"dir2mcp/internal/config"
	"dir2mcp/internal/mcp"
	"dir2mcp/internal/model"
	"dir2mcp/internal/protocol"
	"dir2mcp/internal/store"
)

// TestMCPToolsList_RegistersDayOneToolsWithSchemas verifies that tools/list
// advertises the expected Day 1 MCP tools with schemas.
func TestMCPToolsList_RegistersDayOneToolsWithSchemas(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)

	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}

	var envelope struct {
		Result struct {
			Tools []struct {
				Name         string                 `json:"name"`
				InputSchema  map[string]interface{} `json:"inputSchema"`
				OutputSchema map[string]interface{} `json:"outputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	expected := map[string]bool{
		protocol.ToolNameSearch:           false,
		protocol.ToolNameAsk:              false,
		protocol.ToolNameAskAudio:         false,
		protocol.ToolNameTranscribe:       false,
		protocol.ToolNameAnnotate:         false,
		protocol.ToolNameTranscribeAndAsk: false,
		protocol.ToolNameOpenFile:         false,
		protocol.ToolNameListFiles:        false,
		protocol.ToolNameStats:            false,
	}

	for _, tool := range envelope.Result.Tools {
		if _, ok := expected[tool.Name]; !ok {
			continue
		}
		if len(tool.InputSchema) == 0 {
			t.Fatalf("tool %s missing inputSchema", tool.Name)
		}
		if len(tool.OutputSchema) == 0 {
			t.Fatalf("tool %s missing outputSchema", tool.Name)
		}
		expected[tool.Name] = true
	}

	for name, seen := range expected {
		if !seen {
			t.Fatalf("missing expected tool registration: %s", name)
		}
	}
}

func TestMCPToolsCallTranscribe_MissingRelPath(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":30,"method":"tools/call","params":{"name":"dir2mcp_transcribe","arguments":{}}}`)
	defer func() {
		_ = resp.Body.Close()
	}()
	assertToolCallErrorCode(t, resp, "MISSING_FIELD")
}

func requireRetryableAndResetBody(t *testing.T, resp *http.Response) {
	t.Helper()
	// read and validate that the response has a retryable error flag inside
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}

	var payload struct {
		Result struct {
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("failed to unmarshal response body: %v; body=%s", err, string(body))
	}

	errObjRaw, ok := payload.Result.StructuredContent["error"]
	if !ok {
		t.Fatalf("response body missing structuredContent.error: %s", string(body))
	}
	errObj, ok := errObjRaw.(map[string]interface{})
	if !ok {
		t.Fatalf("structuredContent.error is not an object: %#v", errObjRaw)
	}
	retryableVal, ok := errObj["retryable"]
	if !ok {
		t.Fatalf("response body missing structuredContent.error.retryable: %s", string(body))
	}
	retryableBool, ok := retryableVal.(bool)
	if !ok {
		t.Fatalf("retryable field is not a boolean: %#v", retryableVal)
	}
	if !retryableBool {
		t.Fatalf("expected retryable=true, got %v", retryableBool)
	}

	// reset body so callers can read it again (assertToolCallErrorCode will)
	resp.Body = io.NopCloser(bytes.NewReader(body))
}

func TestMCPToolsCallTranscribe_ProviderFailureIsRetryable(t *testing.T) {
	cfg, st, _ := setupMCPToolStore(t, "voice.wav", "audio", []byte("RIFF0000WAVEfmt data"))
	cfg.MistralAPIKey = "test-key"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/audio/transcriptions" {
			http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()
	cfg.MistralBaseURL = upstream.URL

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":31,"method":"tools/call","params":{"name":"dir2mcp_transcribe","arguments":{"rel_path":"voice.wav"}}}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 for retryable provider failure, got %d", resp.StatusCode)
	}

	requireRetryableAndResetBody(t, resp)
	assertToolCallErrorCode(t, resp, "TRANSCRIBE_FAILED")
}

func TestMCPToolsCallTranscribe_Success(t *testing.T) {
	cfg, st, _ := setupMCPToolStore(t, "voice.wav", "audio", []byte("RIFF0000WAVEfmt data"))
	cfg.MistralAPIKey = "test-key"
	var gotLanguage string

	// use a channel to propagate handler errors back to the main goroutine
	errCh := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			errCh <- fmt.Errorf("parse multipart form: %v", err)
			return
		}
		gotLanguage = r.FormValue("language")
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"segments":[{"start":1,"end":2,"text":"alpha"},{"start":3,"end":4,"text":"beta"}]}`)
	}))
	defer upstream.Close()
	cfg.MistralBaseURL = upstream.URL

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":32,"method":"tools/call","params":{"name":"dir2mcp_transcribe","arguments":{"rel_path":"voice.wav","timestamps":true,"language":"fr"}}}`)
	defer func() { _ = resp.Body.Close() }()
	// check if the upstream handler encountered an error
	select {
	case err := <-errCh:
		t.Fatalf("upstream handler error: %v", err)
	default:
	}
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}

	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Result.IsError {
		t.Fatalf("expected success, got %#v", envelope.Result.StructuredContent)
	}
	if gotLanguage != "fr" {
		t.Fatalf("expected language hint to be forwarded, got %q", gotLanguage)
	}
	if got := envelope.Result.StructuredContent["provider"]; got != "mistral" {
		t.Fatalf("unexpected provider: %#v", got)
	}
	if got, ok := envelope.Result.StructuredContent["transcribed"].(bool); !ok || !got {
		t.Fatalf("expected transcribed=true, got %#v", envelope.Result.StructuredContent["transcribed"])
	}
	if got, ok := envelope.Result.StructuredContent["transcribed_now"].(bool); !ok || !got {
		t.Fatalf("expected transcribed_now=true for fresh transcription, got %#v", envelope.Result.StructuredContent["transcribed_now"])
	}
	if got, ok := envelope.Result.StructuredContent["indexed"].(bool); !ok || !got {
		t.Fatalf("expected indexed=true, got %#v", envelope.Result.StructuredContent["indexed"])
	}
}

func TestMCPToolsCallTranscribe_CreatesAudioDocWhenNotYetIndexed(t *testing.T) {
	rootDir := t.TempDir()
	stateDir := filepath.Join(rootDir, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(rootDir, "audio"), 0o755); err != nil {
		t.Fatalf("mkdir audio dir: %v", err)
	}
	relPath := "audio/voice.wav"
	if err := os.WriteFile(filepath.Join(rootDir, relPath), []byte("RIFF0000WAVEfmt data"), 0o644); err != nil {
		t.Fatalf("write audio file: %v", err)
	}

	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Default()
	cfg.RootDir = rootDir
	cfg.StateDir = stateDir
	cfg.MCPPath = protocol.DefaultMCPPath
	cfg.AuthMode = "none"
	cfg.MistralAPIKey = "test-key"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			t.Fatalf("parse multipart form: %v", err)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"segments":[{"start":1,"end":2,"text":"alpha"}]}`)
	}))
	defer upstream.Close()
	cfg.MistralBaseURL = upstream.URL

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":320,"method":"tools/call","params":{"name":"dir2mcp_transcribe","arguments":{"rel_path":"audio/voice.wav"}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}

	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Result.IsError {
		t.Fatalf("expected success, got %#v", envelope.Result.StructuredContent)
	}

	doc, err := st.GetDocumentByPath(context.Background(), relPath)
	if err != nil {
		t.Fatalf("expected document to be upserted for audio path: %v", err)
	}
	if doc.DocType != "audio" {
		t.Fatalf("expected doc_type audio, got %q", doc.DocType)
	}
}

func TestMCPToolsCallAnnotate_MissingSchema(t *testing.T) {
	cfg, st, _ := setupMCPToolStore(t, "note.txt", "text", []byte("alpha note"))
	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":33,"method":"tools/call","params":{"name":"dir2mcp_annotate","arguments":{"rel_path":"note.txt"}}}`)
	defer func() { _ = resp.Body.Close() }()
	assertToolCallErrorCode(t, resp, "MISSING_FIELD")
}

func TestMCPToolsCallAnnotate_ProviderFailure(t *testing.T) {
	cfg, st, _ := setupMCPToolStore(t, "note.txt", "text", []byte("alpha note"))
	cfg.MistralAPIKey = "test-key"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()
	cfg.MistralBaseURL = upstream.URL

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":34,"method":"tools/call","params":{"name":"dir2mcp_annotate","arguments":{"rel_path":"note.txt","schema_json":{"type":"object"}}}}`)
	defer func() { _ = resp.Body.Close() }()
	assertToolCallErrorCode(t, resp, "ANNOTATE_FAILED")
}

func TestMCPToolsCallAnnotate_Success(t *testing.T) {
	cfg, st, _ := setupMCPToolStore(t, "note.txt", "text", []byte("alpha note"))
	cfg.MistralAPIKey = "test-key"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"summary\":\"alpha\",\"tags\":[\"x\"]}"}}]}`)
	}))
	defer upstream.Close()
	cfg.MistralBaseURL = upstream.URL

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":35,"method":"tools/call","params":{"name":"dir2mcp_annotate","arguments":{"rel_path":"note.txt","schema_json":{"type":"object","properties":{"summary":{"type":"string"}}},"index_flattened_text":true}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}

	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Result.IsError {
		t.Fatalf("expected success, got %#v", envelope.Result.StructuredContent)
	}
	if got := envelope.Result.StructuredContent["stored"]; got != true {
		t.Fatalf("expected stored=true, got %#v", got)
	}
	obj, ok := envelope.Result.StructuredContent["annotation_json"].(map[string]interface{})
	if !ok || obj["summary"] != "alpha" {
		t.Fatalf("unexpected annotation_json: %#v", envelope.Result.StructuredContent["annotation_json"])
	}
}

func TestMCPToolsCallAnnotate_PromptTooLarge(t *testing.T) {
	cfg, st, _ := setupMCPToolStore(t, "note.txt", "text", []byte("small"))
	cfg.MistralAPIKey = "test-key"

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	// create a ridiculously large schema to push prompt over our hard limit
	bigSchema := map[string]interface{}{"foo": strings.Repeat("x", 250000)}
	schemaBytes, err := json.Marshal(bigSchema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	rpc := fmt.Sprintf(`{"jsonrpc":"2.0","id":40,"method":"tools/call","params":{"name":"dir2mcp_annotate","arguments":{"rel_path":"note.txt","schema_json":%s}}}`, string(schemaBytes))
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, rpc)
	defer func() { _ = resp.Body.Close() }()
	assertToolCallErrorCode(t, resp, "ANNOTATE_FAILED")
}

func TestMCPToolsCallTranscribeAndAsk_MissingQuestion(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, &askAudioRetrieverStub{}).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":36,"method":"tools/call","params":{"name":"dir2mcp_transcribe_and_ask","arguments":{"rel_path":"voice.wav"}}}`)
	defer func() { _ = resp.Body.Close() }()
	assertToolCallErrorCode(t, resp, "MISSING_FIELD")
}

func TestMCPToolsCallTranscribeAndAsk_Success(t *testing.T) {
	cfg, st, _ := setupMCPToolStore(t, "voice.wav", "audio", []byte("RIFF0000WAVEfmt data"))
	cfg.MistralAPIKey = "test-key"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"segments":[{"start":1,"end":2,"text":"alpha in transcript"}]}`)
	}))
	defer upstream.Close()
	cfg.MistralBaseURL = upstream.URL

	retriever := &askAudioRetrieverStub{
		askResult: model.AskResult{
			Question: "what is alpha?",
			Answer:   "alpha answer",
			Citations: []model.Citation{
				{ChunkID: 1, RelPath: "voice.wav", Span: model.Span{Kind: "time", StartMS: 1000, EndMS: 2000}},
			},
			Hits:             []model.SearchHit{{ChunkID: 1, RelPath: "voice.wav", Snippet: "alpha in transcript", Span: model.Span{Kind: "time", StartMS: 1000, EndMS: 2000}}},
			IndexingComplete: true,
		},
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":37,"method":"tools/call","params":{"name":"dir2mcp_transcribe_and_ask","arguments":{"rel_path":"voice.wav","question":"what is alpha?","k":5}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}

	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Result.IsError {
		t.Fatalf("expected success, got %#v", envelope.Result.StructuredContent)
	}
	if got := envelope.Result.StructuredContent["answer"]; got != "alpha answer" {
		t.Fatalf("unexpected answer: %#v", got)
	}
	if got := envelope.Result.StructuredContent["transcript_provider"]; got != "mistral" {
		t.Fatalf("unexpected transcript_provider: %#v", got)
	}
}

func setupMCPToolStore(t *testing.T, relPath, docType string, content []byte) (config.Config, *store.SQLiteStore, string) {
	t.Helper()
	rootDir := t.TempDir()
	stateDir := filepath.Join(rootDir, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(rootDir, relPath)), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, relPath), content, 0o644); err != nil {
		t.Fatalf("write input file: %v", err)
	}

	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init sqlite store: %v", err)
	}
	if err := st.UpsertDocument(context.Background(), model.Document{
		RelPath:     relPath,
		DocType:     docType,
		SourceType:  "filesystem",
		SizeBytes:   int64(len(content)),
		MTimeUnix:   1,
		ContentHash: "h1",
		Status:      "ok",
		Deleted:     false,
	}); err != nil {
		t.Fatalf("upsert document: %v", err)
	}

	// ensure callers don't need to close the store manually
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Default()
	cfg.RootDir = rootDir
	cfg.StateDir = stateDir
	cfg.MCPPath = protocol.DefaultMCPPath
	cfg.AuthMode = "none"
	return cfg, st, rootDir
}

func TestMCPToolsCallAskAudio_NilRetrieverReturnsIndexNotReady(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"dir2mcp_ask_audio","arguments":{"question":"What is indexed?"}}}`)
	defer func() {
		_ = resp.Body.Close()
	}()

	assertToolCallErrorCode(t, resp, "INDEX_NOT_READY")
}

func TestMCPToolsCallAskAudio_AskNotImplementedReturnsGracefulSuccess(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	retriever := &askAudioRetrieverStub{
		askErr: model.ErrNotImplemented,
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"dir2mcp_ask_audio","arguments":{"question":"What is indexed?"}}}`)
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}

	var envelope struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if envelope.Result.IsError {
		t.Fatal("expected graceful success for not-implemented ask")
	}
	if len(envelope.Result.Content) == 0 {
		t.Fatal("expected at least one content item")
	}
	if !strings.Contains(strings.ToLower(envelope.Result.Content[0].Text), strings.ToLower(protocol.ToolNameSearch)) {
		t.Fatalf("expected fallback guidance to dir2mcp.search, got %q", envelope.Result.Content[0].Text)
	}
}

func TestMCPToolsCallAskAudio_WithoutTTSReturnsTextOnly(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	retriever := &askAudioRetrieverStub{
		askResult: model.AskResult{
			Question:         "What is indexed?",
			Answer:           "Indexed content is available.",
			Citations:        []model.Citation{},
			Hits:             []model.SearchHit{},
			IndexingComplete: true,
		},
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":22,"method":"tools/call","params":{"name":"dir2mcp_ask_audio","arguments":{"question":"What is indexed?"}}}`)
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}

	var envelope struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if envelope.Result.IsError {
		t.Fatal("expected non-error response")
	}
	if len(envelope.Result.Content) != 1 {
		t.Fatalf("expected one text content item, got %#v", envelope.Result.Content)
	}
	if envelope.Result.Content[0].Type != "text" {
		t.Fatalf("expected text content item, got %#v", envelope.Result.Content[0])
	}
	if !strings.Contains(envelope.Result.Content[0].Text, "ELEVENLABS_API_KEY") {
		t.Fatalf("expected configuration hint for ELEVENLABS_API_KEY, got %q", envelope.Result.Content[0].Text)
	}
}

func TestMCPToolsCallAskAudio_WithTTSReturnsTextAndAudio(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	retriever := &askAudioRetrieverStub{
		askResult: model.AskResult{
			Question:         "What is indexed?",
			Answer:           "Indexed content is available.",
			Citations:        []model.Citation{},
			Hits:             []model.SearchHit{},
			IndexingComplete: true,
		},
	}
	tts := &fakeTTSSynthesizer{
		audio: []byte("fake-mp3-bytes"),
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever, mcp.WithTTS(tts)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":23,"method":"tools/call","params":{"name":"dir2mcp_ask_audio","arguments":{"question":"What is indexed?"}}}`)
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}

	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			Content           []toolContentEnvelope  `json:"content"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if envelope.Result.IsError {
		t.Fatal("expected successful ask_audio response")
	}
	if len(envelope.Result.Content) != 2 {
		t.Fatalf("expected text + audio content items, got %#v", envelope.Result.Content)
	}

	textItem := envelope.Result.Content[0]
	audioItem := envelope.Result.Content[1]
	if textItem.Type != "text" {
		t.Fatalf("unexpected text item: %#v", textItem)
	}
	if audioItem.Type != "audio" {
		t.Fatalf("unexpected audio item type: %#v", audioItem)
	}
	if audioItem.MIMEType != "audio/mpeg" {
		t.Fatalf("unexpected mime type: %q", audioItem.MIMEType)
	}

	wantEncoded := base64.StdEncoding.EncodeToString([]byte("fake-mp3-bytes"))
	if audioItem.Data != wantEncoded {
		t.Fatalf("unexpected audio data payload: got=%q want=%q", audioItem.Data, wantEncoded)
	}

	audioRaw, ok := envelope.Result.StructuredContent["audio"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected structuredContent.audio object, got %#v", envelope.Result.StructuredContent["audio"])
	}
	if gotMime, _ := audioRaw["mime_type"].(string); gotMime != "audio/mpeg" {
		t.Fatalf("unexpected structured audio mime_type: %#v", audioRaw["mime_type"])
	}
	if gotData, _ := audioRaw["data"].(string); gotData != wantEncoded {
		t.Fatalf("unexpected structured audio data: %#v", audioRaw["data"])
	}
}

// TestMCPToolsCallStats_ReturnsStructuredContent verifies the happy-path
// response shape for dir2mcp.stats.
func TestMCPToolsCallStats_ReturnsStructuredContent(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)

	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"dir2mcp_stats","arguments":{}}}`)
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}

	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
			Content           []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if envelope.Result.IsError {
		t.Fatal("expected stats tool call to succeed")
	}
	if len(envelope.Result.Content) == 0 {
		t.Fatal("expected content item in tool response")
	}
	if envelope.Result.StructuredContent["protocol_version"] != cfg.ProtocolVersion {
		t.Fatalf("unexpected protocol_version: %#v", envelope.Result.StructuredContent["protocol_version"])
	}
	assertStatsDocCountsUnavailable(t, envelope.Result.StructuredContent)
	assertStatsIndexingHasMode(t, envelope.Result.StructuredContent)
	assertStatsModelsSTTProvider(t, envelope.Result.StructuredContent)
	assertStatsSessionsActiveAndItems(t, envelope.Result.StructuredContent)
}

func assertStatsDocCountsUnavailable(t *testing.T, sc map[string]interface{}) {
	t.Helper()
	got, ok := sc["doc_counts_available"].(bool)
	if !ok {
		t.Fatalf("expected doc_counts_available boolean, got %#v", sc["doc_counts_available"])
	}
	if got {
		t.Fatalf("expected doc_counts_available=false when retriever missing, got true")
	}
}

func assertStatsIndexingHasMode(t *testing.T, sc map[string]interface{}) {
	t.Helper()
	indexingRaw, ok := sc["indexing"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected indexing object, got %#v", sc["indexing"])
	}
	if _, ok := indexingRaw["mode"]; !ok {
		t.Fatalf("expected indexing.mode in response: %#v", indexingRaw)
	}
}

func assertStatsModelsSTTProvider(t *testing.T, sc map[string]interface{}) {
	t.Helper()
	modelsRaw, ok := sc["models"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected models object, got %#v", sc["models"])
	}
	sttProvider, ok := modelsRaw["stt_provider"].(string)
	if !ok || sttProvider == "" {
		t.Fatalf("expected non-empty string models.stt_provider, got %#v", modelsRaw["stt_provider"])
	}
}

func assertStatsSessionsActiveAndItems(t *testing.T, sc map[string]interface{}) {
	t.Helper()
	sessionsRaw, ok := sc["sessions"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected sessions object, got %#v", sc["sessions"])
	}
	if active, ok := sessionsRaw["active"].(float64); !ok || active < 1 {
		t.Fatalf("expected sessions.active >= 1, got %#v", sessionsRaw["active"])
	}
	items, ok := sessionsRaw["items"].([]interface{})
	if !ok || len(items) == 0 {
		t.Fatalf("expected non-empty sessions.items, got %#v", sessionsRaw["items"])
	}
}

func TestMCPToolsCallStats_UsesRetrieverStats(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	retriever := &askAudioRetrieverStub{
		statsConfigured: true,
		stats: model.Stats{
			Root:            "/repo",
			StateDir:        "/repo/.dir2mcp",
			ProtocolVersion: cfg.ProtocolVersion,
			CorpusStats: model.CorpusStats{
				DocCounts:       map[string]int64{"code": 2, "md": 1},
				TotalDocs:       3,
				Scanned:         4,
				Indexed:         2,
				Skipped:         1,
				Deleted:         1,
				Representations: 6,
				ChunksTotal:     8,
				EmbeddedOK:      7,
				Errors:          1,
			},
		},
	}

	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":33,"method":"tools/call","params":{"name":"dir2mcp_stats","arguments":{}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}

	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Result.IsError {
		t.Fatal("expected stats tool call to succeed")
	}
	assertRetrieverStatsTopLevel(t, envelope.Result.StructuredContent)
	assertRetrieverStatsDocCounts(t, envelope.Result.StructuredContent)
	assertRetrieverStatsIndexing(t, envelope.Result.StructuredContent)
	if !retriever.statsCalled.Load() {
		t.Fatal("expected retriever.Stats to be called")
	}
	assertRetrieverStatsSessionsActive(t, envelope.Result.StructuredContent)
}

func assertRetrieverStatsTopLevel(t *testing.T, sc map[string]interface{}) {
	t.Helper()
	if got := sc["root"]; got != "/repo" {
		t.Fatalf("unexpected root: %#v", got)
	}
	if got := sc["state_dir"]; got != "/repo/.dir2mcp" {
		t.Fatalf("unexpected state_dir: %#v", got)
	}
	if got := sc["total_docs"]; got != float64(3) {
		t.Fatalf("unexpected total_docs: %#v", got)
	}
}

func assertRetrieverStatsDocCounts(t *testing.T, sc map[string]interface{}) {
	t.Helper()
	docCounts, ok := sc["doc_counts"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected doc_counts object, got %#v", sc["doc_counts"])
	}
	if docCounts["code"] != float64(2) || docCounts["md"] != float64(1) {
		t.Fatalf("unexpected doc_counts payload: %#v", docCounts)
	}
	got, ok := sc["doc_counts_available"].(bool)
	if !ok {
		t.Fatalf("expected doc_counts_available boolean, got %#v", sc["doc_counts_available"])
	}
	if !got {
		t.Fatalf("expected doc_counts_available=true when retriever provided stats, got %v", got)
	}
}

func assertRetrieverStatsIndexing(t *testing.T, sc map[string]interface{}) {
	t.Helper()
	indexingRaw, ok := sc["indexing"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected indexing object, got %#v", sc["indexing"])
	}
	if indexingRaw["scanned"] != float64(4) || indexingRaw["representations"] != float64(6) || indexingRaw["chunks_total"] != float64(8) {
		t.Fatalf("unexpected indexing payload: %#v", indexingRaw)
	}
}

func assertRetrieverStatsSessionsActive(t *testing.T, sc map[string]interface{}) {
	t.Helper()
	sessionsRaw, ok := sc["sessions"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected sessions object, got %#v", sc["sessions"])
	}
	if active, ok := sessionsRaw["active"].(float64); !ok || active < 1 {
		t.Fatalf("expected sessions.active >= 1, got %#v", sessionsRaw["active"])
	}
}

// TestMCPToolsCallListFiles_GracefulWithoutSQLiteStore verifies that
// list_files returns an empty, valid response when no store is configured.
func TestMCPToolsCallListFiles_GracefulWithoutSQLiteStore(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)

	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"dir2mcp_list_files","arguments":{"limit":10,"offset":0}}}`)
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}

	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if envelope.Result.IsError {
		t.Fatal("expected list_files tool call to succeed")
	}
	if got := envelope.Result.StructuredContent["limit"]; got != float64(10) {
		t.Fatalf("unexpected limit: %#v", got)
	}
	if got := envelope.Result.StructuredContent["total"]; got != float64(0) {
		t.Fatalf("unexpected total: %#v", got)
	}

	filesRaw, ok := envelope.Result.StructuredContent["files"].([]interface{})
	if !ok {
		t.Fatalf("expected files array, got %#v", envelope.Result.StructuredContent["files"])
	}
	if len(filesRaw) != 0 {
		t.Fatalf("expected empty files list, got %#v", filesRaw)
	}
}

// TestMCPToolsCallStats_RejectsUnknownArgument verifies stats argument
// validation failures are reported as INVALID_FIELD.
func TestMCPToolsCallStats_RejectsUnknownArgument(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)

	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"dir2mcp_stats","arguments":{"unexpected":true}}}`)
	defer func() {
		_ = resp.Body.Close()
	}()

	assertToolCallErrorCode(t, resp, "INVALID_FIELD")
}

// TestMCPToolsCallListFiles_RejectsUnknownArgument verifies unknown
// list_files arguments are rejected.
func TestMCPToolsCallListFiles_RejectsUnknownArgument(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)

	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"dir2mcp_list_files","arguments":{"limit":10,"offset":0,"foo":"bar"}}}`)
	defer func() {
		_ = resp.Body.Close()
	}()

	assertToolCallErrorCode(t, resp, "INVALID_FIELD")
}

// TestMCPToolsCallListFiles_RejectsLimitWrongType verifies non-integer limit
// values are rejected with INVALID_FIELD.
func TestMCPToolsCallListFiles_RejectsLimitWrongType(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)

	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"dir2mcp_list_files","arguments":{"limit":"10","offset":0}}}`)
	defer func() {
		_ = resp.Body.Close()
	}()

	assertToolCallErrorCode(t, resp, "INVALID_FIELD")
}

// TestMCPToolsCallListFiles_RejectsLimitOutOfRange verifies list_files limit
// range checks (min and max bounds).
func TestMCPToolsCallListFiles_RejectsLimitOutOfRange(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)

	for _, tc := range []struct {
		name string
		body string
		code string
	}{
		{
			name: "limit_zero",
			body: `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"dir2mcp_list_files","arguments":{"limit":0,"offset":0}}}`,
			code: "INVALID_RANGE",
		},
		{
			name: "limit_too_large",
			body: `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"dir2mcp_list_files","arguments":{"limit":5001,"offset":0}}}`,
			code: "INVALID_RANGE",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, tc.body)
			defer func() {
				_ = resp.Body.Close()
			}()
			assertToolCallErrorCode(t, resp, tc.code)
		})
	}
}

// TestMCPToolsCallListFiles_RejectsOffsetWrongType verifies non-integer offset
// values are rejected with INVALID_FIELD.
func TestMCPToolsCallListFiles_RejectsOffsetWrongType(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)

	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"dir2mcp_list_files","arguments":{"limit":10,"offset":"0"}}}`)
	defer func() {
		_ = resp.Body.Close()
	}()

	assertToolCallErrorCode(t, resp, "INVALID_FIELD")
}

// TestMCPToolsCallListFiles_RejectsNegativeOffset verifies negative offsets
// are rejected with INVALID_RANGE.
func TestMCPToolsCallListFiles_RejectsNegativeOffset(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, nil).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)

	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"dir2mcp_list_files","arguments":{"limit":10,"offset":-1}}}`)
	defer func() {
		_ = resp.Body.Close()
	}()

	assertToolCallErrorCode(t, resp, "INVALID_RANGE")
}

// TestMCPToolsCallListFiles_StoreFailureReturnsStoreCorrupt verifies store
// backend failures are surfaced as STORE_CORRUPT tool errors.
func TestMCPToolsCallListFiles_StoreFailureReturnsStoreCorrupt(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	server := httptest.NewServer(
		mcp.NewServer(cfg, nil, mcp.WithStore(&failingListFilesStore{err: errors.New("boom")})).Handler(),
	)
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)

	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"dir2mcp_list_files","arguments":{"limit":10,"offset":0}}}`)
	defer func() {
		_ = resp.Body.Close()
	}()

	assertToolCallErrorCode(t, resp, "STORE_CORRUPT")
}

func TestMCPToolsCallAsk_ReturnsStructuredAnswerAndCitations(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	// use the shared stub instead of a duplicate fake
	retriever := &askAudioRetrieverStub{
		askResult: model.AskResult{
			Answer:           "alpha answer",
			Citations:        []model.Citation{{ChunkID: 1, RelPath: "docs/a.md", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 2}}},
			Hits:             []model.SearchHit{{ChunkID: 1, RelPath: "docs/a.md", Snippet: "alpha"}},
			IndexingComplete: true,
		},
		EchoQuestion: true, // mirror the incoming question in results
	}
	server := httptest.NewServer(
		mcp.NewServer(cfg, retriever).Handler(),
	)
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"dir2mcp_ask","arguments":{"question":"what is alpha?","k":3,"index":"both"}}}`)
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}

	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
			Content           []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Result.IsError {
		t.Fatalf("expected ask success, got isError=true: %#v", envelope.Result.StructuredContent)
	}
	if envelope.Result.StructuredContent["question"] != "what is alpha?" {
		t.Fatalf("unexpected question field: %#v", envelope.Result.StructuredContent["question"])
	}
	if envelope.Result.StructuredContent["answer"] != "alpha answer" {
		t.Fatalf("unexpected answer field: %#v", envelope.Result.StructuredContent["answer"])
	}
	if _, ok := envelope.Result.StructuredContent["citations"].([]interface{}); !ok {
		t.Fatalf("expected citations array, got %#v", envelope.Result.StructuredContent["citations"])
	}
}

func TestMCPToolsCallAsk_SearchOnly(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	hits := []model.SearchHit{{ChunkID: 99, RelPath: "foo/bar.go", Snippet: "snippet"}}
	retriever := &askAudioRetrieverStub{
		searchHits:       hits,
		indexingComplete: true,
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":14,"method":"tools/call","params":{"name":"dir2mcp_ask","arguments":{"question":"q?","mode":"search_only","k":5}}}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}

	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
			Content           []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Result.IsError {
		t.Fatalf("expected search_only success, got isError=true: %#v", envelope.Result.StructuredContent)
	}
	if envelope.Result.StructuredContent["answer"] != "" {
		t.Fatalf("expected empty answer, got %#v", envelope.Result.StructuredContent["answer"])
	}
	hitsList, ok := envelope.Result.StructuredContent["hits"].([]interface{})
	if !ok {
		t.Fatalf("expected hits array, got %#v", envelope.Result.StructuredContent["hits"])
	}
	if len(hitsList) != 1 {
		t.Fatalf("unexpected hits length: %#v", hitsList)
	}
	if envelope.Result.StructuredContent["question"] != "q?" {
		t.Fatalf("question field passed through: %#v", envelope.Result.StructuredContent["question"])
	}
	if got, ok := envelope.Result.StructuredContent["indexing_complete"].(bool); !ok || !got {
		t.Fatalf("expected indexing_complete=true, got %#v", envelope.Result.StructuredContent["indexing_complete"])
	}
	if !retriever.searchCalled.Load() {
		t.Fatal("expected Search to be called")
	}
	if retriever.askCalled.Load() {
		t.Fatal("did not expect Ask to be called after optimization")
	}
}

func TestMCPToolsCallSearch_StructuredHitsSchemaShape(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	retriever := &askAudioRetrieverStub{
		searchHits: []model.SearchHit{
			{
				ChunkID: 42,
				RelPath: "docs/payment.md",
				DocType: "text",
				RepType: "raw",
				Score:   0.9,
				Snippet: "payment flow",
				Span:    model.Span{Kind: "lines", StartLine: 10, EndLine: 20},
			},
		},
		indexingComplete: true,
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":77,"method":"tools/call","params":{"name":"dir2mcp_search","arguments":{"query":"payment flow","k":5}}}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}

	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Result.IsError {
		t.Fatalf("expected search success, got isError=true: %#v", envelope.Result.StructuredContent)
	}

	hits, ok := envelope.Result.StructuredContent["hits"].([]interface{})
	if !ok || len(hits) != 1 {
		t.Fatalf("expected one serialized hit, got %#v", envelope.Result.StructuredContent["hits"])
	}
	hit, ok := hits[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected hit object, got %#v", hits[0])
	}
	span, ok := hit["span"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected span object, got %#v", hit["span"])
	}
	if span["kind"] != "lines" {
		t.Fatalf("expected span.kind=lines, got %#v", span["kind"])
	}
	if _, hasCamel := span["startLine"]; hasCamel {
		t.Fatalf("unexpected camelCase field in span: %#v", span)
	}
}

func TestMCPToolsCallSearch_RequiredOutputFieldsPresent(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	retriever := &askAudioRetrieverStub{searchHits: []model.SearchHit{}, indexingComplete: true}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":94,"method":"tools/call","params":{"name":"dir2mcp_search","arguments":{"query":"anything"}}}`)
	defer func() { _ = resp.Body.Close() }()

	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Result.IsError {
		t.Fatalf("unexpected error: %#v", envelope.Result.StructuredContent)
	}
	sc := envelope.Result.StructuredContent
	for _, field := range []string{"query", "k", "index_used", "hits", "indexing_complete"} {
		if _, ok := sc[field]; !ok {
			t.Errorf("required field %q missing from search response; got keys: %v", field, sc)
		}
	}
}

func TestMCPToolsCallAsk_RequiredOutputFieldsPresent(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	retriever := &askAudioRetrieverStub{
		askResult: model.AskResult{
			Question:         "anything",
			Answer:           "an answer",
			IndexingComplete: true,
		},
		EchoQuestion: true,
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":95,"method":"tools/call","params":{"name":"dir2mcp_ask","arguments":{"question":"anything"}}}`)
	defer func() { _ = resp.Body.Close() }()

	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Result.IsError {
		t.Fatalf("unexpected error: %#v", envelope.Result.StructuredContent)
	}
	sc := envelope.Result.StructuredContent
	for _, field := range []string{"question", "answer", "citations", "hits", "indexing_complete"} {
		if _, ok := sc[field]; !ok {
			t.Errorf("required field %q missing from ask response; got keys: %v", field, sc)
		}
	}
}

// failingListFilesStore is a minimal store stub that forces ListFiles to
// return a configured error for error-path testing.
type failingListFilesStore struct {
	err  error
	docs []model.Document
}

func (s *failingListFilesStore) Init(_ context.Context) error {
	return nil
}

func (s *failingListFilesStore) UpsertDocument(_ context.Context, _ model.Document) error {
	return nil
}

func (s *failingListFilesStore) GetDocumentByPath(_ context.Context, _ string) (model.Document, error) {
	return model.Document{}, model.ErrNotImplemented
}

func (s *failingListFilesStore) ListFiles(_ context.Context, _, _ string, _, _ int) ([]model.Document, int64, error) {
	if s.err != nil {
		return nil, 0, s.err
	}
	out := make([]model.Document, len(s.docs))
	copy(out, s.docs)
	return out, int64(len(out)), nil
}

func (s *failingListFilesStore) Close() error {
	return nil
}

// compile-time assertion: ensure our stub satisfies the Store interface used by
// mcp.WithStore.  This will fail to compile if the interface changes.
var _ model.Store = (*failingListFilesStore)(nil)

func TestMCPToolsCallListFiles_TotalReflectsHiddenFilter(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	// Materialise the stubbed docs on disk so the listFilesFiltered
	// resolvability gate (added for issue #176) treats them as real entries
	// rather than dropping them as stale.
	rootDir := t.TempDir()
	cfg.RootDir = rootDir
	if err := os.MkdirAll(filepath.Join(rootDir, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	for _, name := range []string{".DS_Store", ".claude/settings.local.json", "Gilles Deleuze.md"} {
		if err := os.WriteFile(filepath.Join(rootDir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	st := &failingListFilesStore{
		docs: []model.Document{
			{RelPath: ".DS_Store", DocType: "binary_ignored", SourceType: "filesystem", SizeBytes: 1, MTimeUnix: 1, Status: "skipped"},
			{RelPath: ".claude/settings.local.json", DocType: "data", SourceType: "filesystem", SizeBytes: 1, MTimeUnix: 1, Status: "ok"},
			{RelPath: "Gilles Deleuze.md", DocType: "md", SourceType: "filesystem", SizeBytes: 1, MTimeUnix: 1, Status: "ok"},
		},
	}
	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":91,"method":"tools/call","params":{"name":"dir2mcp_list_files","arguments":{"limit":10,"offset":0}}}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}

	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Result.IsError {
		t.Fatalf("expected success, got %#v", envelope.Result.StructuredContent)
	}
	if got := envelope.Result.StructuredContent["total"]; got != float64(1) {
		t.Fatalf("expected filtered total=1, got %#v", got)
	}
	files, ok := envelope.Result.StructuredContent["files"].([]interface{})
	if !ok {
		t.Fatalf("expected files array, got %#v", envelope.Result.StructuredContent["files"])
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 visible file, got %d", len(files))
	}
	f, ok := files[0].(map[string]interface{})
	if !ok || f["rel_path"] != "Gilles Deleuze.md" {
		t.Fatalf("unexpected visible file payload: %#v", files[0])
	}
}

func TestMCPToolsCallListFiles_IncludeHiddenTrue(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	// See TestMCPToolsCallListFiles_TotalReflectsHiddenFilter for why these
	// docs need to exist on disk under cfg.RootDir (issue #176 round-trip
	// guarantee).
	rootDir := t.TempDir()
	cfg.RootDir = rootDir
	if err := os.MkdirAll(filepath.Join(rootDir, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	for _, name := range []string{".DS_Store", ".claude/settings.local.json", "Gilles Deleuze.md"} {
		if err := os.WriteFile(filepath.Join(rootDir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	st := &failingListFilesStore{
		docs: []model.Document{
			{RelPath: ".DS_Store", DocType: "binary_ignored", SourceType: "filesystem", SizeBytes: 1, MTimeUnix: 1, Status: "skipped"},
			{RelPath: ".claude/settings.local.json", DocType: "data", SourceType: "filesystem", SizeBytes: 1, MTimeUnix: 1, Status: "ok"},
			{RelPath: "Gilles Deleuze.md", DocType: "md", SourceType: "filesystem", SizeBytes: 1, MTimeUnix: 1, Status: "ok"},
		},
	}
	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":92,"method":"tools/call","params":{"name":"dir2mcp_list_files","arguments":{"limit":10,"offset":0,"include_hidden":true}}}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}

	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Result.IsError {
		t.Fatalf("expected success, got %#v", envelope.Result.StructuredContent)
	}
	if got := envelope.Result.StructuredContent["total"]; got != float64(3) {
		t.Fatalf("expected total=3 with include_hidden=true, got %#v", got)
	}
	files, ok := envelope.Result.StructuredContent["files"].([]interface{})
	if !ok {
		t.Fatalf("expected files array, got %#v", envelope.Result.StructuredContent["files"])
	}
	if len(files) != 3 {
		t.Fatalf("expected 3 files with include_hidden=true, got %d", len(files))
	}
}

// TestMCPToolsCallListFiles_RoundTripsAdversarialNames is the regression test
// for issue #176: list_files must only emit rel_paths that round-trip through
// open_file, even when the underlying store carries adversarial filenames or
// stale rows whose source file no longer exists on disk.
//
// The "stale row" arm covers the original bug shape: a previous version of the
// ingest pipeline (or manual SQL) had registered a phantom rel_path that no
// longer corresponds to a real file. Without filtering, list_files would
// surface that path and any agent driving open_file off it would 404.
func TestMCPToolsCallListFiles_RoundTripsAdversarialNames(t *testing.T) {
	rootDir := t.TempDir()
	stateDir := filepath.Join(rootDir, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}

	adversarial := []string{
		"normal.pdf",
		"foo--bar.pdf",
		"--leading.pdf",
		"trail--.pdf",
		"with (parens).pdf",
		"with spaces.pdf",
		"with-单-unicode.pdf",
		"SINo3of2025--AML(Amendment)Regulations2025.pdf",
	}
	stalePath := "-/SINo3of2025--AML(Amendment)Regulations2025.md"

	st := setupAdversarialListFilesStore(t, rootDir, stateDir, adversarial, stalePath)

	cfg := config.Default()
	cfg.AuthMode = "none"
	cfg.RootDir = rootDir
	cfg.StateDir = stateDir
	cfg.MCPPath = protocol.DefaultMCPPath

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer server.Close()
	sessionID := initializeSession(t, server.URL+cfg.MCPPath)

	gotPaths := callListFilesAndExtractPaths(t, server.URL+cfg.MCPPath, sessionID)
	assertAdversarialPaths(t, rootDir, gotPaths, adversarial, stalePath)
}

func setupAdversarialListFilesStore(t *testing.T, rootDir, stateDir string, adversarial []string, stalePath string) *store.SQLiteStore {
	t.Helper()
	body := []byte("hello world\n")
	for _, name := range adversarial {
		if err := os.WriteFile(filepath.Join(rootDir, name), body, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	for _, name := range adversarial {
		if err := st.UpsertDocument(context.Background(), model.Document{
			RelPath:     name,
			DocType:     "pdf",
			SourceType:  "filesystem",
			SizeBytes:   int64(len(body)),
			MTimeUnix:   1,
			ContentHash: "h",
			Status:      "ok",
		}); err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
	}
	if err := st.UpsertDocument(context.Background(), model.Document{
		RelPath:     stalePath,
		DocType:     "md",
		SourceType:  "filesystem",
		SizeBytes:   3140,
		MTimeUnix:   1,
		ContentHash: "stale",
		Status:      "ok",
	}); err != nil {
		t.Fatalf("upsert stale row: %v", err)
	}
	return st
}

func callListFilesAndExtractPaths(t *testing.T, url, sessionID string) map[string]bool {
	t.Helper()
	resp := postRPC(t, url, sessionID,
		`{"jsonrpc":"2.0","id":176,"method":"tools/call","params":{"name":"dir2mcp_list_files","arguments":{"limit":50,"offset":0}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}
	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Result.IsError {
		t.Fatalf("expected success, got %#v", envelope.Result.StructuredContent)
	}
	filesRaw, ok := envelope.Result.StructuredContent["files"].([]interface{})
	if !ok {
		t.Fatalf("expected files array, got %#v", envelope.Result.StructuredContent["files"])
	}
	gotPaths := make(map[string]bool, len(filesRaw))
	for _, raw := range filesRaw {
		f, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("expected file object, got %#v", raw)
		}
		rp, ok := f["rel_path"].(string)
		if !ok || strings.TrimSpace(rp) == "" {
			t.Fatalf("expected non-empty string rel_path, got %#v", f["rel_path"])
		}
		gotPaths[rp] = true
	}
	return gotPaths
}

func assertAdversarialPaths(t *testing.T, rootDir string, gotPaths map[string]bool, adversarial []string, stalePath string) {
	t.Helper()
	if gotPaths[stalePath] {
		t.Fatalf("stale rel_path %q must not surface from list_files; got %#v", stalePath, gotPaths)
	}
	for _, name := range adversarial {
		if !gotPaths[name] {
			t.Fatalf("expected list_files to surface %q; got %#v", name, gotPaths)
		}
	}
	for path := range gotPaths {
		joined := filepath.Join(rootDir, path)
		rel, err := filepath.Rel(rootDir, joined)
		if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(path) {
			t.Fatalf("list_files returned unsafe rel_path %q (rel=%q err=%v)", path, rel, err)
		}
		if _, err := os.Stat(joined); err != nil {
			t.Fatalf("list_files returned non-resolvable rel_path %q: %v", path, err)
		}
	}
}

// TestMCPToolsCallOpenFile_PDFNoSpan_DocumentSpan asserts that calling
// open_file on a .pdf without span arguments returns the OCR text and tags
// the structured content with span.kind=document so callers can distinguish
// a full-document representation from a paged slice. Regression test for
// issue #177 — without the fix the handler returned raw PDF bytes via the
// content[].text field (an MCP spec violation) and emitted no span at all.
func TestMCPToolsCallOpenFile_PDFNoSpan_DocumentSpan(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	retriever := &askAudioRetrieverStub{
		openFileConfigured: true,
		openFileContent:    "# Act\n\nARRANGEMENT OF SECTIONS",
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":94,"method":"tools/call","params":{"name":"dir2mcp_open_file","arguments":{"rel_path":"act.pdf"}}}`)
	defer func() { _ = resp.Body.Close() }()

	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			Content           []map[string]any       `json:"content"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Result.IsError {
		t.Fatalf("expected success, got isError=true: %#v", envelope.Result.StructuredContent)
	}
	span, ok := envelope.Result.StructuredContent["span"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected structuredContent.span, got %#v", envelope.Result.StructuredContent)
	}
	if span["kind"] != "document" {
		t.Fatalf("expected span.kind=document, got %#v", span)
	}
	if envelope.Result.StructuredContent["doc_type"] != "pdf" {
		t.Fatalf("expected doc_type=pdf, got %#v", envelope.Result.StructuredContent["doc_type"])
	}
	if got, _ := envelope.Result.StructuredContent["content"].(string); !strings.Contains(got, "ARRANGEMENT OF SECTIONS") {
		t.Fatalf("expected OCR markdown content, got %q", got)
	}
}

// TestMCPToolsCallOpenFile_PDFOCRNotReady asserts that when the retriever
// reports that the OCR cache hasn't been populated yet, the tool surfaces
// OCR_NOT_READY rather than swallowing the error or returning raw bytes.
func TestMCPToolsCallOpenFile_PDFOCRNotReady(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	retriever := &askAudioRetrieverStub{
		openFileConfigured: true,
		openFileErr:        model.ErrOCRNotReady,
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":95,"method":"tools/call","params":{"name":"dir2mcp_open_file","arguments":{"rel_path":"missing.pdf"}}}`)
	defer func() { _ = resp.Body.Close() }()

	assertToolCallErrorCode(t, resp, "OCR_NOT_READY")
}


func TestMCPToolsCallOpenFile_RejectsBinaryContent(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	// content with an embedded NUL byte is the canonical "actually binary"
	// signal the handler must refuse, regardless of doc_type. Since #177 the
	// audio-extension shortcut was dropped because audio is now expected to
	// resolve to a transcript representation upstream.
	retriever := &askAudioRetrieverStub{
		openFileConfigured: true,
		openFileContent:    "raw\x00bytes",
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":93,"method":"tools/call","params":{"name":"dir2mcp_open_file","arguments":{"rel_path":"recording.mp3"}}}`)
	defer func() { _ = resp.Body.Close() }()

	assertToolCallErrorCodeAndMessage(t, resp, "DOC_TYPE_UNSUPPORTED", []string{"transcribe"}, nil)
}

// TestMCPToolsCallOpenFile_RejectsBinaryContent_PDFMessage asserts that when
// open_file rejects a binary payload for a .pdf rel_path, the
// DOC_TYPE_UNSUPPORTED message mentions paging (the actionable fix for PDFs)
// and does *not* mention transcribe (which is the wrong suggestion for PDFs).
// Regression test for the PR #180 review finding that the message hardcoded
// "use transcribe for audio files" for every doc_type.
func TestMCPToolsCallOpenFile_RejectsBinaryContent_PDFMessage(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	retriever := &askAudioRetrieverStub{
		openFileConfigured: true,
		openFileContent:    "%PDF-1.4\x00stream",
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":96,"method":"tools/call","params":{"name":"dir2mcp_open_file","arguments":{"rel_path":"act.pdf"}}}`)
	defer func() { _ = resp.Body.Close() }()

	assertToolCallErrorCodeAndMessage(t, resp, "DOC_TYPE_UNSUPPORTED", []string{"page"}, []string{"transcribe"})
}

// TestMCPToolsCallOpenFile_RejectsBinaryContent_GenericMessage asserts the
// fallback DOC_TYPE_UNSUPPORTED message for non-audio / non-pdf doc_types is
// generic — no per-format suggestion that would be misleading.
func TestMCPToolsCallOpenFile_RejectsBinaryContent_GenericMessage(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	retriever := &askAudioRetrieverStub{
		openFileConfigured: true,
		openFileContent:    "junk\x00bytes",
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	// .bin → inferDocType returns "unknown"; should hit the generic branch.
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":97,"method":"tools/call","params":{"name":"dir2mcp_open_file","arguments":{"rel_path":"blob.bin"}}}`)
	defer func() { _ = resp.Body.Close() }()

	assertToolCallErrorCodeAndMessage(t, resp, "DOC_TYPE_UNSUPPORTED", nil, []string{"transcribe", "page"})
}

// assertToolCallErrorCode validates that a tools/call response returned a
// tool-level error payload with the expected canonical error code.
func assertToolCallErrorCode(t *testing.T, resp *http.Response, wantCode string) {
	t.Helper()
	assertToolCallErrorCodeAndMessage(t, resp, wantCode, nil, nil)
}

// assertToolCallErrorCodeAndMessage extends assertToolCallErrorCode with
// optional substring assertions on structuredContent.error.message. Pass
// mustHave/mustNotHave nil/empty to skip the message check.
func assertToolCallErrorCodeAndMessage(t *testing.T, resp *http.Response, wantCode string, mustHave, mustNotHave []string) {
	t.Helper()

	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}

	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
		Error interface{} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if envelope.Error != nil {
		t.Fatalf("expected tool-level error result, got top-level error: %#v", envelope.Error)
	}
	if !envelope.Result.IsError {
		t.Fatalf("expected isError=true, got false with structuredContent=%#v", envelope.Result.StructuredContent)
	}

	errObjRaw, ok := envelope.Result.StructuredContent["error"]
	if !ok {
		t.Fatalf("expected structuredContent.error, got %#v", envelope.Result.StructuredContent)
	}
	errObj, ok := errObjRaw.(map[string]interface{})
	if !ok {
		t.Fatalf("expected structuredContent.error object, got %#v", errObjRaw)
	}
	gotCode, ok := errObj["code"].(string)
	if !ok {
		t.Fatalf("expected structuredContent.error.code string, got %#v", errObj["code"])
	}
	if gotCode != wantCode {
		t.Fatalf("unexpected error code: got=%q want=%q full_error=%#v", gotCode, wantCode, errObj)
	}
	if len(mustHave) == 0 && len(mustNotHave) == 0 {
		return
	}
	gotMsg, _ := errObj["message"].(string)
	for _, want := range mustHave {
		if !strings.Contains(gotMsg, want) {
			t.Fatalf("error message missing substring %q: got=%q full_error=%#v", want, gotMsg, errObj)
		}
	}
	for _, bad := range mustNotHave {
		if strings.Contains(gotMsg, bad) {
			t.Fatalf("error message must not contain %q: got=%q full_error=%#v", bad, gotMsg, errObj)
		}
	}
}

// initializeSession performs MCP initialize and returns the session id used
// for subsequent tools/list and tools/call requests.
func initializeSession(t *testing.T, url string) string {
	t.Helper()
	resp := postRPC(t, url, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}
	sessionID := resp.Header.Get(protocol.MCPSessionHeader)
	if sessionID == "" {
		t.Fatalf("missing %s header", protocol.MCPSessionHeader)
	}
	return sessionID
}

// postRPC sends a JSON-RPC POST request to the MCP endpoint with an optional
// MCP session header.
func postRPC(t *testing.T, url, sessionID, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set(protocol.MCPSessionHeader, sessionID)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

type askAudioRetrieverStub struct {
	askResult model.AskResult
	askErr    error
	stats     model.Stats
	statsErr  error

	statsConfigured bool
	// values produced by Search mode
	searchHits []model.SearchHit
	searchErr  error
	OnSearch   func(query model.SearchQuery) ([]model.SearchHit, error)
	// EchoQuestion instructs the stub to copy the incoming question
	// into the returned AskResult.Question field. This mirrors the
	// behavior of the previous helper that echoed the input question
	// automatically. Tests can also supply an OnAsk
	// callback to compute a custom question string.
	EchoQuestion bool
	OnAsk        func(question string) string

	// tracking for assertions (read from HTTP handler goroutines)
	searchCalled atomic.Bool
	askCalled    atomic.Bool
	statsCalled  atomic.Bool

	// indexing state for the new accessor
	// this field is only written during initialization and then read by
	// handlers, so it does not currently need to be atomic.  keep as a
	// plain bool for now.
	indexingComplete bool

	// open_file stub; when openFileConfigured is true, OpenFile returns
	// openFileContent/openFileErr instead of ErrNotImplemented.
	openFileConfigured bool
	openFileContent    string
	openFileErr        error
}

func (s *askAudioRetrieverStub) Search(_ context.Context, q model.SearchQuery) ([]model.SearchHit, error) {
	s.searchCalled.Store(true)
	if s.OnSearch != nil {
		return s.OnSearch(q)
	}
	if s.searchErr != nil {
		return nil, s.searchErr
	}
	if s.searchHits != nil {
		return s.searchHits, nil
	}
	return nil, model.ErrNotImplemented
}

func (s *askAudioRetrieverStub) Ask(_ context.Context, question string, _ model.SearchQuery) (model.AskResult, error) {
	s.askCalled.Store(true)
	if s.askErr != nil {
		return model.AskResult{}, s.askErr
	}
	res := s.askResult
	// override the question if requested by the test
	if s.OnAsk != nil {
		res.Question = s.OnAsk(question)
	} else if s.EchoQuestion {
		res.Question = question
	}
	return res, nil
}

func (s *askAudioRetrieverStub) OpenFile(_ context.Context, _ string, _ model.Span, _ int) (string, error) {
	if s.openFileConfigured {
		return s.openFileContent, s.openFileErr
	}
	return "", model.ErrNotImplemented
}

func (s *askAudioRetrieverStub) Stats(_ context.Context) (model.Stats, error) {
	s.statsCalled.Store(true)
	if s.statsErr != nil {
		return model.Stats{}, s.statsErr
	}
	if !s.statsConfigured {
		return model.Stats{}, model.ErrNotImplemented
	}
	return s.stats, nil
}

func (s *askAudioRetrieverStub) IndexingComplete(_ context.Context) (bool, error) {
	return s.indexingComplete, nil
}

// compile-time assertion that askAudioRetrieverStub satisfies the Retriever
// interface; helps catch missing methods during refactoring.
var _ model.Retriever = (*askAudioRetrieverStub)(nil)

type fakeTTSSynthesizer struct {
	audio []byte
	err   error
}

func (f *fakeTTSSynthesizer) Synthesize(_ context.Context, _ string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.audio, nil
}

type toolContentEnvelope struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
}

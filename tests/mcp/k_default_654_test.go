package tests

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
)

// kRecordingRetriever records the k that reached retrieval, whichever entry
// point the tool used: Search (search, and any ask-family request served as
// search_only), Ask (ask, ask_audio, transcribe_and_ask) or Related.
//
// It is written from the HTTP handler goroutine and read from the test
// goroutine, so every field is guarded.
type kRecordingRetriever struct {
	mu        sync.Mutex
	searchK   int
	askK      int
	relatedK  int
	askCalls  int
	hits      []model.SearchHit
	askResult model.AskResult
}

func (r *kRecordingRetriever) Search(_ context.Context, q model.SearchQuery) ([]model.SearchHit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.searchK = q.K
	return r.hits, nil
}

func (r *kRecordingRetriever) Ask(_ context.Context, question string, q model.SearchQuery) (model.AskResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.askK = q.K
	r.askCalls++
	result := r.askResult
	result.Question = question
	return result, nil
}

func (r *kRecordingRetriever) Related(_ context.Context, q model.RelatedQuery) (model.RelatedResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.relatedK = q.K
	return model.RelatedResult{
		SourceChunkID:    q.SourceChunkID,
		HasSourceChunkID: q.SourceChunkID != 0,
		SourceRelPath:    "seed.md",
		K:                q.K,
		IndexUsed:        "text",
		IndexingComplete: true,
	}, nil
}

func (r *kRecordingRetriever) OpenFile(context.Context, string, model.Span, int) (string, error) {
	return "", model.ErrNotImplemented
}
func (r *kRecordingRetriever) Stats(context.Context) (model.Stats, error) {
	return model.Stats{}, model.ErrNotImplemented
}
func (r *kRecordingRetriever) IndexingComplete(context.Context) (bool, error) { return true, nil }

// observedK returns the k recorded by whichever entry point ran.
func (r *kRecordingRetriever) observedK() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.askK != 0:
		return r.askK
	case r.relatedK != 0:
		return r.relatedK
	default:
		return r.searchK
	}
}

func (r *kRecordingRetriever) askCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.askCalls
}

var _ model.Retriever = (*kRecordingRetriever)(nil)
var _ model.RelatedSearcher = (*kRecordingRetriever)(nil)

// kRetrievalTools are the four in-process tools that take a k and reach
// retrieval without a corpus. dir2mcp_transcribe_and_ask is the fifth tool in
// the SPEC §9.1 scope; it needs an indexed audio document, so it has its own
// test below.
var kRetrievalTools = []struct {
	name string
	// args renders the tool arguments. k <= 0 OMITS the field entirely, which
	// is the case rag.k_default resolves.
	args func(k int) string
}{
	{
		name: "dir2mcp_search",
		args: func(k int) string { return `{"query":"q"` + kArgument(k) + `}` },
	},
	{
		name: "dir2mcp_ask",
		args: func(k int) string { return `{"question":"q"` + kArgument(k) + `}` },
	},
	{
		name: "dir2mcp_ask_audio",
		args: func(k int) string { return `{"question":"q"` + kArgument(k) + `}` },
	},
	{
		name: "dir2mcp_related",
		args: func(k int) string { return `{"chunk_id":1` + kArgument(k) + `}` },
	},
}

// kArgument renders the k member of a tool argument object, or nothing at all
// when k <= 0.
func kArgument(k int) string {
	if k <= 0 {
		return ""
	}
	return `,"k":` + strconv.Itoa(k)
}

// callToolExpectingSuccess runs one tools/call against a server built on the
// given config and retriever, and fails the test on any transport-level error.
func callToolExpectingSuccess(t *testing.T, cfg config.Config, retriever model.Retriever, tool, args string) {
	t.Helper()
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()
	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":`+args+`}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: status=%d body=%s", tool, resp.StatusCode, string(payload))
	}
}

// TestMCPOmittedK_ResolvesToConfiguredKDefault pins the precedence SPEC §9.1
// fixes: an omitted k resolves to rag.k_default, on EVERY tool that takes a k.
// The setting is one statement about how much evidence this corpus needs, so a
// server must not honor it on some surfaces and not others.
//
// On main rag.k_default was loaded and persisted but never read, so all four
// rows retrieved the hardcoded 15 instead.
func TestMCPOmittedK_ResolvesToConfiguredKDefault(t *testing.T) {
	const configuredK = 23

	for _, tool := range kRetrievalTools {
		t.Run(tool.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.AuthMode = "none"
			cfg.RAGKDefault = configuredK

			retriever := &kRecordingRetriever{}
			callToolExpectingSuccess(t, cfg, retriever, tool.name, tool.args(0))
			if got := retriever.observedK(); got != configuredK {
				t.Fatalf("%s with omitted k retrieved k=%d, want the configured rag.k_default=%d", tool.name, got, configuredK)
			}
		})
	}
}

// TestMCPSuppliedK_WinsOverConfiguredKDefault pins step 1 of the same
// precedence: the REQUEST field beats the configured default. Wiring
// rag.k_default must not make a caller's explicit k negotiable.
func TestMCPSuppliedK_WinsOverConfiguredKDefault(t *testing.T) {
	const (
		configuredK = 23
		requestedK  = 4
	)

	for _, tool := range kRetrievalTools {
		t.Run(tool.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.AuthMode = "none"
			cfg.RAGKDefault = configuredK

			retriever := &kRecordingRetriever{}
			callToolExpectingSuccess(t, cfg, retriever, tool.name, tool.args(requestedK))
			if got := retriever.observedK(); got != requestedK {
				t.Fatalf("%s with k=%d retrieved k=%d; a supplied k must win over rag.k_default", tool.name, requestedK, got)
			}
		})
	}
}

// TestMCPTranscribeAndAsk_OmittedKResolvesToConfiguredKDefault covers the fifth
// tool in the SPEC §9.1 scope. It needs an indexed audio document and a
// transcription upstream, so it cannot join the table above.
func TestMCPTranscribeAndAsk_OmittedKResolvesToConfiguredKDefault(t *testing.T) {
	const configuredK = 23

	cfg, st, _ := setupMCPToolStore(t, "voice.wav", "audio", []byte("RIFF0000WAVEfmt data"))
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
	cfg = withMistralUpstream(t, cfg, "mistral-ocr", upstream.URL)
	cfg.RAGKDefault = configuredK

	retriever := &kRecordingRetriever{askResult: model.AskResult{Answer: "alpha answer"}}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_transcribe_and_ask","arguments":{"rel_path":"voice.wav","question":"what is alpha?"}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(payload))
	}
	if got := retriever.observedK(); got != configuredK {
		t.Fatalf("transcribe_and_ask with omitted k retrieved k=%d, want the configured rag.k_default=%d", got, configuredK)
	}
}

// askStructuredContent decodes the structuredContent of a successful ask-family
// tools/call.
func askStructuredContent(t *testing.T, resp *http.Response) map[string]interface{} {
	t.Helper()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("decode: %v body=%s", err, payload)
	}
	if envelope.Result.IsError {
		t.Fatalf("unexpected tool error: %s", payload)
	}
	return envelope.Result.StructuredContent
}

// assertSearchOnlyShape checks the SPEC §9.4 no-answer payload: the response
// SHAPE is unchanged, so `answer` and `citations` are present and empty rather
// than absent, and the hits are still returned.
func assertSearchOnlyShape(t *testing.T, structured map[string]interface{}, wantHits int) {
	t.Helper()
	answer, ok := structured["answer"].(string)
	if !ok {
		t.Fatalf("answer is absent or not a string: %#v", structured["answer"])
	}
	if answer != "" {
		t.Fatalf("answer=%q, want empty with generation disabled", answer)
	}
	citations, ok := structured["citations"].([]interface{})
	if !ok {
		t.Fatalf("citations is absent or not an array: %#v", structured["citations"])
	}
	if len(citations) != 0 {
		t.Fatalf("citations has %d entries, want none with generation disabled", len(citations))
	}
	hits, ok := structured["hits"].([]interface{})
	if !ok {
		t.Fatalf("hits is absent or not an array: %#v", structured["hits"])
	}
	if len(hits) != wantHits {
		t.Fatalf("hits=%d want %d: generation is disabled, retrieval is not", len(hits), wantHits)
	}
}

// TestMCPGenerateAnswerFalse_IsServedAsSearchOnly pins the §9.4 clause the spec
// now names: `rag.generate_answer: false` behaves exactly as
// `mode=search_only`. Either condition is sufficient, so a request cannot turn
// generation back on, and `mode=answer` against it is SERVED as search_only
// rather than refused.
//
// On main the key was loaded and persisted but never read, so every row called
// the generator and returned an answer.
func TestMCPGenerateAnswerFalse_IsServedAsSearchOnly(t *testing.T) {
	hits := []model.SearchHit{
		{ChunkID: 1, RelPath: "a.md", Snippet: "alpha", Score: 0.9},
		{ChunkID: 2, RelPath: "b.md", Snippet: "beta", Score: 0.8},
	}

	for _, tc := range []struct {
		name string
		tool string
		args string
	}{
		{name: "ask/mode=answer", tool: "dir2mcp_ask", args: `{"question":"q","mode":"answer"}`},
		{name: "ask/mode omitted", tool: "dir2mcp_ask", args: `{"question":"q"}`},
		{name: "ask_audio/mode=answer", tool: "dir2mcp_ask_audio", args: `{"question":"q","mode":"answer"}`},
		{name: "ask_audio/mode omitted", tool: "dir2mcp_ask_audio", args: `{"question":"q"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.AuthMode = "none"
			cfg.RAGGenerateAnswer = false

			retriever := &kRecordingRetriever{
				hits:      hits,
				askResult: model.AskResult{Answer: "generated answer", Citations: []model.Citation{{ChunkID: 1, RelPath: "a.md"}}},
			}
			server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
			defer server.Close()
			sessionID := initializeSession(t, server.URL+cfg.MCPPath)
			resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
				`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tc.tool+`","arguments":`+tc.args+`}}`)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				payload, _ := io.ReadAll(resp.Body)
				t.Fatalf("status=%d body=%s", resp.StatusCode, string(payload))
			}
			assertSearchOnlyShape(t, askStructuredContent(t, resp), len(hits))
			if calls := retriever.askCallCount(); calls != 0 {
				t.Fatalf("Ask ran %d times; generation is disabled, so no prompt may be assembled", calls)
			}
		})
	}
}

// TestMCPGenerateAnswerTrue_StillAnswers is the converse guard: the default
// deployment still generates, so disabling generation cannot be mistaken for
// the shipped behavior.
func TestMCPGenerateAnswerTrue_StillAnswers(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"

	retriever := &kRecordingRetriever{askResult: model.AskResult{Answer: "generated answer"}}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()
	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_ask","arguments":{"question":"q"}}}`)
	defer func() { _ = resp.Body.Close() }()
	structured := askStructuredContent(t, resp)
	if got, _ := structured["answer"].(string); got != "generated answer" {
		t.Fatalf("answer=%q want the generated answer", got)
	}
}

// TestMCPTranscribeAndAsk_GenerateAnswerFalseWithholdsGeneration extends the
// §9.4 rule to the third ask-family surface. The tool has no `mode` argument, so
// the server setting is the only condition, and an operator who disabled
// generation must not have a chat provider called behind their back.
func TestMCPTranscribeAndAsk_GenerateAnswerFalseWithholdsGeneration(t *testing.T) {
	cfg, st, _ := setupMCPToolStore(t, "voice.wav", "audio", []byte("RIFF0000WAVEfmt data"))
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
	cfg = withMistralUpstream(t, cfg, "mistral-ocr", upstream.URL)
	cfg.RAGGenerateAnswer = false

	retriever := &kRecordingRetriever{
		hits:      []model.SearchHit{{ChunkID: 1, RelPath: "voice.wav", Snippet: "alpha in transcript"}},
		askResult: model.AskResult{Answer: "generated answer"},
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever, mcp.WithStore(st)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_transcribe_and_ask","arguments":{"rel_path":"voice.wav","question":"what is alpha?"}}}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(payload))
	}
	structured := askStructuredContent(t, resp)
	assertSearchOnlyShape(t, structured, 1)
	if calls := retriever.askCallCount(); calls != 0 {
		t.Fatalf("Ask ran %d times; generation is disabled", calls)
	}
	// The transcript provenance is still reported: only generation is withheld.
	if _, ok := structured["transcript_provider"].(string); !ok {
		t.Fatalf("transcript_provider missing from a search-only transcribe_and_ask: %#v", structured)
	}
}

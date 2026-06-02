package tests

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/gemini"
	"github.com/dirstral/dir2mcp/internal/model"
)

func newClient(url string) *gemini.Client {
	c := gemini.NewClient(url, "test-key")
	c.InitialBackoff = time.Millisecond
	c.MaxBackoff = time.Millisecond
	return c
}

// nativeEmbedReq is the native batchEmbedContents request body shape.
type nativeEmbedReq struct {
	Requests []struct {
		Model   string `json:"model"`
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		TaskType             string `json:"taskType"`
		OutputDimensionality *int   `json:"outputDimensionality"`
	} `json:"requests"`
}

func decodeNativeEmbed(t *testing.T, r *http.Request) nativeEmbedReq {
	t.Helper()
	var req nativeEmbedReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Fatalf("decode native embed request: %v", err)
	}
	return req
}

// embedRespFor returns the native batchEmbedContents response: one
// embedding per request, IN REQUEST ORDER (the native batch API preserves
// order), with values[0] = first byte of the request's text so order
// preservation across batches is verifiable.
func embedRespFor(req nativeEmbedReq) map[string]any {
	embs := make([]map[string]any, len(req.Requests))
	for i, r := range req.Requests {
		first := 0.0
		if len(r.Content.Parts) > 0 && r.Content.Parts[0].Text != "" {
			first = float64(r.Content.Parts[0].Text[0])
		}
		embs[i] = map[string]any{"values": []float64{first, 0.5}}
	}
	return map[string]any{"embeddings": embs}
}

func TestEmbed_BatchesPreservesOrderAndRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Embeddings use the native surface: base (no /openai here) +
		// /models/{model}:batchEmbedContents, authenticated with the
		// x-goog-api-key header (versioned base covered by
		// TestEmbed_NativeEndpointPath).
		if want := "/models/gemini-embedding-001:batchEmbedContents"; r.URL.Path != want {
			t.Errorf("path = %s, want %s", r.URL.Path, want)
		}
		if got := r.Header.Get("x-goog-api-key"); got != "test-key" {
			t.Errorf("api key header = %q", got)
		}
		n := atomic.AddInt32(&calls, 1)
		if n == 1 { // first batch: one transient 429 then success
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(embedRespFor(decodeNativeEmbed(t, r)))
	}))
	defer srv.Close()

	c := newClient(srv.URL)
	c.BatchSize = 2 // forces 2 batches for 3 inputs (a,b) then (c)
	in := []string{"a", "b", "c"}
	vecs, err := c.Embed(context.Background(), "gemini-embedding-001", model.EmbedDocument, in)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("got %d vectors, want 3", len(vecs))
	}
	// Each output vector must correspond to its input across batch
	// boundaries (values[0] == first byte of the input string).
	for i, s := range in {
		if vecs[i][0] != float32(s[0]) {
			t.Fatalf("order not preserved at %d: got %v, want first-byte of %q", i, vecs[i], s)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 3 { // 1 retry (429) + 2 batches
		t.Fatalf("calls = %d, want 3", n)
	}
}

// TestEmbed_NativeEndpointPath verifies that embeddings target the native
// surface: the configured OpenAI-compatible base (".../v1beta/openai") has
// its trailing "/openai" dropped, then "/models/{model}:batchEmbedContents"
// is appended — so an operator base of ".../v1beta/openai" yields
// ".../v1beta/models/m:batchEmbedContents".
func TestEmbed_NativeEndpointPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(embedRespFor(decodeNativeEmbed(t, r)))
	}))
	defer srv.Close()

	c := gemini.NewClient(srv.URL+"/v1beta/openai", "test-key")
	if _, err := c.Embed(context.Background(), "m", model.EmbedDocument, []string{"x"}); err != nil {
		t.Fatalf("embed: %v", err)
	}
	if want := "/v1beta/models/m:batchEmbedContents"; gotPath != want {
		t.Fatalf("path = %q, want %q", gotPath, want)
	}
}

func TestEmbed_MissingKeyIsNonRetryableAuth(t *testing.T) {
	c := gemini.NewClient("http://127.0.0.1:0", "")
	_, err := c.Embed(context.Background(), "m", model.EmbedQuery, []string{"x"})
	var pe *model.ProviderError
	if !asProviderErr(err, &pe) || pe.Code != "GEMINI_AUTH" || pe.Retryable {
		t.Fatalf("want non-retryable GEMINI_AUTH, got %v", err)
	}
}

func TestEmbed_EmptyInputsNoCall(t *testing.T) {
	c := gemini.NewClient("http://127.0.0.1:0", "k")
	v, err := c.Embed(context.Background(), "m", model.EmbedDocument, nil)
	if err != nil || len(v) != 0 {
		t.Fatalf("want ([],nil), got %v %v", v, err)
	}
}

// TestEmbed_AsymmetricRoleMapsTaskType pins SPEC 8.1.5: Gemini is an
// asymmetric embedder, so the role maps onto taskType —
// document → RETRIEVAL_DOCUMENT, query → RETRIEVAL_QUERY — and a query
// against the configured code model → CODE_RETRIEVAL_QUERY.
func TestEmbed_AsymmetricRoleMapsTaskType(t *testing.T) {
	var gotTask string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeNativeEmbed(t, r)
		gotTask = req.Requests[0].TaskType
		_ = json.NewEncoder(w).Encode(embedRespFor(req))
	}))
	defer srv.Close()

	c := newClient(srv.URL)
	c.CodeEmbedModel = "code-embed"

	cases := []struct {
		name      string
		modelName string
		role      model.EmbedRole
		want      string
	}{
		{"text document", "text-embed", model.EmbedDocument, "RETRIEVAL_DOCUMENT"},
		{"text query", "text-embed", model.EmbedQuery, "RETRIEVAL_QUERY"},
		{"code document", "code-embed", model.EmbedDocument, "RETRIEVAL_DOCUMENT"},
		{"code query", "code-embed", model.EmbedQuery, "CODE_RETRIEVAL_QUERY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Embed(context.Background(), tc.modelName, tc.role, []string{"hi"}); err != nil {
				t.Fatalf("embed: %v", err)
			}
			if gotTask != tc.want {
				t.Fatalf("taskType = %q, want %q", gotTask, tc.want)
			}
		})
	}
}

// TestEmbed_OutputDimensionalityAndNormalize pins SPEC 8.1.6: a requested
// output dimension is sent as outputDimensionality, and the returned
// vector is L2-normalized to unit length.
func TestEmbed_OutputDimensionalityAndNormalize(t *testing.T) {
	var gotDim *int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeNativeEmbed(t, r)
		gotDim = req.Requests[0].OutputDimensionality
		// Return a non-unit vector [3,4] (norm 5) to prove normalization.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": []map[string]any{{"values": []float64{3, 4}}},
		})
	}))
	defer srv.Close()

	c := newClient(srv.URL)
	c.EmbedTextDim = 768
	vecs, err := c.Embed(context.Background(), "m", model.EmbedDocument, []string{"x"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if gotDim == nil || *gotDim != 768 {
		t.Fatalf("outputDimensionality = %v, want 768", gotDim)
	}
	// [3,4]/5 = [0.6,0.8]; norm must be ~1.
	got := vecs[0]
	if d := got[0] - 0.6; d > 1e-6 || d < -1e-6 {
		t.Fatalf("vec[0] = %v, want 0.6 (normalized)", got[0])
	}
	var norm float64
	for _, v := range got {
		norm += float64(v) * float64(v)
	}
	if norm < 0.999 || norm > 1.001 {
		t.Fatalf("vector not unit length: norm^2 = %v", norm)
	}
}

func TestGenerate_HappyPathAndStructuredContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": []map[string]any{
					{"type": "text", "text": "hello"},
					{"type": "text", "text": "world"},
				}}},
			},
		})
	}))
	defer srv.Close()

	out, err := newClient(srv.URL).Generate(context.Background(), "hi")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// Multiple text parts are newline-joined (mirrors internal/mistral).
	if out != "hello\nworld" {
		t.Fatalf("content = %q, want %q", out, "hello\nworld")
	}
}

// TestGenerate_UsesGenerationTimeoutNotDefault locks the same invariant
// as internal/mistral: NewClient always sets HTTPClient (30s), so the
// per-call GenerationTimeout must still be applied. With a 50ms
// GenerationTimeout and a 300ms-slow server, Generate must fail fast
// (~50ms) rather than hang toward the 30s default.
func TestGenerate_UsesGenerationTimeoutNotDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "late"}}},
		})
	}))
	defer srv.Close()

	c := gemini.NewClient(srv.URL, "test-key") // default HTTPClient: 30s
	c.MaxRetries = 0
	c.GenerationTimeout = 50 * time.Millisecond

	start := time.Now()
	_, err := c.Generate(context.Background(), "hi")
	if err == nil {
		t.Fatal("want timeout error, got nil")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Generate did not honor GenerationTimeout (took %s)", elapsed)
	}
}

func TestGenerate_NoChoicesIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{}})
	}))
	defer srv.Close()
	_, err := newClient(srv.URL).Generate(context.Background(), "hi")
	var pe *model.ProviderError
	if !asProviderErr(err, &pe) || pe.Code != "GEMINI_FAILED" {
		t.Fatalf("want GEMINI_FAILED, got %v", err)
	}
}

func TestHTTPErrorMapping(t *testing.T) {
	cases := []struct {
		status    int
		wantCode  string
		retryable bool
	}{
		{http.StatusUnauthorized, "GEMINI_AUTH", false},
		{http.StatusForbidden, "GEMINI_AUTH", false},
		{http.StatusTooManyRequests, "GEMINI_RATE_LIMIT", true},
		{http.StatusBadGateway, "GEMINI_FAILED", true},
		{http.StatusBadRequest, "GEMINI_FAILED", false},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
		}))
		c := newClient(srv.URL)
		c.MaxRetries = 0
		_, err := c.Embed(context.Background(), "m", model.EmbedDocument, []string{"x"})
		srv.Close()
		var pe *model.ProviderError
		if !asProviderErr(err, &pe) || pe.Code != tc.wantCode || pe.Retryable != tc.retryable {
			t.Fatalf("status %d: got %v, want %s retryable=%v", tc.status, err, tc.wantCode, tc.retryable)
		}
	}
}

func TestGenerate_RequestShapeAndStringContent(t *testing.T) {
	var gotPath, gotAuth, gotModel string
	var gotMsgs int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel, gotMsgs = req.Model, len(req.Messages)
		// Primary OpenAI-compatible shape: content is a plain string.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "pong"}}},
		})
	}))
	defer srv.Close()

	c := newClient(srv.URL)
	c.DefaultChatModel = "gemini-custom"
	out, err := c.Generate(context.Background(), "ping")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if out != "pong" {
		t.Fatalf("content = %q, want pong (string-content path)", out)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("path = %q, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotModel != "gemini-custom" || gotMsgs != 1 {
		t.Fatalf("request shape: model=%q messages=%d", gotModel, gotMsgs)
	}
}

func TestGenerate_MissingKeyIsNonRetryableAuth(t *testing.T) {
	c := gemini.NewClient("http://127.0.0.1:0", "")
	_, err := c.Generate(context.Background(), "hi")
	var pe *model.ProviderError
	if !asProviderErr(err, &pe) || pe.Code != "GEMINI_AUTH" || pe.Retryable {
		t.Fatalf("want non-retryable GEMINI_AUTH, got %v", err)
	}
}

// TestEmbed_DefaultModelFallback covers the empty-model fallback to the
// (overridable) DefaultEmbedModel — the request body must carry it.
func TestEmbed_DefaultModelFallback(t *testing.T) {
	var gotModel, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		req := decodeNativeEmbed(t, r)
		gotModel = req.Requests[0].Model
		_ = json.NewEncoder(w).Encode(embedRespFor(req))
	}))
	defer srv.Close()

	c := newClient(srv.URL)
	c.DefaultEmbedModel = "embed-override"
	if _, err := c.Embed(context.Background(), "", model.EmbedDocument, []string{"x"}); err != nil {
		t.Fatalf("embed: %v", err)
	}
	// The native request qualifies the model as "models/{model}" and the
	// URL path carries the same model id.
	if gotModel != "models/embed-override" {
		t.Fatalf("model = %q, want models/embed-override (DefaultEmbedModel fallback)", gotModel)
	}
	if want := "/models/embed-override:batchEmbedContents"; gotPath != want {
		t.Fatalf("path = %q, want %q", gotPath, want)
	}
}

// TestNonRetryableStopsImmediately asserts a non-retryable HTTP status
// (400) is attempted exactly once even with retries enabled — for both
// Embed and Generate.
func TestNonRetryableStopsImmediately(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*gemini.Client) error
	}{
		{"embed", func(c *gemini.Client) error {
			_, e := c.Embed(context.Background(), "m", model.EmbedDocument, []string{"x"})
			return e
		}},
		{"generate", func(c *gemini.Client) error {
			_, e := c.Generate(context.Background(), "hi")
			return e
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.WriteHeader(http.StatusBadRequest)
			}))
			defer srv.Close()
			c := newClient(srv.URL) // MaxRetries default (3)
			if err := tc.call(c); err == nil {
				t.Fatal("want error")
			}
			if n := atomic.LoadInt32(&calls); n != 1 {
				t.Fatalf("non-retryable 400 attempted %d times, want 1", n)
			}
		})
	}
}

func TestTranscribeAndSynthesize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/audio/transcriptions":
			if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/form-data") {
				t.Errorf("transcribe content-type = %q", ct)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"text": "hello transcript"})
		case "/audio/speech":
			_, _ = w.Write([]byte("AUDIOBYTES"))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := newClient(srv.URL)
	txt, err := c.Transcribe(context.Background(), "a.wav", []byte("pcm"))
	if err != nil || txt != "hello transcript" {
		t.Fatalf("Transcribe = %q, %v", txt, err)
	}
	audio, err := c.Synthesize(context.Background(), "say this")
	if err != nil || string(audio) != "AUDIOBYTES" {
		t.Fatalf("Synthesize = %q, %v", audio, err)
	}
}

func TestAudioMissingKeyAndEmptyInput(t *testing.T) {
	nokey := gemini.NewClient("http://127.0.0.1:0", "")
	if _, err := nokey.Transcribe(context.Background(), "a.wav", []byte("x")); err == nil {
		t.Error("Transcribe missing key must error")
	}
	if _, err := nokey.Synthesize(context.Background(), "x"); err == nil {
		t.Error("Synthesize missing key must error")
	}
	keyed := gemini.NewClient("http://127.0.0.1:0", "k")
	if _, err := keyed.Transcribe(context.Background(), "a.wav", nil); err == nil {
		t.Error("Transcribe empty data must error (no HTTP call)")
	}
	if _, err := keyed.Synthesize(context.Background(), "  "); err == nil {
		t.Error("Synthesize empty text must error (no HTTP call)")
	}
}

// asProviderErr unwraps via errors.As (repo convention) so wrapped
// provider errors are still recognized.
func asProviderErr(err error, target **model.ProviderError) bool {
	if !errors.As(err, target) {
		return false
	}
	return strings.HasPrefix((*target).Code, "GEMINI_")
}

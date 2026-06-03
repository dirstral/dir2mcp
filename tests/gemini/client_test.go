package tests

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
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
				Text       string `json:"text"`
				InlineData *struct {
					MimeType string `json:"mimeType"`
					Data     string `json:"data"`
				} `json:"inlineData"`
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
// TestEmbedMedia_NativeInlineData pins SPEC 8.1.7: EmbedMedia sends each
// item as an inline-data part (base64 + MIME) to batchEmbedContents, with
// the role→taskType mapping, and returns one vector per item in order.
func TestEmbedMedia_NativeInlineData(t *testing.T) {
	var gotPath, gotTask string
	var gotMimes, gotData []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		req := decodeNativeEmbed(t, r)
		for _, rq := range req.Requests {
			gotTask = rq.TaskType
			for _, p := range rq.Content.Parts {
				if p.InlineData != nil {
					gotMimes = append(gotMimes, p.InlineData.MimeType)
					gotData = append(gotData, p.InlineData.Data)
				}
			}
		}
		embs := make([]map[string]any, len(req.Requests))
		for i := range req.Requests {
			embs[i] = map[string]any{"values": []float64{float64(i), 0.5}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": embs})
	}))
	defer srv.Close()

	c := newClient(srv.URL)
	items := []gemini.MediaInput{
		{MimeType: "image/png", Data: []byte("PNGBYTES")},
		{MimeType: "audio/mp3", Data: []byte("MP3BYTES")},
	}
	vecs, err := c.EmbedMedia(context.Background(), "gemini-embedding-2", model.EmbedDocument, items)
	if err != nil {
		t.Fatalf("EmbedMedia: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("got %d vectors, want 2", len(vecs))
	}
	if want := "/models/gemini-embedding-2:batchEmbedContents"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotTask != "RETRIEVAL_DOCUMENT" {
		t.Errorf("taskType = %q, want RETRIEVAL_DOCUMENT", gotTask)
	}
	if len(gotMimes) != 2 || gotMimes[0] != "image/png" || gotMimes[1] != "audio/mp3" {
		t.Errorf("mimes = %v, want [image/png audio/mp3]", gotMimes)
	}
	if d, _ := base64.StdEncoding.DecodeString(gotData[0]); string(d) != "PNGBYTES" {
		t.Errorf("first media payload = %q, want base64(PNGBYTES)", gotData[0])
	}
}

// TestEmbedMedia_EmptyItemErrors covers a media item with no bytes.
func TestEmbedMedia_EmptyItemErrors(t *testing.T) {
	c := gemini.NewClient("http://127.0.0.1:0", "k")
	_, err := c.EmbedMedia(context.Background(), "gemini-embedding-2", model.EmbedDocument, []gemini.MediaInput{{MimeType: "image/png"}})
	if err == nil {
		t.Fatal("empty media item must error before any HTTP call")
	}
}

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

// nativeGenContentReq is the native generateContent request shape (subset).
type nativeGenContentReq struct {
	Contents []struct {
		Parts []struct {
			Text       string `json:"text"`
			InlineData *struct {
				MimeType string `json:"mimeType"`
				Data     string `json:"data"`
			} `json:"inlineData"`
		} `json:"parts"`
	} `json:"contents"`
	GenerationConfig struct {
		ResponseModalities []string `json:"responseModalities"`
		SpeechConfig       struct {
			VoiceConfig struct {
				PrebuiltVoiceConfig struct {
					VoiceName string `json:"voiceName"`
				} `json:"prebuiltVoiceConfig"`
			} `json:"voiceConfig"`
		} `json:"speechConfig"`
	} `json:"generationConfig"`
}

// decodeGenContent decodes a native generateContent request and validates
// it has at least one content part, so a shape change surfaces as a clear
// test failure (HTTP 400 + t.Errorf) instead of an out-of-range panic in
// the handler goroutine. Returns ok=false when the caller must not index.
func decodeGenContent(t *testing.T, w http.ResponseWriter, r *http.Request) (nativeGenContentReq, bool) {
	t.Helper()
	var req nativeGenContentReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Errorf("decode generateContent request: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return req, false
	}
	if len(req.Contents) == 0 || len(req.Contents[0].Parts) == 0 {
		t.Errorf("generateContent request has no content parts")
		http.Error(w, "bad request", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

// TestTranscribe_NativeGenerateContent verifies STT uses the native
// generateContent surface: correct path + x-goog-api-key auth, audio sent
// as an inline-data part with the extension-derived MIME type, and the
// candidate text returned as the transcript (SPEC 8.2).
func TestTranscribe_NativeGenerateContent(t *testing.T) {
	for _, tc := range []struct{ file, wantMime string }{
		{"a.wav", "audio/wav"},
		{"a.mp3", "audio/mp3"},
		{"a.flac", "audio/flac"},
		{"noext", "audio/wav"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			var gotPath, gotKey, gotMime, gotData, gotPrompt string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotKey = r.URL.Path, r.Header.Get("x-goog-api-key")
				req, ok := decodeGenContent(t, w, r)
				if !ok {
					return
				}
				for _, p := range req.Contents[0].Parts {
					if p.Text != "" {
						gotPrompt = p.Text
					}
					if p.InlineData != nil {
						gotMime, gotData = p.InlineData.MimeType, p.InlineData.Data
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"candidates": []map[string]any{{"content": map[string]any{
						"parts": []map[string]any{{"text": "hello transcript"}},
					}}},
				})
			}))
			defer srv.Close()

			c := newClient(srv.URL)
			txt, err := c.Transcribe(context.Background(), tc.file, []byte("pcm"))
			if err != nil || txt != "hello transcript" {
				t.Fatalf("Transcribe = %q, %v", txt, err)
			}
			if want := "/models/gemini-2.5-flash:generateContent"; gotPath != want {
				t.Errorf("path = %q, want %q", gotPath, want)
			}
			if gotKey != "test-key" {
				t.Errorf("api key header = %q, want test-key", gotKey)
			}
			if gotMime != tc.wantMime {
				t.Errorf("mime = %q, want %q", gotMime, tc.wantMime)
			}
			dec, derr := base64.StdEncoding.DecodeString(gotData)
			if derr != nil {
				t.Errorf("inline audio not valid base64 (%q): %v", gotData, derr)
			} else if string(dec) != "pcm" {
				t.Errorf("inline audio = %q, want base64(pcm)", gotData)
			}
			if !strings.Contains(strings.ToLower(gotPrompt), "transcript") {
				t.Errorf("prompt = %q, want a transcription instruction", gotPrompt)
			}
		})
	}
}

// TestTranscribe_LanguageHint verifies the optional STT language hint
// (SPEC 8.2 stt_language) is woven into the transcription prompt.
func TestTranscribe_LanguageHint(t *testing.T) {
	var gotPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeGenContent(t, w, r)
		if !ok {
			return
		}
		gotPrompt = req.Contents[0].Parts[0].Text
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{{"content": map[string]any{
				"parts": []map[string]any{{"text": "hola"}},
			}}},
		})
	}))
	defer srv.Close()

	c := newClient(srv.URL)
	c.DefaultSTTLanguage = "Spanish"
	if _, err := c.Transcribe(context.Background(), "a.wav", []byte("pcm")); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if !strings.Contains(gotPrompt, "Spanish") {
		t.Errorf("prompt = %q, want it to mention the configured language", gotPrompt)
	}
}

// TestSynthesize_NativeAudioWavWrapped verifies TTS uses generateContent
// with responseModalities:[AUDIO] + the configured voice, and that the
// returned raw PCM is WAV-wrapped (SPEC 8.3).
func TestSynthesize_NativeAudioWavWrapped(t *testing.T) {
	pcm := []byte{0x01, 0x02, 0x03, 0x04}
	var gotPath string
	var gotMods []string
	var gotVoice string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		req, ok := decodeGenContent(t, w, r)
		if !ok {
			return
		}
		gotMods = req.GenerationConfig.ResponseModalities
		gotVoice = req.GenerationConfig.SpeechConfig.VoiceConfig.PrebuiltVoiceConfig.VoiceName
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{{"content": map[string]any{
				"parts": []map[string]any{{"inlineData": map[string]any{
					"mimeType": "audio/L16;rate=24000",
					"data":     base64.StdEncoding.EncodeToString(pcm),
				}}},
			}}},
		})
	}))
	defer srv.Close()

	c := newClient(srv.URL)
	c.DefaultTTSVoice = "Puck"
	audio, err := c.Synthesize(context.Background(), "say this")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if want := "/models/gemini-2.5-flash-preview-tts:generateContent"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if len(gotMods) != 1 || gotMods[0] != "AUDIO" {
		t.Errorf("responseModalities = %v, want [AUDIO]", gotMods)
	}
	if gotVoice != "Puck" {
		t.Errorf("voice = %q, want Puck", gotVoice)
	}
	// The output must be a 44-byte-header WAV wrapping the PCM payload.
	if len(audio) != 44+len(pcm) {
		t.Fatalf("wav length = %d, want %d", len(audio), 44+len(pcm))
	}
	if string(audio[0:4]) != "RIFF" || string(audio[8:12]) != "WAVE" {
		t.Fatalf("missing RIFF/WAVE header: %x", audio[:12])
	}
	if rate := binary.LittleEndian.Uint32(audio[24:28]); rate != 24000 {
		t.Errorf("sample rate = %d, want 24000", rate)
	}
	if !bytes.Equal(audio[44:], pcm) {
		t.Errorf("PCM payload not preserved: got %x want %x", audio[44:], pcm)
	}
}

// TestSynthesize_NoAudioPartErrors covers a candidate with no inline audio.
func TestSynthesize_NoAudioPartErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{{"content": map[string]any{
				"parts": []map[string]any{{"text": "no audio here"}},
			}}},
		})
	}))
	defer srv.Close()
	if _, err := newClient(srv.URL).Synthesize(context.Background(), "hi"); err == nil {
		t.Fatal("want error when response carries no audio part")
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

package tests

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/openai"
)

func newClient(url string) *openai.Client {
	c := openai.NewClient(url, "test-key")
	c.InitialBackoff = time.Millisecond
	c.MaxBackoff = time.Millisecond
	return c
}

// embedResp echoes one item per input, embedding[0] = first byte of the
// input string, returned in REVERSED order to exercise index reordering.
func embedResp(inputs []string) map[string]any {
	n := len(inputs)
	data := make([]map[string]any, n)
	for i := 0; i < n; i++ {
		p := n - 1 - i
		data[i] = map[string]any{"index": p, "embedding": []float64{float64(inputs[p][0]), 0.5}}
	}
	return map[string]any{"data": data}
}

func TestEmbed_BatchesPreservesOrderAndRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The adapter appends "/embeddings" to the configured BaseURL;
		// any version segment (e.g. /v1) is part of the operator's
		// BaseURL, not added here. This test's base has none, so the
		// path is exactly "/embeddings" (versioned base covered by
		// TestEndpointPathJoinPreservesVersion).
		if r.URL.Path != "/embeddings" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth = %q", got)
		}
		n := atomic.AddInt32(&calls, 1)
		if n == 1 { // first batch: one transient 429 then success
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(embedResp(req.Input))
	}))
	defer srv.Close()

	c := newClient(srv.URL)
	c.BatchSize = 2 // forces 2 batches for 3 inputs (a,b) then (c)
	in := []string{"a", "b", "c"}
	vecs, err := c.Embed(context.Background(), "text-embedding-3-small", model.EmbedDocument, in)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("got %d vectors, want 3", len(vecs))
	}
	// Each output vector must correspond to its input across batch
	// boundaries (embedding[0] == first byte of the input string).
	for i, s := range in {
		if vecs[i][0] != float32(s[0]) {
			t.Fatalf("order not preserved at %d: got %v, want first-byte of %q", i, vecs[i], s)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 3 { // 1 retry (429) + 2 batches
		t.Fatalf("calls = %d, want 3", n)
	}
}

// TestEndpointPathJoinPreservesVersion verifies the adapter appends the
// endpoint to a BaseURL that already carries a version segment, so an
// operator-configured ".../v1" yields ".../v1/embeddings".
func TestEndpointPathJoinPreservesVersion(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float64{1}}},
		})
	}))
	defer srv.Close()

	c := openai.NewClient(srv.URL+"/v1", "test-key")
	if _, err := c.Embed(context.Background(), "m", model.EmbedDocument, []string{"x"}); err != nil {
		t.Fatalf("embed: %v", err)
	}
	if gotPath != "/v1/embeddings" {
		t.Fatalf("path = %q, want /v1/embeddings", gotPath)
	}
}

func TestEmbed_MissingKeyIsNonRetryableAuth(t *testing.T) {
	c := openai.NewClient("http://127.0.0.1:0", "")
	_, err := c.Embed(context.Background(), "m", model.EmbedQuery, []string{"x"})
	var pe *model.ProviderError
	if !asProviderErr(err, &pe) || pe.Code != "OPENAI_AUTH" || pe.Retryable {
		t.Fatalf("want non-retryable OPENAI_AUTH, got %v", err)
	}
}

func TestEmbed_EmptyInputsNoCall(t *testing.T) {
	c := openai.NewClient("http://127.0.0.1:0", "k")
	v, err := c.Embed(context.Background(), "m", model.EmbedDocument, nil)
	if err != nil || len(v) != 0 {
		t.Fatalf("want ([],nil), got %v %v", v, err)
	}
}

// TestEmbed_SymmetricRoleByteIdentical pins SPEC 8.1.5: OpenAI is a
// symmetric embedder, so the raw request bytes MUST be identical for
// EmbedDocument and EmbedQuery (role accepted and ignored).
func TestEmbed_SymmetricRoleByteIdentical(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float64{1}}},
		})
	}))
	defer srv.Close()

	c := newClient(srv.URL)
	for _, role := range []model.EmbedRole{model.EmbedDocument, model.EmbedQuery} {
		if _, err := c.Embed(context.Background(), "m", role, []string{"hi"}); err != nil {
			t.Fatalf("embed (%s): %v", role, err)
		}
	}
	if len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatalf("symmetric provider must send byte-identical request:\n%q\n%q", bodies[0], bodies[1])
	}
}

func TestGenerate_HappyPathAndStructuredContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": []map[string]any{
					{"type": "text", "text": "hello "},
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
	if out != "hello world" {
		t.Fatalf("content = %q, want %q", out, "hello world")
	}
}

// TestGenerate_SendsBoundedMaxTokens locks issue #500: the chat request
// must always carry a finite max_tokens so a misbehaving/self-hosted model
// cannot run away past the generation timeout and fail the whole file.
// NewClient's default (4096, matching the anthropic sibling so answer
// synthesis and annotate JSON are not truncated) applies when the caller sets
// nothing; an explicit GenerationMaxTokens overrides it.
func TestGenerate_SendsBoundedMaxTokens(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override int
		want     float64
	}{
		{name: "default", override: 0, want: 4096},
		{name: "override", override: 42, want: 42},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []map[string]any{{"message": map[string]any{"content": "ok"}}},
				})
			}))
			defer srv.Close()

			c := newClient(srv.URL)
			if tc.override != 0 {
				c.GenerationMaxTokens = tc.override
			}
			if _, err := c.Generate(context.Background(), "hi"); err != nil {
				t.Fatalf("generate: %v", err)
			}
			mt, ok := gotBody["max_tokens"]
			if !ok {
				t.Fatalf("request omitted max_tokens; body = %v", gotBody)
			}
			if got, _ := mt.(float64); got != tc.want {
				t.Fatalf("max_tokens = %v, want %v", mt, tc.want)
			}
		})
	}
}

// TestGenerateWithMaxTokens_PerCallCap locks the per-call seam: a caller with a
// known-short output (e.g. one translated transcript line) can request a tight
// cap for that call WITHOUT lowering the generous default that Generate
// (ask/annotate) uses. A positive maxTokens is sent verbatim; a <= 0 maxTokens
// falls back to the client default, so it behaves like Generate.
func TestGenerateWithMaxTokens_PerCallCap(t *testing.T) {
	for _, tc := range []struct {
		name      string
		maxTokens int
		want      float64
	}{
		{name: "tight cap", maxTokens: 512, want: 512},
		{name: "zero falls back to default", maxTokens: 0, want: 4096},
		{name: "negative falls back to default", maxTokens: -1, want: 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []map[string]any{{"message": map[string]any{"content": "ok"}}},
				})
			}))
			defer srv.Close()

			var bg model.BoundedGenerator = newClient(srv.URL)
			if _, err := bg.GenerateWithMaxTokens(context.Background(), "hi", tc.maxTokens); err != nil {
				t.Fatalf("generate: %v", err)
			}
			mt, ok := gotBody["max_tokens"]
			if !ok {
				t.Fatalf("request omitted max_tokens; body = %v", gotBody)
			}
			if got, _ := mt.(float64); got != tc.want {
				t.Fatalf("max_tokens = %v, want %v", mt, tc.want)
			}
		})
	}
}

// TestGenerate_UsesGenerationTimeoutNotDefault locks the fix for the
// PR #191 review: NewClient always sets HTTPClient (30s), so the
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

	c := openai.NewClient(srv.URL, "test-key") // default HTTPClient: 30s
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
	if !asProviderErr(err, &pe) || pe.Code != "OPENAI_FAILED" {
		t.Fatalf("want OPENAI_FAILED, got %v", err)
	}
}

func TestHTTPErrorMapping(t *testing.T) {
	cases := []struct {
		status    int
		wantCode  string
		retryable bool
	}{
		{http.StatusUnauthorized, "OPENAI_AUTH", false},
		{http.StatusTooManyRequests, "OPENAI_RATE_LIMIT", true},
		{http.StatusBadGateway, "OPENAI_FAILED", true},
		{http.StatusBadRequest, "OPENAI_FAILED", false},
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
	nokey := openai.NewClient("http://127.0.0.1:0", "")
	if _, err := nokey.Transcribe(context.Background(), "a.wav", []byte("x")); err == nil {
		t.Error("Transcribe missing key must error")
	}
	if _, err := nokey.Synthesize(context.Background(), "x"); err == nil {
		t.Error("Synthesize missing key must error")
	}
	keyed := openai.NewClient("http://127.0.0.1:0", "k")
	if _, err := keyed.Transcribe(context.Background(), "a.wav", nil); err == nil {
		t.Error("Transcribe empty data must error (no HTTP call)")
	}
	if _, err := keyed.Synthesize(context.Background(), "  "); err == nil {
		t.Error("Synthesize empty text must error (no HTTP call)")
	}
}

func asProviderErr(err error, target **model.ProviderError) bool {
	if err == nil {
		return false
	}
	pe, ok := err.(*model.ProviderError)
	if ok {
		*target = pe
	}
	return ok && strings.HasPrefix(pe.Code, "OPENAI_")
}

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

	"github.com/dirstral/dir2mcp/internal/gemini"
	"github.com/dirstral/dir2mcp/internal/model"
)

func newClient(url string) *gemini.Client {
	c := gemini.NewClient(url, "test-key")
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
		// any version segment is part of the operator's BaseURL, not
		// added here. This test's base has none, so the path is exactly
		// "/embeddings" (versioned base covered by
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
	vecs, err := c.Embed(context.Background(), "text-embedding-004", model.EmbedDocument, in)
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
// operator-configured ".../v1beta/openai" yields
// ".../v1beta/openai/embeddings".
func TestEndpointPathJoinPreservesVersion(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float64{1}}},
		})
	}))
	defer srv.Close()

	c := gemini.NewClient(srv.URL+"/v1beta/openai", "test-key")
	if _, err := c.Embed(context.Background(), "m", model.EmbedDocument, []string{"x"}); err != nil {
		t.Fatalf("embed: %v", err)
	}
	if gotPath != "/v1beta/openai/embeddings" {
		t.Fatalf("path = %q, want /v1beta/openai/embeddings", gotPath)
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

// TestEmbed_SymmetricRoleByteIdentical pins SPEC 8.1.5: Gemini is a
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

// TestGenerate_UsesGenerationTimeoutNotDefault locks the same invariant
// as internal/openai: NewClient always sets HTTPClient (30s), so the
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

func asProviderErr(err error, target **model.ProviderError) bool {
	if err == nil {
		return false
	}
	pe, ok := err.(*model.ProviderError)
	if ok {
		*target = pe
	}
	return ok && strings.HasPrefix(pe.Code, "GEMINI_")
}

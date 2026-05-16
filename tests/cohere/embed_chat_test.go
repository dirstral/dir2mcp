package tests

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cohere"
	"github.com/dirstral/dir2mcp/internal/model"
)

// embedResp echoes one vector per input where vec[0] = first byte of the
// corresponding input string, preserving order (Cohere returns float
// vectors positionally under embeddings.float).
func embedResp(inputs []string) map[string]any {
	floats := make([][]float64, len(inputs))
	for i, s := range inputs {
		floats[i] = []float64{float64(s[0]), 0.5}
	}
	return map[string]any{"embeddings": map[string]any{"float": floats}}
}

type embedBody struct {
	Model          string   `json:"model"`
	Texts          []string `json:"texts"`
	InputType      string   `json:"input_type"`
	EmbeddingTypes []string `json:"embedding_types"`
}

// TestEmbed_AsymmetricInputTypeByRole pins SPEC 8.1.5: Cohere is an
// ASYMMETRIC embedder, so the wire request MUST carry
// input_type=search_document for EmbedDocument and search_query for
// EmbedQuery (decoded-body assertion).
func TestEmbed_AsymmetricInputTypeByRole(t *testing.T) {
	var raws [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/embed" {
			t.Errorf("path = %s, want /v2/embed", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth = %q", got)
		}
		raw, _ := io.ReadAll(r.Body)
		raws = append(raws, raw)
		var req struct {
			Texts []string `json:"texts"`
		}
		_ = json.Unmarshal(raw, &req)
		_ = json.NewEncoder(w).Encode(embedResp(req.Texts))
	}))
	defer srv.Close()

	c := newClient(srv.URL)
	cases := []struct {
		role model.EmbedRole
		want string
	}{
		{model.EmbedDocument, "search_document"},
		{model.EmbedQuery, "search_query"},
	}
	for _, tc := range cases {
		if _, err := c.Embed(context.Background(), "embed-v4.0", tc.role, []string{"hi"}); err != nil {
			t.Fatalf("embed (%s): %v", tc.role, err)
		}
	}
	if len(raws) != 2 {
		t.Fatalf("want 2 requests, got %d", len(raws))
	}
	for i, tc := range cases {
		var b embedBody
		if err := json.Unmarshal(raws[i], &b); err != nil {
			t.Fatalf("decode body %d: %v", i, err)
		}
		if b.InputType != tc.want {
			t.Fatalf("role %s: input_type = %q, want %q (raw=%s)", tc.role, b.InputType, tc.want, raws[i])
		}
		if b.Model != "embed-v4.0" || len(b.Texts) != 1 || b.Texts[0] != "hi" {
			t.Fatalf("unexpected embed body: %+v", b)
		}
		if len(b.EmbeddingTypes) != 1 || b.EmbeddingTypes[0] != "float" {
			t.Fatalf("embedding_types = %v, want [float]", b.EmbeddingTypes)
		}
	}
	// Asymmetric: the two raw bodies must differ (unlike symmetric OpenAI).
	if string(raws[0]) == string(raws[1]) {
		t.Fatalf("asymmetric provider must send role-dependent bodies; both = %s", raws[0])
	}
}

func TestEmbed_BatchesPreserveOrderAndRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 { // first batch: one transient 429 then success
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		var req struct {
			Texts []string `json:"texts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(embedResp(req.Texts))
	}))
	defer srv.Close()

	c := newClient(srv.URL)
	c.BatchSize = 2 // 3 inputs -> batches (a,b) then (c)
	in := []string{"a", "b", "c"}
	vecs, err := c.Embed(context.Background(), "embed-v4.0", model.EmbedDocument, in)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("got %d vectors, want 3", len(vecs))
	}
	for i, s := range in {
		if vecs[i][0] != float32(s[0]) {
			t.Fatalf("order not preserved at %d: got %v, want first byte of %q", i, vecs[i], s)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 3 { // 1 retry (429) + 2 batches
		t.Fatalf("calls = %d, want 3", n)
	}
}

func TestEmbed_EmptyInputsNoCall(t *testing.T) {
	c := cohere.NewClient("http://127.0.0.1:0", "k")
	v, err := c.Embed(context.Background(), "m", model.EmbedDocument, nil)
	if err != nil || len(v) != 0 {
		t.Fatalf("want ([],nil), got %v %v", v, err)
	}
}

func TestEmbed_MissingKeyIsNonRetryableAuth(t *testing.T) {
	c := cohere.NewClient("http://127.0.0.1:0", "")
	_, err := c.Embed(context.Background(), "m", model.EmbedQuery, []string{"x"})
	assertProviderError(t, err, "COHERE_AUTH", false)
}

func TestEmbed_DefaultModelWhenEmpty(t *testing.T) {
	var b embedBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &b)
		_ = json.NewEncoder(w).Encode(embedResp([]string{"x"}))
	}))
	defer srv.Close()
	if _, err := newClient(srv.URL).Embed(context.Background(), "", model.EmbedDocument, []string{"x"}); err != nil {
		t.Fatalf("embed: %v", err)
	}
	if b.Model != cohere.DefaultEmbedModel {
		t.Fatalf("model = %q, want default %q", b.Model, cohere.DefaultEmbedModel)
	}
}

func TestEmbed_SizeMismatchRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"embeddings":{"float":[[1,2]]}}`))
	}))
	defer srv.Close()
	_, err := newClient(srv.URL).Embed(context.Background(), "m", model.EmbedDocument, []string{"a", "b"})
	assertProviderError(t, err, "COHERE_FAILED", false)
}

func TestEmbed_StatusMapping(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		wantCode  string
		retryable bool
	}{
		{"unauthorized", http.StatusUnauthorized, "COHERE_AUTH", false},
		{"rate limit", http.StatusTooManyRequests, "COHERE_RATE_LIMIT", true},
		{"server error", http.StatusBadGateway, "COHERE_FAILED", true},
		{"bad request", http.StatusBadRequest, "COHERE_FAILED", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
			}))
			defer srv.Close()
			c := newClient(srv.URL)
			c.MaxRetries = 0
			_, err := c.Embed(context.Background(), "m", model.EmbedDocument, []string{"x"})
			assertProviderError(t, err, tc.wantCode, tc.retryable)
		})
	}
}

type chatBody struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

func TestGenerate_HappyPathConcatsTextParts(t *testing.T) {
	var b chatBody
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]any{"content": []map[string]any{
				{"type": "text", "text": "hello "},
				{"type": "text", "text": "world"},
			}},
		})
	}))
	defer srv.Close()

	out, err := newClient(srv.URL).Generate(context.Background(), "hi there")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if out != "hello world" {
		t.Fatalf("content = %q, want %q", out, "hello world")
	}
	if gotPath != "/v2/chat" {
		t.Fatalf("path = %q, want /v2/chat", gotPath)
	}
	if b.Model != cohere.DefaultChatModel {
		t.Fatalf("model = %q, want %q", b.Model, cohere.DefaultChatModel)
	}
	if len(b.Messages) != 1 || b.Messages[0].Role != "user" || b.Messages[0].Content != "hi there" {
		t.Fatalf("unexpected chat body: %+v", b)
	}
}

func TestGenerate_EmptyContentIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"content":[]}}`))
	}))
	defer srv.Close()
	_, err := newClient(srv.URL).Generate(context.Background(), "hi")
	assertProviderError(t, err, "COHERE_FAILED", false)
}

func TestGenerate_MissingKeyIsNonRetryableAuth(t *testing.T) {
	c := cohere.NewClient("http://127.0.0.1:0", "")
	_, err := c.Generate(context.Background(), "hi")
	assertProviderError(t, err, "COHERE_AUTH", false)
}

func TestGenerate_StatusMapping(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		wantCode  string
		retryable bool
	}{
		{"unauthorized", http.StatusUnauthorized, "COHERE_AUTH", false},
		{"rate limit", http.StatusTooManyRequests, "COHERE_RATE_LIMIT", true},
		{"server error", http.StatusInternalServerError, "COHERE_FAILED", true},
		{"bad request", http.StatusBadRequest, "COHERE_FAILED", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
			}))
			defer srv.Close()
			c := newClient(srv.URL)
			c.MaxRetries = 0
			_, err := c.Generate(context.Background(), "hi")
			assertProviderError(t, err, tc.wantCode, tc.retryable)
		})
	}
}

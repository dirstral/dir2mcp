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

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/omniembed"
)

// newClient builds an omniembed client with near-zero backoff so retry
// tests stay fast.
func newClient(url, apiKey string) *omniembed.Client {
	c := omniembed.NewClient(url, apiKey)
	c.InitialBackoff = time.Millisecond
	c.MaxBackoff = time.Millisecond
	return c
}

// embedReq is the OpenAI-compatible request shape the server receives.
type embedReq struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// decodeReq parses the JSON embeddings request body.
func decodeReq(t *testing.T, r *http.Request) embedReq {
	t.Helper()
	var req embedReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return req
}

// embedResponseFor builds an OpenAI-compatible embeddings response where the
// i-th input maps to a dim-length vector. Indices are returned out of order
// to exercise the reorder-by-index logic.
func embedResponseFor(n, dim int) map[string]any {
	data := make([]map[string]any, 0, n)
	for i := n - 1; i >= 0; i-- { // reverse order on purpose
		vec := make([]float64, dim)
		for d := range vec {
			vec[d] = float64(i)*0.1 + float64(d)
		}
		data = append(data, map[string]any{"index": i, "embedding": vec})
	}
	return map[string]any{"data": data}
}

// TestEmbed_TextOpenAIShape verifies text embedding hits POST
// /v1/embeddings with the OpenAI {model,input:[...]} shape and returns
// dim-correct vectors in input order.
func TestEmbed_TextOpenAIShape(t *testing.T) {
	const dim = 8
	var gotPath, gotModel string
	var gotInput []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		req := decodeReq(t, r)
		gotModel = req.Model
		gotInput = req.Input
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(embedResponseFor(len(req.Input), dim))
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	c.DefaultEmbedModel = "omniembed-v0.1"

	vecs, err := c.Embed(context.Background(), "", model.EmbedDocument, []string{"alpha", "beta", "gamma"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotPath != "/v1/embeddings" {
		t.Errorf("path = %q, want /v1/embeddings", gotPath)
	}
	if gotModel != "omniembed-v0.1" {
		t.Errorf("model = %q, want omniembed-v0.1 (default applied)", gotModel)
	}
	if len(gotInput) != 3 || gotInput[0] != "alpha" || gotInput[2] != "gamma" {
		t.Errorf("input = %v, want [alpha beta gamma]", gotInput)
	}
	if len(vecs) != 3 {
		t.Fatalf("got %d vectors, want 3", len(vecs))
	}
	for i, v := range vecs {
		if len(v) != dim {
			t.Errorf("vector %d dim = %d, want %d", i, len(v), dim)
		}
	}
	// Reorder-by-index correctness: vec[i][0] == i*0.1.
	if vecs[1][0] < 0.09 || vecs[1][0] > 0.11 {
		t.Errorf("vector 1 not reordered to input order: %v", vecs[1])
	}
}

// TestEmbedMedia_DataURIInput verifies media items are sent as base64
// data URIs in the same input array, and that vectors are dim-correct.
func TestEmbedMedia_DataURIInput(t *testing.T) {
	const dim = 4
	var gotInput []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeReq(t, r)
		gotInput = req.Input
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(embedResponseFor(len(req.Input), dim))
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	items := []model.MediaInput{
		{MimeType: "image/png", Data: []byte("PNGBYTES")},
		{MimeType: "audio/mp3", Data: []byte("MP3BYTES")},
	}
	vecs, err := c.EmbedMedia(context.Background(), "omniembed", model.EmbedDocument, items)
	if err != nil {
		t.Fatalf("EmbedMedia: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("got %d vectors, want 2", len(vecs))
	}
	for i, v := range vecs {
		if len(v) != dim {
			t.Errorf("vector %d dim = %d, want %d", i, len(v), dim)
		}
	}
	if len(gotInput) != 2 {
		t.Fatalf("got %d inputs, want 2", len(gotInput))
	}
	if !strings.HasPrefix(gotInput[0], "data:image/png;base64,") {
		t.Errorf("input[0] = %q, want image/png data URI", gotInput[0])
	}
	if !strings.HasPrefix(gotInput[1], "data:audio/mp3;base64,") {
		t.Errorf("input[1] = %q, want audio/mp3 data URI", gotInput[1])
	}
	// Raw bytes must not appear verbatim (defense against accidental
	// plaintext leakage); they are base64-encoded.
	if strings.Contains(gotInput[0], "PNGBYTES") {
		t.Errorf("input[0] leaked raw bytes: %q", gotInput[0])
	}
}

// TestEmbedMedia_EmptyItemRejected verifies an empty media payload is a
// non-retryable error and never reaches the network.
func TestEmbedMedia_EmptyItemRejected(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	_, err := c.EmbedMedia(context.Background(), "omniembed", model.EmbedDocument,
		[]model.MediaInput{{MimeType: "image/png", Data: nil}})
	if err == nil {
		t.Fatal("want error for empty media item, got nil")
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("server hit %d times, want 0 (validated before request)", got)
	}
}

// TestEmbed_EmptyInputsNoCall verifies zero inputs short-circuit with no
// HTTP call (mirrors Embedder contract for empty batches).
func TestEmbed_EmptyInputsNoCall(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	vecs, err := c.Embed(context.Background(), "m", model.EmbedQuery, nil)
	if err != nil {
		t.Fatalf("Embed(empty): %v", err)
	}
	if len(vecs) != 0 {
		t.Errorf("got %d vectors, want 0", len(vecs))
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("server hit %d times, want 0", got)
	}
}

// TestEmbed_BearerOnlyWhenCredentialed verifies the Authorization header is
// sent only when an api_key is configured (credential-optional self-hosted).
func TestEmbed_BearerOnlyWhenCredentialed(t *testing.T) {
	check := func(t *testing.T, apiKey, wantAuth string) {
		t.Helper()
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			req := decodeReq(t, r)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(embedResponseFor(len(req.Input), 2))
		}))
		defer srv.Close()
		c := newClient(srv.URL, apiKey)
		if _, err := c.Embed(context.Background(), "m", model.EmbedDocument, []string{"x"}); err != nil {
			t.Fatalf("Embed: %v", err)
		}
		if gotAuth != wantAuth {
			t.Errorf("Authorization = %q, want %q", gotAuth, wantAuth)
		}
	}
	t.Run("credential-less", func(t *testing.T) { check(t, "", "") })
	t.Run("credentialed", func(t *testing.T) { check(t, "sekret", "Bearer sekret") })
}

// TestEmbed_RetryOn5xx verifies a transient 5xx is retried and then
// succeeds, while a 401 is a non-retryable OMNIEMBED_AUTH error.
func TestEmbed_RetryOn5xx(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			http.Error(w, "overloaded", http.StatusServiceUnavailable)
			return
		}
		req := decodeReq(t, r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(embedResponseFor(len(req.Input), 3))
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	vecs, err := c.Embed(context.Background(), "m", model.EmbedDocument, []string{"x"})
	if err != nil {
		t.Fatalf("Embed after retry: %v", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != 3 {
		t.Errorf("unexpected vectors after retry: %v", vecs)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("attempts = %d, want 2 (one retry)", got)
	}
}

// TestEmbed_AuthErrorNotRetried verifies a 401 maps to a non-retryable
// OMNIEMBED_AUTH ProviderError and is attempted exactly once.
func TestEmbed_AuthErrorNotRetried(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "k")
	_, err := c.Embed(context.Background(), "m", model.EmbedDocument, []string{"x"})
	if err == nil {
		t.Fatal("want auth error, got nil")
	}
	var pErr *model.ProviderError
	if !errors.As(err, &pErr) || pErr.Code != "OMNIEMBED_AUTH" {
		t.Errorf("err = %v, want OMNIEMBED_AUTH ProviderError", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (auth not retried)", got)
	}
}

// TestEmbed_SizeMismatch verifies a response with the wrong vector count is
// a non-retryable failure.
func TestEmbed_SizeMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Return 1 vector for 2 inputs.
		_ = json.NewEncoder(w).Encode(embedResponseFor(1, 4))
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	_, err := c.Embed(context.Background(), "m", model.EmbedDocument, []string{"a", "b"})
	if err == nil {
		t.Fatal("want size-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "size mismatch") {
		t.Errorf("err = %v, want size mismatch", err)
	}
}

package tests

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/mistral"
	"github.com/dirstral/dir2mcp/internal/model"
)

func TestGenerate_SuccessStringContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("unexpected auth header: %q", got)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// verify default model is used
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req["model"] != mistral.DefaultChatModel {
			t.Errorf("unexpected model: %#v", req["model"])
			http.Error(w, "bad model", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "Hello from model"}},
			},
		})
	}))
	defer server.Close()

	client := mistral.NewClient(server.URL, "test-key")
	out, err := client.Generate(context.Background(), "Say hello")
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	if out != "Hello from model" {
		t.Fatalf("unexpected generated text: %q", out)
	}
}

func TestGenerate_SuccessArrayContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": []map[string]any{
							{"type": "text", "text": "first"},
							{"type": "text", "text": "second"},
						},
					},
				},
			},
		})
	}))
	defer server.Close()

	client := mistral.NewClient(server.URL, "test-key")
	out, err := client.Generate(context.Background(), "Two lines")
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	if out != "first\nsecond" {
		t.Fatalf("unexpected generated text: %q", out)
	}
}

func TestGenerate_RetriesOnRateLimit(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limited"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "ok"}}},
		})
	}))
	defer server.Close()

	client := mistral.NewClient(server.URL, "test-key")
	client.MaxRetries = 2
	client.InitialBackoff = 1 * time.Millisecond
	client.MaxBackoff = 1 * time.Millisecond

	out, err := client.Generate(context.Background(), "retry me")
	if err != nil {
		t.Fatalf("Generate should have succeeded after retry: %v", err)
	}
	if out != "ok" {
		t.Fatalf("unexpected generated text: %q", out)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("unexpected call count: got %d want 2", got)
	}
}

func TestGenerate_MapsAuthErrors(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		// always return 401 regardless of attempt count
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("invalid key"))
		_ = n // silence unused
	}))
	defer server.Close()

	client := mistral.NewClient(server.URL, "bad-key")
	// set MaxRetries high enough that a failure to mark auth errors as non-retryable
	// would trigger at least one retry invocation
	client.MaxRetries = 2

	_, err := client.Generate(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected auth error")
	}
	var providerErr *model.ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("expected ProviderError, got %T", err)
	}
	if providerErr.Code != "MISTRAL_AUTH" {
		t.Fatalf("unexpected code: %s", providerErr.Code)
	}
	if providerErr.Retryable {
		t.Fatal("auth errors should not be retryable")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("unexpected call count: got %d want 1", got)
	}
}

func TestGenerate_ValidatesPrompt(t *testing.T) {
	// use a local server so that even if validation is bypassed we won't hit
	// the real API. handler should never be invoked.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("network call should not happen during prompt validation")
		http.Error(w, "unexpected call", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := mistral.NewClient(server.URL, "test-key")
	_, err := client.Generate(context.Background(), "   ")
	if err == nil {
		t.Fatal("expected prompt validation error")
	}
	var providerErr *model.ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("expected ProviderError, got %T", err)
	}
	if providerErr.Code != "MISTRAL_FAILED" {
		t.Fatalf("unexpected code: %s", providerErr.Code)
	}
	if !strings.Contains(providerErr.Message, "prompt is required") {
		t.Fatalf("unexpected message: %q", providerErr.Message)
	}
}

func TestGenerate_UsesConfiguredGenerationTimeoutWithCustomHTTPClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(20 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "slow ok"}}},
		})
	}))
	defer server.Close()

	client := mistral.NewClient(server.URL, "test-key")
	// Custom client would previously bypass GenerationTimeout.
	client.HTTPClient = &http.Client{
		Timeout:   2 * time.Second,
		Transport: http.DefaultTransport,
	}
	client.GenerationTimeout = 5 * time.Millisecond

	_, err := client.Generate(context.Background(), "timeout please")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	var providerErr *model.ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("expected ProviderError, got %T", err)
	}
	if providerErr.Code != "MISTRAL_FAILED" || !providerErr.Retryable {
		t.Fatalf("unexpected provider error: %#v", providerErr)
	}
	if providerErr.Cause == nil || !strings.Contains(providerErr.Cause.Error(), "Client.Timeout") {
		t.Fatalf("expected timeout cause, got: %v", providerErr.Cause)
	}
}

func TestGenerate_UsesConfiguredChatModelInRequest(t *testing.T) {
	const expectedModel = "chat-override"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req["model"] != expectedModel {
			http.Error(w, "wrong model", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "ok"}}},
		})
	}))
	defer server.Close()

	client := mistral.NewClient(server.URL, "test-key")
	client.DefaultChatModel = expectedModel
	out, err := client.Generate(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	if out != "ok" {
		t.Fatalf("unexpected output: %q", out)
	}
}

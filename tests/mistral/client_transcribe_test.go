package tests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/mistral"
	"github.com/dirstral/dir2mcp/internal/model"
)

func TestTranscribe_FormatsSegments(t *testing.T) {
	var handlerErr error
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			mu.Lock()
			handlerErr = fmt.Errorf("unexpected path: %s", r.URL.Path)
			mu.Unlock()
			http.Error(w, handlerErr.Error(), http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			mu.Lock()
			handlerErr = fmt.Errorf("unexpected auth header: %q", got)
			mu.Unlock()
			http.Error(w, handlerErr.Error(), http.StatusUnauthorized)
			return
		}
		if got := r.Header.Get("x-api-key"); got != "" {
			mu.Lock()
			handlerErr = fmt.Errorf("unexpected x-api-key header present: %q", got)
			mu.Unlock()
			http.Error(w, handlerErr.Error(), http.StatusUnauthorized)
			return
		}
		// ensure default transcription model is included
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			mu.Lock()
			handlerErr = fmt.Errorf("ParseMultipartForm failed: %v", err)
			mu.Unlock()
			http.Error(w, handlerErr.Error(), http.StatusBadRequest)
			return
		}
		if m := r.FormValue("model"); m != mistral.DefaultTranscribeModel {
			mu.Lock()
			handlerErr = fmt.Errorf("unexpected model: %s", m)
			mu.Unlock()
			http.Error(w, handlerErr.Error(), http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"segments": []map[string]any{
				{"start": 0.0, "end": 1.5, "text": "hello"},
				{"start": 65.0, "end": 70.0, "text": "world"},
			},
			"text": "hello world",
		})
	}))
	defer server.Close()

	client := mistral.NewClient(server.URL, "test-key")
	out, err := client.Transcribe(context.Background(), "clip.mp3", []byte("audio"))
	if err != nil {
		t.Fatalf("Transcribe failed: %v", err)
	}
	mu.Lock()
	if handlerErr != nil {
		t.Fatalf("handler error: %v", handlerErr)
	}
	mu.Unlock()
	want := "[00:00] hello\n[01:05] world"
	if out != want {
		t.Fatalf("unexpected transcript output: got %q want %q", out, want)
	}
}

func TestTranscribe_CustomModel(t *testing.T) {
	var handlerErr error
	var mu sync.Mutex
	testModel := "custom-stt"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			mu.Lock()
			handlerErr = fmt.Errorf("ParseMultipartForm failed: %v", err)
			mu.Unlock()
			http.Error(w, handlerErr.Error(), http.StatusBadRequest)
			return
		}
		if m := r.FormValue("model"); m != testModel {
			mu.Lock()
			handlerErr = fmt.Errorf("expected model %q, got %q", testModel, m)
			mu.Unlock()
			http.Error(w, handlerErr.Error(), http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"text": "ok"})
	}))
	defer server.Close()

	client := mistral.NewClient(server.URL, "test-key")
	client.DefaultTranscribeModel = testModel
	out, err := client.Transcribe(context.Background(), "clip.mp3", []byte("audio"))
	if err != nil {
		t.Fatalf("Transcribe failed: %v", err)
	}
	mu.Lock()
	if handlerErr != nil {
		t.Fatalf("handler error: %v", handlerErr)
	}
	mu.Unlock()
	if out != "ok" {
		t.Fatalf("unexpected transcript: %q", out)
	}
}

func TestTranscribe_RetriesRateLimit(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limited"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"text": "ok"})
	}))
	defer server.Close()

	client := mistral.NewClient(server.URL, "test-key")
	client.MaxRetries = 2
	client.InitialBackoff = 1 * time.Millisecond
	client.MaxBackoff = 1 * time.Millisecond

	out, err := client.Transcribe(context.Background(), "retry.wav", []byte("audio"))
	if err != nil {
		t.Fatalf("Transcribe should succeed after retry: %v", err)
	}
	if out != "ok" {
		t.Fatalf("unexpected transcript: %q", out)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("unexpected call count: got %d want 2", got)
	}
}

func TestTranscribe_MapsAuthErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("invalid key"))
	}))
	defer server.Close()

	client := mistral.NewClient(server.URL, "bad-key")
	client.MaxRetries = 1

	_, err := client.Transcribe(context.Background(), "bad.mp3", []byte("audio"))
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
}

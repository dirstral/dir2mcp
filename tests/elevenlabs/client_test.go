package tests

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"dir2mcp/internal/elevenlabs"
	"dir2mcp/internal/model"
)

type elevenLabsRoundTripFunc func(*http.Request) (*http.Response, error)

func (f elevenLabsRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestElevenLabsSynthesize_ReturnsAudioBytes(t *testing.T) {
	var (
		gotPath   string
		gotMethod string
		gotAuth   string
		gotBody   string
	)

	rt := elevenLabsRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("xi-api-key")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewBufferString("fake-mp3")),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})

	client := elevenlabs.NewClient("test-api-key", "voice-default")
	client.HTTPClient = &http.Client{Transport: rt}

	audio, err := client.Synthesize(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("Synthesize failed: %v", err)
	}
	if string(audio) != "fake-mp3" {
		t.Fatalf("unexpected synthesized bytes: %q", string(audio))
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("unexpected method: %q", gotMethod)
	}
	if gotPath != "/v1/text-to-speech/voice-default" {
		t.Fatalf("unexpected request path: %q", gotPath)
	}
	if gotAuth != "test-api-key" {
		t.Fatalf("unexpected xi-api-key header: %q", gotAuth)
	}
	if !strings.Contains(gotBody, "hello world") {
		t.Fatalf("request body missing input text: %q", gotBody)
	}
}

func TestElevenLabsSynthesize_Maps401ToAuthError(t *testing.T) {
	rt := elevenLabsRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Body:       io.NopCloser(bytes.NewBufferString("unauthorized")),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})

	client := elevenlabs.NewClient("test-api-key", "voice-default")
	client.HTTPClient = &http.Client{Transport: rt}

	_, err := client.Synthesize(context.Background(), "hello world")
	if err == nil {
		t.Fatal("expected provider error")
	}
	var providerErr *model.ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("expected ProviderError, got %T", err)
	}
	if providerErr.Code != "ELEVENLABS_AUTH" {
		t.Fatalf("unexpected code: %s", providerErr.Code)
	}
	if providerErr.Retryable {
		t.Fatal("expected non-retryable auth error")
	}
}

func TestElevenLabsSynthesize_Maps429ToRateLimitError(t *testing.T) {
	rt := elevenLabsRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(bytes.NewBufferString("rate limited")),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})

	client := elevenlabs.NewClient("test-api-key", "voice-default")
	client.HTTPClient = &http.Client{Transport: rt}

	_, err := client.Synthesize(context.Background(), "hello world")
	if err == nil {
		t.Fatal("expected provider error")
	}
	var providerErr *model.ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("expected ProviderError, got %T", err)
	}
	if providerErr.Code != "ELEVENLABS_RATE_LIMIT" {
		t.Fatalf("unexpected code: %s", providerErr.Code)
	}
	if !providerErr.Retryable {
		t.Fatal("expected retryable rate-limit error")
	}
}

func TestElevenLabsSynthesize_Maps5xxToRetryableFailure(t *testing.T) {
	rt := elevenLabsRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Body:       io.NopCloser(bytes.NewBufferString("upstream unavailable")),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})

	client := elevenlabs.NewClient("test-api-key", "voice-default")
	client.HTTPClient = &http.Client{Transport: rt}

	_, err := client.Synthesize(context.Background(), "hello world")
	if err == nil {
		t.Fatal("expected provider error")
	}
	var providerErr *model.ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("expected ProviderError, got %T", err)
	}
	if providerErr.Code != "ELEVENLABS_FAILED" {
		t.Fatalf("unexpected code: %s", providerErr.Code)
	}
	if !providerErr.Retryable {
		t.Fatal("expected retryable failure for 5xx")
	}
}

func TestElevenLabsTranscribe_ReturnsTimestampedSegments(t *testing.T) {
	var (
		gotPath      string
		gotMethod    string
		gotAuth      string
		gotModelID   string
		gotFileBytes []byte
	)

	rt := elevenLabsRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("xi-api-key")

		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("parse content-type: %v", err)
		}
		if mediaType != "multipart/form-data" {
			t.Fatalf("unexpected media type: %q", mediaType)
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("read multipart part: %v", err)
			}
			body, _ := io.ReadAll(part)
			switch part.FormName() {
			case "model_id":
				gotModelID = string(body)
			case "file":
				gotFileBytes = body
			}
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewBufferString(`{"segments":[{"start":0,"text":"hello"},{"start":2.4,"text":"world"}]}`)),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})

	client := elevenlabs.NewClient("test-api-key", "voice-default")
	client.HTTPClient = &http.Client{Transport: rt}

	text, err := client.Transcribe(context.Background(), "audio/sample.mp3", []byte("fake-audio"))
	if err != nil {
		t.Fatalf("Transcribe failed: %v", err)
	}
	if text != "[00:00] hello\n[00:02] world" {
		t.Fatalf("unexpected transcript: %q", text)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("unexpected method: %q", gotMethod)
	}
	if gotPath != "/v1/speech-to-text" {
		t.Fatalf("unexpected request path: %q", gotPath)
	}
	if gotAuth != "test-api-key" {
		t.Fatalf("unexpected xi-api-key header: %q", gotAuth)
	}
	if gotModelID != "scribe_v1" {
		t.Fatalf("unexpected model_id: %q", gotModelID)
	}
	if string(gotFileBytes) != "fake-audio" {
		t.Fatalf("unexpected uploaded bytes: %q", string(gotFileBytes))
	}
}

func TestElevenLabsTranscribe_FallbacksToTextField(t *testing.T) {
	rt := elevenLabsRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewBufferString(`{"text":"plain transcript"}`)),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})

	client := elevenlabs.NewClient("test-api-key", "voice-default")
	client.HTTPClient = &http.Client{Transport: rt}

	text, err := client.Transcribe(context.Background(), "audio/sample.mp3", []byte("fake-audio"))
	if err != nil {
		t.Fatalf("Transcribe failed: %v", err)
	}
	if text != "plain transcript" {
		t.Fatalf("unexpected transcript: %q", text)
	}
}

func TestElevenLabsTranscribe_MapsAuthError(t *testing.T) {
	rt := elevenLabsRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Body:       io.NopCloser(bytes.NewBufferString("unauthorized")),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})

	client := elevenlabs.NewClient("test-api-key", "voice-default")
	client.HTTPClient = &http.Client{Transport: rt}

	_, err := client.Transcribe(context.Background(), "audio/sample.mp3", []byte("fake-audio"))
	if err == nil {
		t.Fatal("expected provider error")
	}
	var providerErr *model.ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("expected ProviderError, got %T", err)
	}
	if providerErr.Code != "ELEVENLABS_AUTH" {
		t.Fatalf("unexpected code: %s", providerErr.Code)
	}
}

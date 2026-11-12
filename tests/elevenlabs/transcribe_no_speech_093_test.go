package tests

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/dirstral/dir2mcp/internal/elevenlabs"
	"github.com/dirstral/dir2mcp/internal/model"
)

func elevenLabsSTTClient(body string) *elevenlabs.Client {
	rt := elevenLabsRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewBufferString(body)),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})
	client := elevenlabs.NewClient("test-api-key", "voice-default")
	client.HTTPClient = &http.Client{Transport: rt}
	return client
}

// TestElevenLabsTranscribe_WellFormedEmptyIsNoSpeech pins SPEC 0.93.0: a
// well-formed response whose text fields are empty is a decode that found no
// speech, so Transcribe returns ("", nil) instead of ELEVENLABS_FAILED.
func TestElevenLabsTranscribe_WellFormedEmptyIsNoSpeech(t *testing.T) {
	for name, body := range map[string]string{
		"text":       `{"language_code":"eng","language_probability":0.2,"text":"","words":[]}`,
		"transcript": `{"transcript":""}`,
		"segments":   `{"segments":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			text, err := elevenLabsSTTClient(body).Transcribe(context.Background(), "music.mp3", []byte("audio"))
			if err != nil || text != "" {
				t.Fatalf("want (\"\", nil), got (%q, %v)", text, err)
			}
		})
	}
}

// TestElevenLabsTranscribe_MalformedResponseStaysAnError: a body that names no
// text-bearing field stays a non-retryable ELEVENLABS_FAILED.
func TestElevenLabsTranscribe_MalformedResponseStaysAnError(t *testing.T) {
	_, err := elevenLabsSTTClient(`{"language_code":"eng"}`).Transcribe(context.Background(), "x.mp3", []byte("audio"))
	var pe *model.ProviderError
	if !errors.As(err, &pe) || pe.Code != "ELEVENLABS_FAILED" || pe.Retryable {
		t.Fatalf("want non-retryable ELEVENLABS_FAILED, got %v", err)
	}
}

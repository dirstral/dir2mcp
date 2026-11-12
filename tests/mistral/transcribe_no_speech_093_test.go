package tests

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dirstral/dir2mcp/internal/mistral"
	"github.com/dirstral/dir2mcp/internal/model"
)

func voxtralClient(t *testing.T, body string) *mistral.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := mistral.NewClient(srv.URL, "test-key")
	c.MaxRetries = 0
	return c
}

// TestTranscribe_WellFormedEmptyIsNoSpeech pins SPEC 0.93.0 for Voxtral: a
// well-formed response with empty text and no segments is a decode that found
// no speech, so Transcribe returns ("", nil) instead of MISTRAL_FAILED.
func TestTranscribe_WellFormedEmptyIsNoSpeech(t *testing.T) {
	for name, body := range map[string]string{
		"text and segments": `{"model":"voxtral-mini-latest","text":"","language":null,"segments":[]}`,
		"text only":         `{"text":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			text, err := voxtralClient(t, body).Transcribe(context.Background(), "music.mp3", []byte("audio"))
			if err != nil || text != "" {
				t.Fatalf("want (\"\", nil), got (%q, %v)", text, err)
			}
		})
	}
}

// TestTranscribe_MalformedResponseStaysAnError: a body with neither a text nor
// a segments field stays a non-retryable MISTRAL_FAILED.
func TestTranscribe_MalformedResponseStaysAnError(t *testing.T) {
	_, err := voxtralClient(t, `{"model":"voxtral-mini-latest"}`).Transcribe(context.Background(), "x.mp3", []byte("audio"))
	var pe *model.ProviderError
	if !errors.As(err, &pe) || pe.Code != "MISTRAL_FAILED" || pe.Retryable {
		t.Fatalf("want non-retryable MISTRAL_FAILED, got %v", err)
	}
}

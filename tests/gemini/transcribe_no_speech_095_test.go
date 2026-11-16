package tests

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// geminiSTTServer answers every generateContent request with body.
func geminiSTTServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestTranscribe_EmptyGeneratedTextIsNoSpeech pins SPEC 0.95.0 §8.2 for the
// native Gemini surface: the generated text is the text field of the
// normalized response, so empty or whitespace-only generated text is a
// no-speech result. Transcribe returns ("", nil), not GEMINI_FAILED.
func TestTranscribe_EmptyGeneratedTextIsNoSpeech(t *testing.T) {
	for name, body := range map[string]string{
		"empty text":      `{"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"STOP"}]}`,
		"whitespace text": `{"candidates":[{"content":{"parts":[{"text":" \n\t "}]},"finishReason":"STOP"}]}`,
		"no parts":        `{"candidates":[{"content":{"role":"model"},"finishReason":"STOP"}]}`,
		"no finishReason": `{"candidates":[{"content":{"parts":[{"text":""}]}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			text, err := newClient(geminiSTTServer(t, body).URL).Transcribe(context.Background(), "music.mp3", []byte("audio"))
			if err != nil || text != "" {
				t.Fatalf("want (\"\", nil), got (%q, %v)", text, err)
			}
		})
	}
}

// TestTranscribe_NoGeneratedTextFieldStaysAnError: a response with no
// candidate has no text field, and a candidate that the model did not finish
// is not a decode result. Both stay a non-retryable GEMINI_FAILED, so neither
// becomes a durable no_speech skip.
func TestTranscribe_NoGeneratedTextFieldStaysAnError(t *testing.T) {
	for name, body := range map[string]string{
		"no candidates":    `{}`,
		"empty candidates": `{"candidates":[]}`,
		"prompt blocked":   `{"promptFeedback":{"blockReason":"SAFETY"}}`,
		"safety stop":      `{"candidates":[{"finishReason":"SAFETY"}]}`,
		"token limit":      `{"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"MAX_TOKENS"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newClient(geminiSTTServer(t, body).URL).Transcribe(context.Background(), "music.mp3", []byte("audio"))
			var pe *model.ProviderError
			if !errors.As(err, &pe) || pe.Code != "GEMINI_FAILED" || pe.Retryable {
				t.Fatalf("want a non-retryable GEMINI_FAILED, got %v", err)
			}
		})
	}
}

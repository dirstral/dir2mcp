package tests

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// noSpeechBody is the measured answer of both whisper large-v3 and a GigaAM
// server to a 60 s window that holds only music: a well-formed verbose_json
// response with empty text and no segments.
const noSpeechBody = `{"task":"transcribe","language":"nn","duration":60.0,"text":"","segments":[]}`

func whisperServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestTranscribe_WellFormedEmptyIsNoSpeech pins SPEC 0.93.0 (§8.2): a
// well-formed response with empty text and no segment that holds text is a
// successful decode that found no speech. It is not WHISPER_FAILED.
func TestTranscribe_WellFormedEmptyIsNoSpeech(t *testing.T) {
	for name, body := range map[string]string{
		"verbose_json":        noSpeechBody,
		"text only":           `{"text":""}`,
		"segments only":       `{"segments":[]}`,
		"blank segment texts": `{"text":"  ","segments":[{"start":0,"end":3,"text":" "}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := newClient(whisperServer(t, body).URL, "")
			res, err := c.TranscribeStructured(context.Background(), "music.m4a", []byte("audio"))
			if err != nil {
				t.Fatalf("a well-formed empty response must not be an error, got %v", err)
			}
			if res.Text != "" || len(res.Words) != 0 {
				t.Fatalf("no speech must yield an empty transcript, got text=%q words=%d", res.Text, len(res.Words))
			}
			text, err := c.Transcribe(context.Background(), "music.m4a", []byte("audio"))
			if err != nil || text != "" {
				t.Fatalf("Transcribe: want (\"\", nil), got (%q, %v)", text, err)
			}
		})
	}
}

// TestTranscribe_NoSpeechKeepsTheLanguageReport: the language a server reports
// with empty text is kept. An identifier response with empty text that reports
// a language is a signal (§8.2.3); the pipeline ignores the report of a
// no-speech decode window itself.
func TestTranscribe_NoSpeechKeepsTheLanguageReport(t *testing.T) {
	c := newClient(whisperServer(t, `{"text":"","language":"ru","language_probability":0.91,"segments":[]}`).URL, "")
	res, err := c.TranscribeStructured(context.Background(), "probe.m4a", []byte("audio"))
	if err != nil || res.Text != "" {
		t.Fatalf("want a no-speech result, got text=%q err=%v", res.Text, err)
	}
	if res.Language != "ru" || res.LanguageConfidence != 0.91 {
		t.Fatalf("language report = %q (%v), want ru (0.91)", res.Language, res.LanguageConfidence)
	}
}

// TestTranscribe_WordsWithoutTextStayAnError: a response with timed words but
// no text contradicts itself. The server heard something, so it is not a
// no-speech result; it stays a non-retryable WHISPER_FAILED and the audio is
// decoded again on the next run.
func TestTranscribe_WordsWithoutTextStayAnError(t *testing.T) {
	for name, body := range map[string]string{
		"top-level words": `{"text":"","segments":[],"words":[{"word":"hello","start":0,"end":1}]}`,
		"segment words":   `{"text":"","segments":[{"start":0,"end":1,"text":"","words":[{"word":"hi","start":0,"end":1}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := newClient(whisperServer(t, body).URL, "")
			_, err := c.TranscribeStructured(context.Background(), "x.m4a", []byte("audio"))
			var pe *model.ProviderError
			if !errors.As(err, &pe) || pe.Code != "WHISPER_FAILED" || pe.Retryable {
				t.Fatalf("want non-retryable WHISPER_FAILED, got %v", err)
			}
		})
	}
}

// TestTranscribe_MalformedResponseStaysAnError pins the other half: a body that
// names neither a text nor a segments field, or has one of the wrong type, is
// malformed, so it stays a non-retryable WHISPER_FAILED.
func TestTranscribe_MalformedResponseStaysAnError(t *testing.T) {
	for name, body := range map[string]string{
		"empty object": `{}`,
		"null":         `null`,
		"null fields":  `{"text":null,"segments":null}`,
		"null text":    `{"text":null,"segments":[]}`,
		"text number":  `{"text":5}`,
		"segments obj": `{"text":"","segments":{}}`,
		"segment str":  `{"segments":["x"]}`,
		"segment text": `{"segments":[{"start":0,"end":1,"text":7}]}`,
		"other object": `{"task":"transcribe","language":"en","duration":12.0}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := newClient(whisperServer(t, body).URL, "")
			_, err := c.TranscribeStructured(context.Background(), "x.m4a", []byte("audio"))
			var pe *model.ProviderError
			if !errors.As(err, &pe) {
				t.Fatalf("want a ProviderError, got %v", err)
			}
			if pe.Code != "WHISPER_FAILED" || pe.Retryable {
				t.Fatalf("want non-retryable WHISPER_FAILED, got %+v", pe)
			}
		})
	}
}

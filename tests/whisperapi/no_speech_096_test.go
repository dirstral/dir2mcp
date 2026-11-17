package tests

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestTranscribe_WellFormedEmptyIsNoSpeech pins SPEC 0.96.0 (§8.2): a
// well-formed response with empty text and no segment that holds text is a
// successful decode that found no speech. It is not WHISPER_FAILED.
func TestTranscribe_WellFormedEmptyIsNoSpeech(t *testing.T) {
	for name, body := range map[string]string{
		"verbose_json":  noSpeechBody,
		"text only":     `{"text":""}`,
		"segments only": `{"segments":[]}`,
		// A null words field counts as absent.
		"null words":          `{"text":"","words":null,"segments":[{"text":"","words":null}]}`,
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
		// The text holds only whitespace, or is absent (SPEC 0.96.0 §8.2).
		"whitespace text, top-level words": `{"text":" ","words":[{"word":"hello","start":0,"end":1}]}`,
		"absent text, top-level words":     `{"segments":[],"words":[{"word":"hello","start":0,"end":1}]}`,
		// Every entry counts, also a blank one: the rule is "at least one entry".
		// A words field that is present and not an array.
		"words object":         `{"text":"","words":{}}`,
		"segment words string": `{"text":"","segments":[{"text":"","words":"hello"}]}`,
		"blank word entries":   `{"text":"","segments":[{"text":"","words":[{"word":" "}]}],"words":[{"word":""}]}`,
		"segment words":        `{"text":"","segments":[{"start":0,"end":1,"text":"","words":[{"word":"hi","start":0,"end":1}]}]}`,
		// Blank segment words must not hide a spoken top-level word.
		"blank segment words, spoken top-level word": `{"text":"","segments":[{"text":"","words":[{"word":" "}]}],"words":[{"word":"hello","start":0,"end":1}]}`,
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
		// A segment with a time range but no text key (SPEC 0.96.0 §8.2).
		"timed segment, no text": `{"text":"","segments":[{"start":0,"end":1}]}`,
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

// TestTranscribe_TimedSegmentWithEmptyTextIsNoSpeech: a segment that has a
// start, an end and an empty text is well-formed, so the response is a
// no-speech result (SPEC 0.96.0 §8.2). Only a timed segment with no text key
// is malformed.
func TestTranscribe_TimedSegmentWithEmptyTextIsNoSpeech(t *testing.T) {
	c := newClient(whisperServer(t, `{"text":"","segments":[{"start":0,"end":1,"text":""}]}`).URL, "")
	res, err := c.TranscribeStructured(context.Background(), "x.m4a", []byte("audio"))
	if err != nil || res.Text != "" {
		t.Fatalf("want an empty result and no error, got (%q, %v)", res.Text, err)
	}
}

// TestTranscribe_EmptyWordArraysAreNoSpeech: a words array with no entry
// reports no speech, so the response is a no-speech result.
func TestTranscribe_EmptyWordArraysAreNoSpeech(t *testing.T) {
	body := `{"text":"","segments":[{"text":"","words":[]}],"words":[]}`
	c := newClient(whisperServer(t, body).URL, "")
	res, err := c.TranscribeStructured(context.Background(), "x.m4a", []byte("audio"))
	if err != nil || res.Text != "" {
		t.Fatalf("want an empty result and no error, got (%q, %v)", res.Text, err)
	}
}

// TestTranscribe_SegmentTextWithWordsIsSpeech: the timed-words rule needs no
// text anywhere in the response. A segment that holds text makes the response
// well-formed speech, also with no top-level text.
func TestTranscribe_SegmentTextWithWordsIsSpeech(t *testing.T) {
	body := `{"segments":[{"start":0,"end":1,"text":"hello","words":[{"word":"hello","start":0,"end":1}]}]}`
	c := newClient(whisperServer(t, body).URL, "")
	res, err := c.TranscribeStructured(context.Background(), "talk.m4a", []byte("audio"))
	if err != nil || !strings.Contains(res.Text, "hello") {
		t.Fatalf("want the segment text and no error, got (%q, %v)", res.Text, err)
	}
}

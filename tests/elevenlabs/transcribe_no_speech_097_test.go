package tests

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
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

// TestElevenLabsTranscribe_WellFormedEmptyIsNoSpeech pins SPEC 0.97.0: a
// well-formed response whose text fields are empty is a decode that found no
// speech, so Transcribe returns ("", nil) instead of ELEVENLABS_FAILED.
func TestElevenLabsTranscribe_WellFormedEmptyIsNoSpeech(t *testing.T) {
	for name, body := range map[string]string{
		"text":       `{"language_code":"eng","language_probability":0.2,"text":"","words":[]}`,
		"transcript": `{"transcript":""}`,
		"segments":   `{"segments":[]}`,
		// A null words field counts as absent.
		"null words": `{"text":"","words":null,"segments":[{"text":"","words":null}]}`,
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
	for _, body := range []string{
		`{"language_code":"eng"}`, `{"text":null}`, `{"text":"","transcript":null}`,
		// A timed segment with no text key (SPEC 0.97.0 §8.2), in seconds and
		// in the millisecond form this adapter reads.
		`{"segments":[{"start":1.0}]}`, `{"segments":[{"start_ms":1000}]}`,
		// Timed words with no text (SPEC 0.97.0 §8.2).
		`{"text":" ","words":[{"text":"hello","start":0,"end":1}]}`,
		// A words field that is present and not an array.
		`{"text":"","words":{}}`,
		`{"text":"","segments":[{"text":"","words":7}]}`,
	} {
		_, err := elevenLabsSTTClient(body).Transcribe(context.Background(), "x.mp3", []byte("audio"))
		var pe *model.ProviderError
		if !errors.As(err, &pe) || pe.Code != "ELEVENLABS_FAILED" || pe.Retryable {
			t.Fatalf("%s: want non-retryable ELEVENLABS_FAILED, got %v", body, err)
		}
	}
}

// TestElevenLabsTranscribe_SegmentTextWithWordsIsSpeech: the timed-words rule
// needs no text anywhere in the response. A segment that holds text makes the
// response well-formed speech, also with no top-level text.
func TestElevenLabsTranscribe_SegmentTextWithWordsIsSpeech(t *testing.T) {
	body := `{"segments":[{"start":0,"end":1,"text":"hello","words":[{"text":"hello","start":0,"end":1}]}]}`
	text, err := elevenLabsSTTClient(body).Transcribe(context.Background(), "talk.mp3", []byte("audio"))
	if err != nil || !strings.Contains(text, "hello") {
		t.Fatalf("want the segment text and no error, got (%q, %v)", text, err)
	}
}

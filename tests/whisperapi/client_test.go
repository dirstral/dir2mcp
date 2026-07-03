package tests

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/whisperapi"
)

// jsonSegments is a standard OpenAI /v1/audio/transcriptions json response
// with two timed segments (verbose_json additionally carries words[]).
const jsonSegments = `{
  "text": "hello there general",
  "language": "en",
  "duration": 75.0,
  "segments": [
    {"start": 0.0, "end": 4.2, "text": " hello there"},
    {"start": 65.0, "end": 70.0, "text": "general"}
  ]
}`

func newClient(url, apiKey string) *whisperapi.Client {
	c := whisperapi.NewClient(url, apiKey)
	c.InitialBackoff = time.Millisecond
	c.MaxBackoff = time.Millisecond
	return c
}

// parsedForm captures the multipart fields a request carried.
type parsedForm struct {
	model          string
	language       string
	responseFormat string
	fileName       string
	auth           string
	hasAuth        bool
	vadFilter      string
	hasVADFilter   bool
	task           string
	hasTask        bool
}

func parseMultipart(t *testing.T, r *http.Request) parsedForm {
	t.Helper()
	out := parsedForm{}
	out.auth = r.Header.Get("Authorization")
	out.hasAuth = out.auth != ""

	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse content type: %v", err)
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			t.Fatalf("read multipart part: %v", perr)
		}
		data, _ := io.ReadAll(part)
		switch part.FormName() {
		case "model":
			out.model = string(data)
		case "language":
			out.language = string(data)
		case "response_format":
			out.responseFormat = string(data)
		case "file":
			out.fileName = part.FileName()
		case "vad_filter":
			out.vadFilter = string(data)
			out.hasVADFilter = true
		case "task":
			out.task = string(data)
			out.hasTask = true
		}
	}
	return out
}

// TestTranscribeSegmentsFormatting asserts the json segments response is
// rendered as [mm:ss]-prefixed lines (the format chunkTranscriptByTime
// consumes) and that the multipart fields are sent.
func TestTranscribeSegmentsFormatting(t *testing.T) {
	var got parsedForm
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		got = parseMultipart(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, jsonSegments)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "secret-key")
	c.DefaultModel = "faster-whisper-large-v3"
	c.DefaultLanguage = "en"

	out, err := c.Transcribe(context.Background(), "podcast/ep1.mp3", []byte("audiobytes"))
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}

	want := "[00:00] hello there\n[01:05] general"
	if out != want {
		t.Fatalf("transcript = %q, want %q", out, want)
	}

	// multipart fields
	if got.model != "faster-whisper-large-v3" {
		t.Errorf("model field = %q, want faster-whisper-large-v3", got.model)
	}
	if got.language != "en" {
		t.Errorf("language field = %q, want en", got.language)
	}
	if got.responseFormat != "verbose_json" {
		t.Errorf("response_format = %q, want verbose_json (default, #252)", got.responseFormat)
	}
	if !strings.HasSuffix(got.fileName, "ep1.mp3") {
		t.Errorf("file name = %q, want suffix ep1.mp3", got.fileName)
	}
	// auth header present when api_key set
	if got.auth != "Bearer secret-key" {
		t.Errorf("auth header = %q, want Bearer secret-key", got.auth)
	}
}

// TestVerboseJSONResponseFormat asserts response_format=verbose_json is
// sent when configured.
func TestVerboseJSONResponseFormat(t *testing.T) {
	var got parsedForm
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = parseMultipart(t, r)
		_, _ = io.WriteString(w, jsonSegments)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "")
	c.ResponseFormat = whisperapi.ResponseFormatVerboseJSON

	if _, err := c.Transcribe(context.Background(), "a.wav", []byte("x")); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if got.responseFormat != "verbose_json" {
		t.Errorf("response_format = %q, want verbose_json", got.responseFormat)
	}
}

// TestVADFilterField asserts the OpenAI-compatible vad_filter=true multipart
// field is sent only when VADFilter is enabled (dir2mcp#258, config media.vad),
// and is omitted entirely by default so non-VAD servers see no extra field.
func TestVADFilterField(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		wantSet bool
	}{
		{"enabled sends vad_filter=true", true, true},
		{"disabled omits vad_filter", false, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var got parsedForm
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = parseMultipart(t, r)
				_, _ = io.WriteString(w, jsonSegments)
			}))
			defer srv.Close()

			c := newClient(srv.URL, "")
			c.VADFilter = tc.enabled

			if _, err := c.Transcribe(context.Background(), "a.wav", []byte("x")); err != nil {
				t.Fatalf("Transcribe: %v", err)
			}
			if got.hasVADFilter != tc.wantSet {
				t.Fatalf("vad_filter present = %v, want %v", got.hasVADFilter, tc.wantSet)
			}
			if tc.wantSet && got.vadFilter != "true" {
				t.Errorf("vad_filter = %q, want true", got.vadFilter)
			}
		})
	}
}

// TestTaskField asserts the multipart `task` field is sent only when the client
// is configured to translate. Plain transcription (the default, and an explicit
// "transcribe") must omit the field entirely so servers that don't accept it
// keep working, and translate must decode straight to English via task=translate.
func TestTaskField(t *testing.T) {
	for _, tc := range []struct {
		name     string
		task     string
		wantSet  bool
		wantTask string
	}{
		{"default omits task", "", false, ""},
		{"explicit transcribe omits task", whisperapi.TaskTranscribe, false, ""},
		{"translate sends task=translate", whisperapi.TaskTranslate, true, "translate"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var got parsedForm
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = parseMultipart(t, r)
				_, _ = io.WriteString(w, jsonSegments)
			}))
			defer srv.Close()

			c := newClient(srv.URL, "")
			c.Task = tc.task

			if _, err := c.Transcribe(context.Background(), "a.wav", []byte("x")); err != nil {
				t.Fatalf("Transcribe: %v", err)
			}
			if got.hasTask != tc.wantSet {
				t.Fatalf("task present = %v, want %v", got.hasTask, tc.wantSet)
			}
			if tc.wantSet && got.task != tc.wantTask {
				t.Errorf("task = %q, want %q", got.task, tc.wantTask)
			}
		})
	}
}

// TestNoAuthWhenCredentialLess asserts no Authorization header is sent for a
// credential-less (self-hosted) endpoint, and the language field is omitted
// when no language is configured.
func TestNoAuthWhenCredentialLess(t *testing.T) {
	var got parsedForm
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = parseMultipart(t, r)
		_, _ = io.WriteString(w, jsonSegments)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "") // no api_key

	if _, err := c.Transcribe(context.Background(), "a.wav", []byte("x")); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if got.hasAuth {
		t.Errorf("auth header should be absent for credential-less endpoint, got %q", got.auth)
	}
	if got.language != "" {
		t.Errorf("language field should be empty when unset, got %q", got.language)
	}
}

// TestRetryOn5xx asserts a retryable 5xx is retried and eventually succeeds.
func TestRetryOn5xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "overloaded")
			return
		}
		_, _ = io.WriteString(w, jsonSegments)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "k")
	out, err := c.Transcribe(context.Background(), "a.wav", []byte("x"))
	if err != nil {
		t.Fatalf("Transcribe after retry: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("calls = %d, want 2 (one 5xx then success)", n)
	}
	if !strings.HasPrefix(out, "[00:00] hello there") {
		t.Errorf("transcript = %q, want [mm:ss] formatting", out)
	}
}

// TestAuthErrorNotRetried asserts a 401 is surfaced as a non-retryable
// WHISPER_AUTH error and not retried.
func TestAuthErrorNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "bad key")
	}))
	defer srv.Close()

	c := newClient(srv.URL, "k")
	_, err := c.Transcribe(context.Background(), "a.wav", []byte("x"))
	if err == nil {
		t.Fatal("expected auth error")
	}
	pe, ok := err.(*model.ProviderError)
	if !ok {
		t.Fatalf("error type = %T, want *model.ProviderError", err)
	}
	if pe.Code != "WHISPER_AUTH" || pe.Retryable {
		t.Errorf("err = %+v, want WHISPER_AUTH non-retryable", pe)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("calls = %d, want 1 (auth error not retried)", n)
	}
}

// TestFlatTextFallback asserts a response without segments falls back to the
// flat text field.
func TestFlatTextFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"text":"just flat text"}`)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "k")
	out, err := c.Transcribe(context.Background(), "a.wav", []byte("x"))
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if out != "just flat text" {
		t.Errorf("transcript = %q, want flat text fallback", out)
	}
}

// TestEmptyAudioRejected asserts empty input is rejected locally as a
// non-retryable WHISPER_FAILED without any HTTP call.
func TestEmptyAudioRejected(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = io.WriteString(w, jsonSegments)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "k")
	_, err := c.Transcribe(context.Background(), "a.wav", nil)
	if err == nil {
		t.Fatal("expected error for empty audio")
	}
	pe, ok := err.(*model.ProviderError)
	if !ok {
		t.Fatalf("error type = %T, want *model.ProviderError", err)
	}
	if pe.Code != "WHISPER_FAILED" || pe.Retryable {
		t.Errorf("err = %+v, want WHISPER_FAILED non-retryable", pe)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Errorf("calls = %d, want 0 (no HTTP for empty input)", n)
	}
}

// TestMalformedJSON asserts a 200 with an unparseable body surfaces as a
// non-retryable WHISPER_FAILED decode error (not retried).
func TestMalformedJSON(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = io.WriteString(w, `{not valid json`)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "k")
	_, err := c.Transcribe(context.Background(), "a.wav", []byte("x"))
	if err == nil {
		t.Fatal("expected decode error")
	}
	pe, ok := err.(*model.ProviderError)
	if !ok {
		t.Fatalf("error type = %T, want *model.ProviderError", err)
	}
	if pe.Code != "WHISPER_FAILED" || pe.Retryable {
		t.Errorf("err = %+v, want WHISPER_FAILED non-retryable", pe)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("calls = %d, want 1 (decode error not retried)", n)
	}
}

// TestNon200EmptyBody asserts a non-200 with an empty body still yields a
// categorized ProviderError with a non-empty message (no panic, no leak).
func TestNon200EmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest) // 4xx, non-retryable, empty body
	}))
	defer srv.Close()

	c := newClient(srv.URL, "k")
	_, err := c.Transcribe(context.Background(), "a.wav", []byte("x"))
	if err == nil {
		t.Fatal("expected error for non-200")
	}
	pe, ok := err.(*model.ProviderError)
	if !ok {
		t.Fatalf("error type = %T, want *model.ProviderError", err)
	}
	if pe.Code != "WHISPER_FAILED" || pe.Retryable {
		t.Errorf("err = %+v, want WHISPER_FAILED non-retryable", pe)
	}
	if strings.TrimSpace(pe.Message) == "" {
		t.Error("message should be non-empty even for empty upstream body")
	}
	if pe.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", pe.StatusCode)
	}
}

// TestContextCancellationRespected asserts an already-cancelled context
// short-circuits the request rather than hitting the server.
func TestContextCancellationRespected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, jsonSegments)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := newClient(srv.URL, "k")
	if _, err := c.Transcribe(ctx, "a.wav", []byte("x")); err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

// verboseJSONWords is a verbose_json response carrying per-segment word
// timestamps (the faster-whisper / OpenAI-compatible shape #252 parses).
const verboseJSONWords = `{
  "text": "hello there general",
  "language": "en",
  "duration": 75.0,
  "segments": [
    {"start": 0.0, "end": 4.2, "text": " hello there", "words": [
      {"word": "hello", "start": 0.0, "end": 0.5},
      {"word": "there", "start": 0.5, "end": 1.0}
    ]},
    {"start": 65.0, "end": 70.0, "text": "general", "words": [
      {"word": "general", "start": 65.0, "end": 65.8}
    ]}
  ]
}`

// verboseJSONTopLevelWords carries a top-level words[] array instead of
// per-segment words (some servers emit this shape).
const verboseJSONTopLevelWords = `{
  "text": "hello there",
  "segments": [{"start": 0.0, "end": 2.0, "text": "hello there"}],
  "words": [
    {"word": "hello", "start": 0.0, "end": 0.5},
    {"word": "there", "start": 0.5, "end": 1.0}
  ]
}`

// TestTranscribeStructuredWords asserts TranscribeStructured returns the same
// segment text as Transcribe plus parsed per-word timing in ms (#252).
func TestTranscribeStructuredWords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, verboseJSONWords)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "k")
	res, err := c.TranscribeStructured(context.Background(), "a.wav", []byte("x"))
	if err != nil {
		t.Fatalf("TranscribeStructured: %v", err)
	}

	wantText := "[00:00] hello there\n[01:05] general"
	if res.Text != wantText {
		t.Fatalf("text = %q, want %q", res.Text, wantText)
	}
	want := []model.TimedWord{
		{Word: "hello", StartMS: 0, EndMS: 500},
		{Word: "there", StartMS: 500, EndMS: 1000},
		{Word: "general", StartMS: 65000, EndMS: 65800},
	}
	if len(res.Words) != len(want) {
		t.Fatalf("words len = %d, want %d (%+v)", len(res.Words), len(want), res.Words)
	}
	for i := range want {
		if res.Words[i] != want[i] {
			t.Errorf("word[%d] = %+v, want %+v", i, res.Words[i], want[i])
		}
	}
}

// TestTranscribeStructuredTopLevelWords asserts a top-level words[] array is
// used as a fallback when segments carry no words.
func TestTranscribeStructuredTopLevelWords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, verboseJSONTopLevelWords)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "k")
	res, err := c.TranscribeStructured(context.Background(), "a.wav", []byte("x"))
	if err != nil {
		t.Fatalf("TranscribeStructured: %v", err)
	}
	if len(res.Words) != 2 || res.Words[1].Word != "there" || res.Words[1].StartMS != 500 {
		t.Fatalf("top-level words = %+v, want 2 words ending at there@500ms", res.Words)
	}
}

// TestTranscribeStructuredWordsAbsent asserts a segments-only response yields a
// transcript with no word timing (identical to the words-absent path).
func TestTranscribeStructuredWordsAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, jsonSegments)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "k")
	res, err := c.TranscribeStructured(context.Background(), "a.wav", []byte("x"))
	if err != nil {
		t.Fatalf("TranscribeStructured: %v", err)
	}
	if res.Words != nil {
		t.Errorf("words = %+v, want nil for segments-only response", res.Words)
	}
	if res.Text != "[00:00] hello there\n[01:05] general" {
		t.Errorf("text = %q", res.Text)
	}
}

// TestClientImplementsStructuredTranscriber documents the optional capability
// contract: the whisper client is a model.StructuredTranscriber.
func TestClientImplementsStructuredTranscriber(t *testing.T) {
	var _ model.StructuredTranscriber = whisperapi.NewClient("http://x", "")
}

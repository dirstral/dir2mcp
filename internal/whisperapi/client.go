// Package whisperapi is a pure-Go client for a self-hosted,
// OpenAI-compatible speech-to-text endpoint — the GPU-VPS path
// (dir2mcp#240, epic #250).
//
// It targets the STANDARD OpenAI POST {BaseURL}/v1/audio/transcriptions
// multipart contract, which is implemented by common self-hosted shims
// (vLLM-audio, whisper-server, faster-whisper OpenAI servers). The
// investigation behind #240 found that livevtt's own servers are NOT
// OpenAI-compatible, so this client deliberately speaks the portable
// standard endpoint rather than any livevtt-specific protocol.
//
// The endpoint may be credential-less (a self-hosted box on a private
// network): a Bearer token is sent only when an api_key is configured.
//
// Transcripts are parsed into the same `[mm:ss] text` per-segment line
// format the rest of the ingest pipeline expects (so chunkTranscriptByTime
// works unchanged), mirroring the Mistral/Voxtral transcriber.
package whisperapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
)

const (
	defaultRequestTimeout = 120 * time.Second
	defaultMaxRetries     = 3
	defaultInitialBackoff = 250 * time.Millisecond
	defaultMaxBackoff     = 2 * time.Second
	// defaultMaxPayloadBytes bounds the audio payload size so callers
	// avoid sending absurdly large blobs that fail upstream or time out.
	defaultMaxPayloadBytes = 50 * 1024 * 1024

	// DefaultModel is a sensible fallback model name. Self-hosted servers
	// frequently ignore the field or accept "whisper-1" as an alias, but
	// operators SHOULD set an explicit model via the provider profile.
	DefaultModel = "whisper-1"

	// ResponseFormatJSON yields segment-level timestamps.
	ResponseFormatJSON = "json"
	// ResponseFormatVerboseJSON additionally yields word-level timestamps
	// (consumed by #252; this client already parses them into the struct).
	ResponseFormatVerboseJSON = "verbose_json"
)

// Client speaks the OpenAI-compatible /v1/audio/transcriptions contract
// against a self-hosted base URL.
//
// Error codes:
//   - WHISPER_AUTH (non-retryable): upstream 401/403.
//   - WHISPER_RATE_LIMIT (retryable): upstream 429.
//   - WHISPER_FAILED (retryable for network/5xx, non-retryable otherwise).
type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client

	MaxRetries     int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// MaxPayloadBytes bounds the audio payload size (bytes). Values <= 0
	// fall back to defaultMaxPayloadBytes.
	MaxPayloadBytes int

	// DefaultModel is sent as the multipart `model` field. Empty falls
	// back to the package DefaultModel constant.
	DefaultModel string
	// DefaultLanguage optionally sets the `language` hint. Empty means
	// provider auto-detection (no language field is sent).
	DefaultLanguage string
	// ResponseFormat selects the response schema: "verbose_json" (default,
	// segments + word timestamps per spec §8.6.1) or "json" (segments only).
	// Empty falls back to ResponseFormatVerboseJSON.
	ResponseFormat string

	// VADFilter, when true, sends the OpenAI-compatible `vad_filter=true` form
	// field so a server that supports voice-activity detection (e.g.
	// faster-whisper) skips non-speech audio (dir2mcp#258, config `media.vad`).
	// Servers without VAD support ignore the field. Default false.
	VADFilter bool
}

// compile-time interface check.
var _ model.Transcriber = (*Client)(nil)

// NewClient constructs a client with safe default retry/timeout settings.
// apiKey may be empty for a credential-less self-hosted endpoint.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL:         strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:          strings.TrimSpace(apiKey),
		HTTPClient:      &http.Client{Timeout: defaultRequestTimeout},
		MaxRetries:      defaultMaxRetries,
		InitialBackoff:  defaultInitialBackoff,
		MaxBackoff:      defaultMaxBackoff,
		MaxPayloadBytes: defaultMaxPayloadBytes,
		DefaultModel:    DefaultModel,
		// Default to verbose_json so the server returns per-word timestamps
		// (spec §8.6.1, #252). Segment parsing is unaffected — verbose_json is a
		// superset of json — and operators MAY override to "json" via the
		// provider profile if their server omits or mishandles word timing.
		ResponseFormat: ResponseFormatVerboseJSON,
	}
}

// transcribeResponse models the standard OpenAI transcription schema. The
// `words` field is only populated when response_format=verbose_json; it is
// carried here (unused by segment parsing today) so #252 can build
// word-level timestamps on top of this struct without re-shaping it.
type transcribeResponse struct {
	Text     string              `json:"text"`
	Language string              `json:"language,omitempty"`
	Duration float64             `json:"duration,omitempty"`
	Segments []transcriptSegment `json:"segments,omitempty"`
	// Words is the top-level word array some verbose_json servers return
	// instead of (or in addition to) per-segment words. Used as a fallback
	// when no segment carries its own words (#252).
	Words []transcriptWord `json:"words,omitempty"`
}

type transcriptSegment struct {
	Start float64          `json:"start"`
	End   float64          `json:"end"`
	Text  string           `json:"text"`
	Words []transcriptWord `json:"words,omitempty"`
}

// transcriptWord is the verbose_json word-level timestamp (#252). Present
// only when response_format=verbose_json.
type transcriptWord struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// Transcribe implements model.Transcriber. It returns only the segment text so
// the contract is unchanged; per-word timing is available via
// TranscribeStructured.
func (c *Client) Transcribe(ctx context.Context, relPath string, data []byte) (string, error) {
	res, err := c.TranscribeStructured(ctx, relPath, data)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// TranscribeStructured implements model.StructuredTranscriber: it returns the
// same segment text as Transcribe plus per-word timing when the server provided
// it (spec §8.6.1, #252). Words is nil when the response carried none.
func (c *Client) TranscribeStructured(ctx context.Context, relPath string, data []byte) (model.TranscriptResult, error) {
	if len(data) == 0 {
		return model.TranscriptResult{}, &model.ProviderError{Code: "WHISPER_FAILED", Message: "transcription input is empty", Retryable: false}
	}
	maxPayload := c.MaxPayloadBytes
	if maxPayload <= 0 {
		maxPayload = defaultMaxPayloadBytes
	}
	if len(data) > maxPayload {
		return model.TranscriptResult{}, &model.ProviderError{
			Code:      "WHISPER_FAILED",
			Message:   fmt.Sprintf("transcription input too large (%d bytes, limit %d)", len(data), maxPayload),
			Retryable: false,
		}
	}
	return c.transcribeWithRetry(ctx, relPath, data)
}

// compile-time interface check for the optional word-timing capability.
var _ model.StructuredTranscriber = (*Client)(nil)

func (c *Client) transcribeWithRetry(ctx context.Context, relPath string, data []byte) (model.TranscriptResult, error) {
	maxAttempts := c.MaxRetries + 1
	if maxAttempts <= 0 {
		maxAttempts = 1
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		out, err := c.transcribeOnce(ctx, relPath, data)
		if err == nil {
			return out, nil
		}
		lastErr = err

		var providerErr *model.ProviderError
		if !errors.As(err, &providerErr) || !providerErr.Retryable || attempt == maxAttempts-1 {
			return model.TranscriptResult{}, err
		}

		if waitErr := c.wait(ctx, c.backoffForAttempt(attempt)); waitErr != nil {
			return model.TranscriptResult{}, waitErr
		}
	}
	return model.TranscriptResult{}, lastErr
}

// buildBody builds the multipart form body. Returns the raw body bytes and
// the writer (for the Content-Type boundary), or a ProviderError.
func (c *Client) buildBody(relPath string, data []byte) ([]byte, *multipart.Writer, error) {
	fail := func(msg string, cause error) ([]byte, *multipart.Writer, error) {
		return nil, nil, &model.ProviderError{Code: "WHISPER_FAILED", Message: msg, Retryable: false, Cause: cause}
	}
	fileName := strings.TrimSpace(filepath.Base(relPath))
	if fileName == "" || fileName == "." || fileName == string(filepath.Separator) {
		fileName = "audio.wav"
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	fileField, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return fail("failed to build transcription request body", err)
	}
	if _, err := fileField.Write(data); err != nil {
		return fail("failed to write transcription input", err)
	}

	modelName := strings.TrimSpace(c.DefaultModel)
	if modelName == "" {
		modelName = DefaultModel
	}
	if err := writer.WriteField("model", modelName); err != nil {
		return fail("failed to write transcription model", err)
	}
	if err := writer.WriteField("response_format", c.responseFormat()); err != nil {
		return fail("failed to write transcription response_format", err)
	}
	if language := strings.TrimSpace(c.DefaultLanguage); language != "" {
		if err := writer.WriteField("language", language); err != nil {
			return fail("failed to write transcription language", err)
		}
	}
	if c.VADFilter {
		if err := writer.WriteField("vad_filter", "true"); err != nil {
			return fail("failed to write transcription vad_filter", err)
		}
	}
	if err := writer.Close(); err != nil {
		return fail("failed to finalize transcription request body", err)
	}
	return buf.Bytes(), writer, nil
}

// responseFormat returns the configured response format, defaulting to
// verbose_json (the word-timing superset). An unknown value is passed through
// unchanged so operators can target server-specific formats; only the empty
// value is defaulted.
func (c *Client) responseFormat() string {
	if f := strings.TrimSpace(c.ResponseFormat); f != "" {
		return f
	}
	return ResponseFormatVerboseJSON
}

func (c *Client) transcribeOnce(ctx context.Context, relPath string, data []byte) (model.TranscriptResult, error) {
	if strings.TrimSpace(c.BaseURL) == "" {
		return model.TranscriptResult{}, &model.ProviderError{Code: "WHISPER_FAILED", Message: "missing whisper base_url", Retryable: false}
	}
	body, writer, err := c.buildBody(relPath, data)
	if err != nil {
		return model.TranscriptResult{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/audio/transcriptions", bytes.NewReader(body))
	if err != nil {
		return model.TranscriptResult{}, &model.ProviderError{Code: "WHISPER_FAILED", Message: "failed to build transcription request", Retryable: false, Cause: err}
	}
	// Bearer auth is optional: only set it for credentialed endpoints.
	if key := strings.TrimSpace(c.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultRequestTimeout}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return model.TranscriptResult{}, &model.ProviderError{Code: "WHISPER_FAILED", Message: "transcription request failed", Retryable: true, Cause: err}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return model.TranscriptResult{}, httpError(resp)
	}

	var parsed transcribeResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return model.TranscriptResult{}, &model.ProviderError{
			Code:      "WHISPER_FAILED",
			Message:   "failed to decode transcription response",
			Retryable: false,
			Cause:     err,
		}
	}

	if segText, ok := parseTranscriptSegments(parsed); ok {
		return model.TranscriptResult{Text: segText, Words: parsed.timedWords()}, nil
	}
	text := strings.TrimSpace(parsed.Text)
	if text == "" {
		return model.TranscriptResult{}, &model.ProviderError{
			Code:      "WHISPER_FAILED",
			Message:   "transcription response had no text content",
			Retryable: false,
		}
	}
	// Flat-text fallback (no segments): words have no segment frame to anchor
	// against, so emit text only.
	return model.TranscriptResult{Text: text}, nil
}

// timedWords flattens the verbose_json word timestamps into a time-ordered
// []model.TimedWord with absolute ms offsets (#252). It prefers per-segment
// words and falls back to a top-level words array; returns nil when neither is
// present or all words are empty/zero-length, so a words-absent response yields
// no word timing.
func (r transcribeResponse) timedWords() []model.TimedWord {
	var raw []transcriptWord
	for _, seg := range r.Segments {
		raw = append(raw, seg.Words...)
	}
	if len(raw) == 0 {
		raw = r.Words
	}
	if len(raw) == 0 {
		return nil
	}
	out := make([]model.TimedWord, 0, len(raw))
	for _, w := range raw {
		word := strings.TrimSpace(w.Word)
		if word == "" {
			continue
		}
		startMS := secondsToMS(w.Start)
		endMS := secondsToMS(w.End)
		if endMS < startMS {
			endMS = startMS
		}
		out = append(out, model.TimedWord{Word: word, StartMS: startMS, EndMS: endMS})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// secondsToMS converts fractional seconds to whole milliseconds, clamping
// negative values to 0.
func secondsToMS(sec float64) int {
	if sec <= 0 {
		return 0
	}
	return int(sec*1000 + 0.5)
}

// parseTranscriptSegments converts timed segments into `[mm:ss] text` (or
// `[mm:ss.mmm] text` when the segment starts sub-second) lines, the format
// chunkTranscriptByTime consumes. Sub-second start times are preserved so
// distinct in-second segments do not collapse onto one marker (issue #431).
// Returns ("", false) when no non-empty segments are present (caller falls back
// to flat text).
func parseTranscriptSegments(parsed transcribeResponse) (string, bool) {
	if len(parsed.Segments) == 0 {
		return "", false
	}
	lines := make([]string, 0, len(parsed.Segments))
	for _, seg := range parsed.Segments {
		text := strings.TrimSpace(seg.Text)
		if text == "" {
			continue
		}
		lines = append(lines, model.FormatTranscriptTimestamp(secondsToMS(seg.Start))+" "+text)
	}
	if len(lines) == 0 {
		return "", false
	}
	return strings.Join(lines, "\n"), true
}

func httpError(resp *http.Response) error {
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	errMsg := strings.TrimSpace(string(bodyBytes))
	if errMsg == "" {
		errMsg = "upstream returned non-200 response"
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &model.ProviderError{Code: "WHISPER_AUTH", Message: errMsg, Retryable: false, StatusCode: resp.StatusCode}
	case resp.StatusCode == http.StatusTooManyRequests:
		return &model.ProviderError{Code: "WHISPER_RATE_LIMIT", Message: errMsg, Retryable: true, StatusCode: resp.StatusCode}
	case resp.StatusCode >= http.StatusInternalServerError:
		return &model.ProviderError{Code: "WHISPER_FAILED", Message: errMsg, Retryable: true, StatusCode: resp.StatusCode}
	default:
		return &model.ProviderError{Code: "WHISPER_FAILED", Message: errMsg, Retryable: false, StatusCode: resp.StatusCode}
	}
}

func (c *Client) backoffForAttempt(attempt int) time.Duration {
	initial := c.InitialBackoff
	if initial <= 0 {
		initial = defaultInitialBackoff
	}
	maxBackoff := c.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = defaultMaxBackoff
	}
	backoff := initial
	for i := 0; i < attempt; i++ {
		backoff *= 2
		if backoff >= maxBackoff {
			return maxBackoff
		}
	}
	return backoff
}

func (c *Client) wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

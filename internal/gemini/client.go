// Package gemini provides a direct-HTTP adapter for Google Gemini. Chat
// (model.Generator) and STT use Gemini's OpenAI-compatible surface (POST
// {base}/chat/completions); embeddings (model.Embedder) use Gemini's
// NATIVE surface (POST {base-without-/openai}/models/{model}:batchEmbedContents)
// because only the native API supports `taskType` (asymmetric embeddings,
// SPEC 8.1.5) and `outputDimensionality` (Matryoshka/MRL, SPEC 8.1.6) —
// the OpenAI-compatible /embeddings shim exposes neither.
//
// It mirrors internal/mistral and internal/cohere (BaseURL/APIKey/
// HTTPClient, bounded exponential retry, typed model.ProviderError) so
// callers can depend on model.Embedder / model.Generator without taking
// a hard dependency on this package.
//
// Gemini embeddings are ASYMMETRIC (SPEC 8.1.5): the model.EmbedRole is
// mapped onto `taskType` (RETRIEVAL_DOCUMENT / RETRIEVAL_QUERY, and
// CODE_RETRIEVAL_QUERY for a query against the configured code model).
//
// TODO(spec 8.1.2): native Gemini STT/TTS. The capability matrix marks
// `gemini` as stt ✅ tts ✅, but this adapter implements embed + chat
// only. Audio (model.Transcriber / TTS) is deferred to a follow-up PR
// and is intentionally not implemented here.
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
)

const (
	// defaultBaseURL is Gemini's OpenAI-compatible endpoint (chat/STT).
	// The adapter appends /chat/completions to this base.
	defaultBaseURL = "https://generativelanguage.googleapis.com/v1beta/openai"
	// geminiNativeBaseURL is the native API base used for embeddings
	// (models/{model}:batchEmbedContents) — the OpenAI-compat base minus
	// the trailing /openai. Used when BaseURL is the default/empty.
	geminiNativeBaseURL      = "https://generativelanguage.googleapis.com/v1beta"
	defaultRequestTimeout    = 30 * time.Second
	defaultGenerationTimeout = 120 * time.Second
	defaultMaxRetries        = 3
	defaultInitialBackoff    = 250 * time.Millisecond
	defaultMaxBackoff        = 2 * time.Second
	defaultBatchSize         = 64

	// DefaultEmbedModel / DefaultChatModel are fallbacks used only when
	// the caller passes an empty model name. The config resolver
	// normally supplies explicit models per profile.
	DefaultEmbedModel = "gemini-embedding-001"
	DefaultChatModel  = "gemini-2.5-flash"
	DefaultSTTModel   = "gemini-2.5-flash"
	DefaultTTSModel   = "gemini-2.5-flash-preview-tts"
	DefaultTTSVoice   = "Kore"
)

// Client is a Gemini adapter that speaks the OpenAI-compatible wire
// protocol.
//
// Error codes (carried on *model.ProviderError):
//   - GEMINI_AUTH (non-retryable): missing key, 401/403
//   - GEMINI_RATE_LIMIT (retryable): 429
//   - GEMINI_FAILED (retryable for network/5xx, else non-retryable)
type Client struct {
	BaseURL           string
	APIKey            string
	HTTPClient        *http.Client
	MaxRetries        int
	InitialBackoff    time.Duration
	MaxBackoff        time.Duration
	BatchSize         int
	GenerationTimeout time.Duration
	// DefaultEmbedModel/DefaultChatModel are used when the corresponding
	// call is made with an empty model name.
	DefaultEmbedModel string
	// CodeEmbedModel is the resolved code-embedding model name; a query
	// against it selects Gemini's CODE_RETRIEVAL_QUERY task type (SPEC
	// 8.1.5). Empty disables the code-aware refinement.
	CodeEmbedModel string
	// EmbedTextDim / EmbedCodeDim request a specific output dimensionality
	// (Matryoshka/MRL, SPEC 8.1.6); zero means the model's native size.
	// When set, returned vectors are L2-normalized.
	EmbedTextDim     int
	EmbedCodeDim     int
	DefaultChatModel string
	DefaultSTTModel  string
	DefaultTTSModel  string
	DefaultTTSVoice  string
}

// compile-time assertions that *Client implements the model contracts.
var (
	_ model.Embedder    = (*Client)(nil)
	_ model.Generator   = (*Client)(nil)
	_ model.Transcriber = (*Client)(nil)
)

// NewClient constructs a client with safe default retry/timeout settings.
// An empty baseURL falls back to Gemini's OpenAI-compatible endpoint.
func NewClient(baseURL, apiKey string) *Client {
	baseURL = strings.TrimSpace(baseURL)
	apiKey = strings.TrimSpace(apiKey)
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		BaseURL:           strings.TrimRight(baseURL, "/"),
		APIKey:            apiKey,
		HTTPClient:        &http.Client{Timeout: defaultRequestTimeout},
		MaxRetries:        defaultMaxRetries,
		InitialBackoff:    defaultInitialBackoff,
		MaxBackoff:        defaultMaxBackoff,
		BatchSize:         defaultBatchSize,
		GenerationTimeout: defaultGenerationTimeout,
		DefaultEmbedModel: DefaultEmbedModel,
		DefaultChatModel:  DefaultChatModel,
		DefaultSTTModel:   DefaultSTTModel,
		DefaultTTSModel:   DefaultTTSModel,
		DefaultTTSVoice:   DefaultTTSVoice,
	}
}

// Native batchEmbedContents request/response shapes. The native API
// (unlike the OpenAI-compatible /embeddings shim) carries taskType and
// outputDimensionality, and returns embeddings in request order.
type geminiEmbedContentPart struct {
	Text string `json:"text"`
}

type geminiEmbedContent struct {
	Parts []geminiEmbedContentPart `json:"parts"`
}

type geminiEmbedSingleRequest struct {
	Model                string             `json:"model"`
	Content              geminiEmbedContent `json:"content"`
	TaskType             string             `json:"taskType,omitempty"`
	OutputDimensionality *int               `json:"outputDimensionality,omitempty"`
}

type geminiBatchEmbedRequest struct {
	Requests []geminiEmbedSingleRequest `json:"requests"`
}

type geminiBatchEmbedResponse struct {
	Embeddings []struct {
		Values []float64 `json:"values"`
	} `json:"embeddings"`
}

// Embed implements model.Embedder via Gemini's native embed surface
// (models/{model}:batchEmbedContents). The input role maps onto taskType
// (SPEC 8.1.5); a query against the configured code model uses
// CODE_RETRIEVAL_QUERY. When a non-native output dimension is requested
// (SPEC 8.1.6) the returned vectors are L2-normalized. Inputs are sent in
// BatchSize-sized batches, each retried with bounded exponential backoff;
// the native batch response preserves request order.
func (c *Client) Embed(ctx context.Context, modelName string, role model.EmbedRole, inputs []string) ([][]float32, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return nil, &model.ProviderError{Code: "GEMINI_AUTH", Message: "missing Gemini API key", Retryable: false}
	}
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		modelName = strings.TrimSpace(c.DefaultEmbedModel)
		if modelName == "" {
			modelName = DefaultEmbedModel
		}
	}
	if len(inputs) == 0 {
		return [][]float32{}, nil
	}

	isCode := c.CodeEmbedModel != "" && modelName == strings.TrimSpace(c.CodeEmbedModel)
	taskType := geminiTaskType(role, isCode)
	dim := c.EmbedTextDim
	if isCode {
		dim = c.EmbedCodeDim
	}

	batchSize := c.BatchSize
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}

	out := make([][]float32, 0, len(inputs))
	for start := 0; start < len(inputs); start += batchSize {
		end := start + batchSize
		if end > len(inputs) {
			end = len(inputs)
		}
		vectors, err := c.embedBatchWithRetry(ctx, modelName, taskType, dim, inputs[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vectors...)
	}
	return out, nil
}

// geminiTaskType maps the SPEC 8.1.5 input role onto Gemini's taskType.
// Documents always embed as RETRIEVAL_DOCUMENT; a query embeds as
// RETRIEVAL_QUERY, or CODE_RETRIEVAL_QUERY when it targets the code model.
func geminiTaskType(role model.EmbedRole, isCode bool) string {
	if role == model.EmbedQuery {
		if isCode {
			return "CODE_RETRIEVAL_QUERY"
		}
		return "RETRIEVAL_QUERY"
	}
	return "RETRIEVAL_DOCUMENT"
}

func (c *Client) embedBatchWithRetry(ctx context.Context, modelName, taskType string, dim int, inputs []string) ([][]float32, error) {
	maxRetries := c.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if err := c.wait(ctx, c.backoffForAttempt(attempt-1)); err != nil {
				return nil, err
			}
		}
		vectors, err := c.embedBatchNative(ctx, modelName, taskType, dim, inputs)
		if err == nil {
			return vectors, nil
		}
		lastErr = err
		var pErr *model.ProviderError
		if errors.As(err, &pErr) && !pErr.Retryable {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *Client) embedBatchNative(ctx context.Context, modelName, taskType string, dim int, inputs []string) ([][]float32, error) {
	qualified := "models/" + modelName
	reqs := make([]geminiEmbedSingleRequest, len(inputs))
	for i, in := range inputs {
		r := geminiEmbedSingleRequest{
			Model:    qualified,
			Content:  geminiEmbedContent{Parts: []geminiEmbedContentPart{{Text: in}}},
			TaskType: taskType,
		}
		if dim > 0 {
			d := dim
			r.OutputDimensionality = &d
		}
		reqs[i] = r
	}
	body, err := json.Marshal(geminiBatchEmbedRequest{Requests: reqs})
	if err != nil {
		return nil, &model.ProviderError{Code: "GEMINI_FAILED", Message: "failed to marshal embedding request", Retryable: false, Cause: err}
	}

	resp, err := c.doNativeJSON(ctx, "/"+qualified+":batchEmbedContents", body, defaultRequestTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, httpError(resp)
	}

	var parsed geminiBatchEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, &model.ProviderError{Code: "GEMINI_FAILED", Message: "failed to decode embedding response", Retryable: false, StatusCode: resp.StatusCode, Cause: err}
	}
	if len(parsed.Embeddings) != len(inputs) {
		return nil, &model.ProviderError{Code: "GEMINI_FAILED", Message: fmt.Sprintf("embedding response size mismatch: got %d vectors for %d inputs", len(parsed.Embeddings), len(inputs)), Retryable: false, StatusCode: resp.StatusCode}
	}

	vectors := make([][]float32, len(inputs))
	for i, e := range parsed.Embeddings {
		vec := make([]float32, len(e.Values))
		for j, v := range e.Values {
			vec[j] = float32(v)
		}
		if dim > 0 {
			l2Normalize(vec)
		}
		vectors[i] = vec
	}
	return vectors, nil
}

// nativeBaseURL derives Gemini's native API base from the configured
// (OpenAI-compatible) base by dropping a trailing "/openai" segment, so
// embeddings reach models/{model}:batchEmbedContents while chat keeps
// using the OpenAI-compatible base.
func (c *Client) nativeBaseURL() string {
	b := strings.TrimRight(c.BaseURL, "/")
	if trimmed := strings.TrimSuffix(b, "/openai"); trimmed != b {
		return trimmed
	}
	if b == "" {
		return geminiNativeBaseURL
	}
	return b
}

// doNativeJSON POSTs to Gemini's native API, authenticating with the
// x-goog-api-key header so the key never lands in a URL/query string.
func (c *Client) doNativeJSON(ctx context.Context, path string, body []byte, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.nativeBaseURL()+path, bytes.NewReader(body))
	if err != nil {
		return nil, &model.ProviderError{Code: "GEMINI_FAILED", Message: "failed to build request", Retryable: false, Cause: err}
	}
	req.Header.Set("x-goog-api-key", c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := clientWithTimeout(c.HTTPClient, timeout).Do(req)
	if err != nil {
		return nil, &model.ProviderError{Code: "GEMINI_FAILED", Message: "request failed", Retryable: true, Cause: err}
	}
	return resp, nil
}

// l2Normalize scales v to unit L2 length in place. MRL-truncated Gemini
// vectors (outputDimensionality below the native size) are not
// pre-normalized, and the index's cosine/IP scoring assumes unit vectors
// (SPEC 8.1.6). A zero vector is left unchanged.
func l2Normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return
	}
	inv := float32(1.0 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
}

type generateMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type generateRequest struct {
	Model    string            `json:"model"`
	Messages []generateMessage `json:"messages"`
}

type generateResponse struct {
	Choices []struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// Generate implements model.Generator via {base}/chat/completions.
func (c *Client) Generate(ctx context.Context, prompt string) (string, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return "", &model.ProviderError{Code: "GEMINI_AUTH", Message: "missing Gemini API key", Retryable: false}
	}
	if strings.TrimSpace(prompt) == "" {
		return "", &model.ProviderError{Code: "GEMINI_FAILED", Message: "prompt is required", Retryable: false}
	}
	chatModel := strings.TrimSpace(c.DefaultChatModel)
	if chatModel == "" {
		chatModel = DefaultChatModel
	}

	maxRetries := c.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}
	timeout := c.GenerationTimeout
	if timeout <= 0 {
		timeout = defaultGenerationTimeout
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if err := c.wait(ctx, c.backoffForAttempt(attempt-1)); err != nil {
				return "", err
			}
		}
		text, err := c.generateOnce(ctx, chatModel, prompt, timeout)
		if err == nil {
			return text, nil
		}
		lastErr = err
		var pErr *model.ProviderError
		if errors.As(err, &pErr) && !pErr.Retryable {
			return "", err
		}
	}
	return "", lastErr
}

func (c *Client) generateOnce(ctx context.Context, chatModel, prompt string, timeout time.Duration) (string, error) {
	body, err := json.Marshal(generateRequest{
		Model:    chatModel,
		Messages: []generateMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return "", &model.ProviderError{Code: "GEMINI_FAILED", Message: "failed to marshal generation request", Retryable: false, Cause: err}
	}
	resp, err := c.doJSON(ctx, "/chat/completions", body, timeout)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", httpError(resp)
	}

	var parsed generateResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", &model.ProviderError{Code: "GEMINI_FAILED", Message: "failed to decode generation response", Retryable: false, StatusCode: resp.StatusCode, Cause: err}
	}
	if len(parsed.Choices) == 0 {
		return "", &model.ProviderError{Code: "GEMINI_FAILED", Message: "generation response had no choices", Retryable: false, StatusCode: resp.StatusCode}
	}
	text := strings.TrimSpace(contentToText(parsed.Choices[0].Message.Content))
	if text == "" {
		return "", &model.ProviderError{Code: "GEMINI_FAILED", Message: "generation response had empty content", Retryable: false, StatusCode: resp.StatusCode}
	}
	return text, nil
}

// contentToText accepts either a plain string content or the OpenAI
// structured content array ([{"type":"text","text":"..."}]) and returns
// the concatenated text.
func contentToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		// Join text parts with newlines to match the existing Generator
		// adapter (internal/mistral) so multiple parts don't merge into a
		// different user-visible answer (e.g. "firstsecond").
		texts := make([]string, 0, len(parts))
		for _, p := range parts {
			if p.Type == "" || p.Type == "text" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

type transcriptionResponse struct {
	Text string `json:"text"`
}

// Transcribe implements model.Transcriber via {base}/audio/transcriptions
// on Gemini's OpenAI-compatible endpoint (SPEC 8.1.2 gemini stt).
func (c *Client) Transcribe(ctx context.Context, relPath string, data []byte) (string, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return "", &model.ProviderError{Code: "GEMINI_AUTH", Message: "missing Gemini API key", Retryable: false}
	}
	if len(data) == 0 {
		return "", &model.ProviderError{Code: "GEMINI_FAILED", Message: "transcription input is empty", Retryable: false}
	}
	sttModel := strings.TrimSpace(c.DefaultSTTModel)
	if sttModel == "" {
		sttModel = DefaultSTTModel
	}
	timeout := c.GenerationTimeout
	if timeout <= 0 {
		timeout = defaultGenerationTimeout
	}
	return retryAudio(ctx, c, func() (string, error) {
		return c.transcribeOnce(ctx, relPath, data, sttModel, timeout)
	})
}

func (c *Client) transcribeOnce(ctx context.Context, relPath string, data []byte, sttModel string, timeout time.Duration) (string, error) {
	name := strings.TrimSpace(filepath.Base(relPath))
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "audio.wav"
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	ff, err := w.CreateFormFile("file", name)
	if err == nil {
		_, err = ff.Write(data)
	}
	if err == nil {
		err = w.WriteField("model", sttModel)
	}
	if err == nil {
		err = w.Close()
	}
	if err != nil {
		return "", &model.ProviderError{Code: "GEMINI_FAILED", Message: "failed to build transcription body", Retryable: false, Cause: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/audio/transcriptions", bytes.NewReader(buf.Bytes()))
	if err != nil {
		return "", &model.ProviderError{Code: "GEMINI_FAILED", Message: "failed to build transcription request", Retryable: false, Cause: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := clientWithTimeout(c.HTTPClient, timeout).Do(req)
	if err != nil {
		return "", &model.ProviderError{Code: "GEMINI_FAILED", Message: "transcription request failed", Retryable: true, Cause: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", httpError(resp)
	}
	var parsed transcriptionResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", &model.ProviderError{Code: "GEMINI_FAILED", Message: "failed to decode transcription response", Retryable: false, StatusCode: resp.StatusCode, Cause: err}
	}
	return strings.TrimSpace(parsed.Text), nil
}

type speechRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
	Voice string `json:"voice"`
}

// Synthesize implements the optional TTS surface (mcp.TTSSynthesizer
// shape) via {base}/audio/speech, returning raw audio bytes. TTS is
// fail-open per SPEC 8.3.
func (c *Client) Synthesize(ctx context.Context, text string) ([]byte, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return nil, &model.ProviderError{Code: "GEMINI_AUTH", Message: "missing Gemini API key", Retryable: false}
	}
	if strings.TrimSpace(text) == "" {
		return nil, &model.ProviderError{Code: "GEMINI_FAILED", Message: "tts input is empty", Retryable: false}
	}
	ttsModel := strings.TrimSpace(c.DefaultTTSModel)
	if ttsModel == "" {
		ttsModel = DefaultTTSModel
	}
	voice := strings.TrimSpace(c.DefaultTTSVoice)
	if voice == "" {
		voice = DefaultTTSVoice
	}
	timeout := c.GenerationTimeout
	if timeout <= 0 {
		timeout = defaultGenerationTimeout
	}
	body, err := json.Marshal(speechRequest{Model: ttsModel, Input: text, Voice: voice})
	if err != nil {
		return nil, &model.ProviderError{Code: "GEMINI_FAILED", Message: "failed to marshal tts request", Retryable: false, Cause: err}
	}
	return retryAudio(ctx, c, func() ([]byte, error) {
		resp, err := c.doJSON(ctx, "/audio/speech", body, timeout)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, httpError(resp)
		}
		audio, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			return nil, &model.ProviderError{Code: "GEMINI_FAILED", Message: "failed to read tts audio", Retryable: true, Cause: rerr}
		}
		if len(audio) == 0 {
			return nil, &model.ProviderError{Code: "GEMINI_FAILED", Message: "tts returned no audio", Retryable: false, StatusCode: resp.StatusCode}
		}
		return audio, nil
	})
}

// retryAudio runs op with the client's bounded exponential backoff,
// stopping early on a non-retryable *model.ProviderError.
func retryAudio[T any](ctx context.Context, c *Client, op func() (T, error)) (T, error) {
	maxRetries := c.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}
	var zero T
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if err := c.wait(ctx, c.backoffForAttempt(attempt-1)); err != nil {
				return zero, err
			}
		}
		out, err := op()
		if err == nil {
			return out, nil
		}
		lastErr = err
		var pErr *model.ProviderError
		if errors.As(err, &pErr) && !pErr.Retryable {
			return zero, err
		}
	}
	return zero, lastErr
}

func (c *Client) doJSON(ctx context.Context, path string, body []byte, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, &model.ProviderError{Code: "GEMINI_FAILED", Message: "failed to build request", Retryable: false, Cause: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := clientWithTimeout(c.HTTPClient, timeout).Do(req)
	if err != nil {
		return nil, &model.ProviderError{Code: "GEMINI_FAILED", Message: "request failed", Retryable: true, Cause: err}
	}
	return resp, nil
}

// clientWithTimeout returns an *http.Client that uses the per-call
// timeout. The default client built by NewClient carries the short
// (30s) request timeout, so chat completions — which use the longer
// GenerationTimeout — must override it even when HTTPClient is set
// (mirrors internal/mistral). The base client's Transport is shared via
// a shallow copy so connection pooling is preserved.
func clientWithTimeout(base *http.Client, timeout time.Duration) *http.Client {
	if base == nil {
		return &http.Client{Timeout: timeout}
	}
	cp := *base
	cp.Timeout = timeout
	return &cp
}

func httpError(resp *http.Response) error {
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(bodyBytes))
	if msg == "" {
		msg = "upstream returned non-200 response"
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &model.ProviderError{Code: "GEMINI_AUTH", Message: msg, Retryable: false, StatusCode: resp.StatusCode}
	case resp.StatusCode == http.StatusTooManyRequests:
		return &model.ProviderError{Code: "GEMINI_RATE_LIMIT", Message: msg, Retryable: true, StatusCode: resp.StatusCode}
	case resp.StatusCode >= http.StatusInternalServerError:
		return &model.ProviderError{Code: "GEMINI_FAILED", Message: msg, Retryable: true, StatusCode: resp.StatusCode}
	default:
		return &model.ProviderError{Code: "GEMINI_FAILED", Message: msg, Retryable: false, StatusCode: resp.StatusCode}
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

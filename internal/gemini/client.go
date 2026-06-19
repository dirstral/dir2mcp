// Package gemini provides a direct-HTTP adapter for Google Gemini. Only
// chat (model.Generator) uses Gemini's OpenAI-compatible surface (POST
// {base}/chat/completions); embeddings (model.Embedder) and audio
// (model.Transcriber / TTS) use Gemini's NATIVE surface
// ({base-without-/openai}/models/{model}:{batchEmbedContents,generateContent})
// because the OpenAI-compatible layer exposes neither `taskType`/
// `outputDimensionality` (SPEC 8.1.5/8.1.6) nor `/v1/audio/*` (SPEC 8.2/8.3).
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
// STT (model.Transcriber) and TTS use the native
// models/{model}:generateContent surface too (SPEC 8.2/8.3): STT sends the
// audio as an inline-data part; TTS requests responseModalities:[AUDIO]
// with a speechConfig voice and WAV-wraps the returned PCM. Gemini's
// OpenAI-compatible layer exposes no /v1/audio/*, so only chat rides it.
package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/usage"
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

	// geminiTTSSampleRate is the sample rate of Gemini's TTS PCM output
	// (signed 16-bit little-endian, mono); used to build the WAV header
	// when the response mimeType omits an explicit rate.
	geminiTTSSampleRate = 24000
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
	// DefaultSTTLanguage is an optional language hint included in the
	// transcription prompt (SPEC 8.2 stt_language); empty omits it.
	DefaultSTTLanguage string
	DefaultTTSModel    string
	DefaultTTSVoice    string
}

// compile-time assertions that *Client implements the model contracts.
var (
	_ model.Embedder           = (*Client)(nil)
	_ model.MultimodalEmbedder = (*Client)(nil)
	_ model.Generator          = (*Client)(nil)
	_ model.Transcriber        = (*Client)(nil)
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
type geminiEmbedSingleRequest struct {
	Model                string        `json:"model"`
	Content              geminiContent `json:"content"`
	TaskType             string        `json:"taskType,omitempty"`
	OutputDimensionality *int          `json:"outputDimensionality,omitempty"`
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
	modelName, dim, taskType, err := c.embedParams(modelName, role)
	if err != nil {
		return nil, err
	}
	if len(inputs) == 0 {
		return [][]float32{}, nil
	}
	reqs := make([]geminiEmbedSingleRequest, len(inputs))
	for i, in := range inputs {
		reqs[i] = c.embedReq(modelName, taskType, dim, geminiPart{Text: in})
	}
	return c.embedBatched(ctx, modelName, dim, reqs)
}

// EmbedMedia embeds non-text media (images, audio, video, PDFs) via the
// same native batchEmbedContents surface, sending each item as an
// inline-data part (SPEC 8.1.7). Role→taskType and dimension/normalization
// behave as for Embed.
func (c *Client) EmbedMedia(ctx context.Context, modelName string, role model.EmbedRole, items []model.MediaInput) ([][]float32, error) {
	modelName, dim, taskType, err := c.embedParams(modelName, role)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return [][]float32{}, nil
	}
	reqs := make([]geminiEmbedSingleRequest, len(items))
	for i, it := range items {
		if len(it.Data) == 0 {
			return nil, &model.ProviderError{Code: "GEMINI_FAILED", Message: "media input is empty", Retryable: false}
		}
		part := geminiPart{InlineData: &geminiInlineData{MimeType: it.MimeType, Data: base64.StdEncoding.EncodeToString(it.Data)}}
		reqs[i] = c.embedReq(modelName, taskType, dim, part)
	}
	return c.embedBatched(ctx, modelName, dim, reqs)
}

// embedParams resolves the effective model name, requested output dimension,
// and taskType for an embed call (shared by Embed/EmbedMedia).
func (c *Client) embedParams(modelName string, role model.EmbedRole) (string, int, string, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return "", 0, "", &model.ProviderError{Code: "GEMINI_AUTH", Message: "missing Gemini API key", Retryable: false}
	}
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		modelName = strings.TrimSpace(c.DefaultEmbedModel)
		if modelName == "" {
			modelName = DefaultEmbedModel
		}
	}
	isCode := c.CodeEmbedModel != "" && modelName == strings.TrimSpace(c.CodeEmbedModel)
	dim := c.EmbedTextDim
	if isCode {
		dim = c.EmbedCodeDim
	}
	return modelName, dim, geminiTaskType(role, isCode), nil
}

// embedReq builds one native embed request for a single content part.
func (c *Client) embedReq(modelName, taskType string, dim int, part geminiPart) geminiEmbedSingleRequest {
	r := geminiEmbedSingleRequest{
		Model:    "models/" + modelName,
		Content:  geminiContent{Parts: []geminiPart{part}},
		TaskType: taskType,
	}
	if dim > 0 {
		d := dim
		r.OutputDimensionality = &d
	}
	return r
}

// embedBatched sends reqs in BatchSize-sized batches (each retried) and
// concatenates the vectors in request order.
func (c *Client) embedBatched(ctx context.Context, modelName string, dim int, reqs []geminiEmbedSingleRequest) ([][]float32, error) {
	batchSize := c.BatchSize
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	out := make([][]float32, 0, len(reqs))
	for start := 0; start < len(reqs); start += batchSize {
		end := start + batchSize
		if end > len(reqs) {
			end = len(reqs)
		}
		vectors, err := c.embedReqsWithRetry(ctx, modelName, dim, reqs[start:end])
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

func (c *Client) embedReqsWithRetry(ctx context.Context, modelName string, dim int, reqs []geminiEmbedSingleRequest) ([][]float32, error) {
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
		vectors, err := c.embedReqsNative(ctx, modelName, dim, reqs)
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

func (c *Client) embedReqsNative(ctx context.Context, modelName string, dim int, reqs []geminiEmbedSingleRequest) ([][]float32, error) {
	body, err := json.Marshal(geminiBatchEmbedRequest{Requests: reqs})
	if err != nil {
		return nil, &model.ProviderError{Code: "GEMINI_FAILED", Message: "failed to marshal embedding request", Retryable: false, Cause: err}
	}
	resp, err := c.doNativeJSON(ctx, "/models/"+modelName+":batchEmbedContents", body, defaultRequestTimeout)
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
	if len(parsed.Embeddings) != len(reqs) {
		return nil, &model.ProviderError{Code: "GEMINI_FAILED", Message: fmt.Sprintf("embedding response size mismatch: got %d vectors for %d inputs", len(parsed.Embeddings), len(reqs)), Retryable: false, StatusCode: resp.StatusCode}
	}

	vectors := make([][]float32, len(reqs))
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
	Usage usage.OpenAIUsage `json:"usage"`
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
	if !parsed.Usage.Empty() {
		usage.Report(ctx, usage.StageGenerate, parsed.Usage.ToUsage())
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

// Native generateContent request/response shapes, shared by STT and TTS.
// Gemini's OpenAI-compatible layer exposes no /v1/audio/*, so audio uses
// models/{model}:generateContent (SPEC 8.2/8.3).
type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // base64
}

type geminiPart struct {
	Text       string            `json:"text,omitempty"`
	InlineData *geminiInlineData `json:"inlineData,omitempty"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPrebuiltVoice struct {
	VoiceName string `json:"voiceName"`
}

type geminiVoiceConfig struct {
	PrebuiltVoiceConfig geminiPrebuiltVoice `json:"prebuiltVoiceConfig"`
}

type geminiSpeechConfig struct {
	VoiceConfig geminiVoiceConfig `json:"voiceConfig"`
}

type geminiGenerationConfig struct {
	ResponseModalities []string            `json:"responseModalities,omitempty"`
	SpeechConfig       *geminiSpeechConfig `json:"speechConfig,omitempty"`
}

type geminiGenerateRequest struct {
	Contents         []geminiContent         `json:"contents"`
	GenerationConfig *geminiGenerationConfig `json:"generationConfig,omitempty"`
}

type geminiGenerateResponse struct {
	Candidates []struct {
		Content geminiContent `json:"content"`
	} `json:"candidates"`
}

// Transcribe implements model.Transcriber via the native
// models/{model}:generateContent surface: the audio is sent as an inline
// data part with a transcription instruction, and the model's text output
// is the transcript (SPEC 8.2). Gemini's OpenAI-compatible layer has no
// /v1/audio/transcriptions.
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
	prompt := "Generate a verbatim transcript of the speech in this audio. Return only the transcript text, with no commentary, labels, or timestamps."
	if lang := strings.TrimSpace(c.DefaultSTTLanguage); lang != "" {
		prompt += " The audio language is " + lang + "."
	}
	body, err := json.Marshal(geminiGenerateRequest{
		Contents: []geminiContent{{Parts: []geminiPart{
			{Text: prompt},
			{InlineData: &geminiInlineData{MimeType: audioMIMEType(relPath), Data: base64.StdEncoding.EncodeToString(data)}},
		}}},
	})
	if err != nil {
		return "", &model.ProviderError{Code: "GEMINI_FAILED", Message: "failed to marshal transcription request", Retryable: false, Cause: err}
	}
	resp, err := c.doNativeJSON(ctx, "/models/"+sttModel+":generateContent", body, timeout)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", httpError(resp)
	}
	var parsed geminiGenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", &model.ProviderError{Code: "GEMINI_FAILED", Message: "failed to decode transcription response", Retryable: false, StatusCode: resp.StatusCode, Cause: err}
	}
	text := strings.TrimSpace(firstCandidateText(parsed))
	if text == "" {
		return "", &model.ProviderError{Code: "GEMINI_FAILED", Message: "transcription response had no text", Retryable: false, StatusCode: resp.StatusCode}
	}
	return text, nil
}

// firstCandidateText concatenates the text parts of the first candidate.
func firstCandidateText(r geminiGenerateResponse) string {
	if len(r.Candidates) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range r.Candidates[0].Content.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// Synthesize implements the optional TTS surface (mcp.TTSSynthesizer
// shape) via the native models/{model}:generateContent surface with
// responseModalities:[AUDIO] + a speechConfig voice (SPEC 8.3). Gemini
// returns raw PCM (s16le, 24kHz, mono); it is WAV-wrapped so the bytes are
// directly playable, matching the other TTS adapters. TTS is fail-open
// per SPEC 8.3.
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
	body, err := json.Marshal(geminiGenerateRequest{
		Contents: []geminiContent{{Parts: []geminiPart{{Text: text}}}},
		GenerationConfig: &geminiGenerationConfig{
			ResponseModalities: []string{"AUDIO"},
			SpeechConfig:       &geminiSpeechConfig{VoiceConfig: geminiVoiceConfig{PrebuiltVoiceConfig: geminiPrebuiltVoice{VoiceName: voice}}},
		},
	})
	if err != nil {
		return nil, &model.ProviderError{Code: "GEMINI_FAILED", Message: "failed to marshal tts request", Retryable: false, Cause: err}
	}
	return retryAudio(ctx, c, func() ([]byte, error) {
		resp, err := c.doNativeJSON(ctx, "/models/"+ttsModel+":generateContent", body, timeout)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, httpError(resp)
		}
		var parsed geminiGenerateResponse
		if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
			return nil, &model.ProviderError{Code: "GEMINI_FAILED", Message: "failed to decode tts response", Retryable: false, StatusCode: resp.StatusCode, Cause: err}
		}
		pcm, rate, perr := firstAudioPart(parsed)
		if perr != nil {
			return nil, perr
		}
		return wavWrapPCM16(pcm, rate), nil
	})
}

// firstAudioPart returns the decoded PCM bytes and sample rate from the
// first inline-audio part of the first candidate. The sample rate is
// parsed from the part's mimeType (e.g. "audio/L16;rate=24000"),
// defaulting to geminiTTSSampleRate.
func firstAudioPart(r geminiGenerateResponse) ([]byte, int, error) {
	if len(r.Candidates) == 0 {
		return nil, 0, &model.ProviderError{Code: "GEMINI_FAILED", Message: "tts response had no candidates", Retryable: false}
	}
	for _, p := range r.Candidates[0].Content.Parts {
		if p.InlineData == nil || p.InlineData.Data == "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(p.InlineData.Data)
		if err != nil {
			return nil, 0, &model.ProviderError{Code: "GEMINI_FAILED", Message: "failed to decode tts audio", Retryable: false, Cause: err}
		}
		rate, err := sampleRateFromMIME(p.InlineData.MimeType)
		if err != nil {
			return nil, 0, &model.ProviderError{Code: "GEMINI_FAILED", Message: err.Error(), Retryable: false}
		}
		return raw, rate, nil
	}
	return nil, 0, &model.ProviderError{Code: "GEMINI_FAILED", Message: "tts response had no audio", Retryable: false}
}

// audioMIMEType maps a file extension to a Gemini-accepted audio MIME
// type, defaulting to audio/wav for unknown/empty extensions.
func audioMIMEType(relPath string) string {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(relPath), ".")) {
	case "mp3":
		return "audio/mp3"
	case "aiff", "aif":
		return "audio/aiff"
	case "aac":
		return "audio/aac"
	case "ogg", "oga":
		return "audio/ogg"
	case "flac":
		return "audio/flac"
	case "m4a", "mp4":
		return "audio/mp4"
	default:
		return "audio/wav"
	}
}

// sampleRateFromMIME extracts the rate from Gemini's PCM mime type (e.g.
// "audio/L16;rate=24000"). wavWrapPCM16 only produces a correct file for
// signed-16-bit little-endian PCM, so a non-L16 type or a malformed rate
// is rejected (returning a structured error) rather than silently wrapped
// into a "valid" WAV with corrupted audio. An empty mime type falls back
// to the documented default rate.
func sampleRateFromMIME(mime string) (int, error) {
	m := strings.TrimSpace(strings.ToLower(mime))
	if m == "" {
		return geminiTTSSampleRate, nil
	}
	if !strings.HasPrefix(m, "audio/l16") {
		return 0, fmt.Errorf("unsupported tts audio mime type %q (expected audio/L16 PCM)", mime)
	}
	for _, seg := range strings.Split(m, ";") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(seg), "rate="); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("invalid sample rate in tts audio mime type %q", mime)
			}
			return n, nil
		}
	}
	return geminiTTSSampleRate, nil
}

// wavWrapPCM16 wraps raw signed-16-bit little-endian mono PCM in a minimal
// RIFF/WAVE container so the bytes are directly playable (SPEC 8.3) —
// Gemini returns headerless PCM, while every other TTS adapter returns a
// self-describing container.
func wavWrapPCM16(pcm []byte, sampleRate int) []byte {
	if sampleRate <= 0 {
		sampleRate = geminiTTSSampleRate
	}
	const numChannels, bitsPerSample = 1, 16
	byteRate := sampleRate * numChannels * bitsPerSample / 8
	blockAlign := numChannels * bitsPerSample / 8
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(36+len(pcm)))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16)) // PCM fmt chunk size
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))  // audio format = PCM
	_ = binary.Write(&buf, binary.LittleEndian, uint16(numChannels))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(byteRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(blockAlign))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(bitsPerSample))
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(pcm)))
	buf.Write(pcm)
	return buf.Bytes()
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

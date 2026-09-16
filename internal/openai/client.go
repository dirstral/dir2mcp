// Package openai provides a direct-HTTP adapter for the OpenAI-compatible
// API surface (POST {base}/embeddings, POST {base}/chat/completions). It
// is the provider-agnostic "backbone" (SPEC 8.1.1): the same wire shape
// is served by OpenAI, OpenRouter, Groq, Together, Azure-style gateways,
// local Ollama/vLLM/LM Studio, and Mistral chat/embeddings — selected
// purely by BaseURL + model names.
//
// It mirrors internal/mistral and internal/cohere (BaseURL/APIKey/
// HTTPClient, bounded exponential retry, typed model.ProviderError) so
// callers can depend on model.Embedder / model.Generator without taking
// a hard dependency on this package.
//
// A custom (non-default) base_url is treated as a self-hosted / local
// OpenAI-compatible endpoint (Ollama/vLLM/LM Studio, SPEC §8.5) and MAY be
// credential-less: when no api_key is configured against such a base_url the
// client does NOT fail with OPENAI_AUTH and sends no Bearer header, mirroring
// the credential-optional whisper/omniembed/colbert self-hosted clients. A
// missing key against the public OpenAI cloud endpoint (the default base_url)
// is still a hard OPENAI_AUTH error.
//
// OpenAI embeddings are symmetric, so the model.EmbedRole is accepted
// and intentionally ignored (SPEC 8.1.5) — observable behavior MUST NOT
// differ by role for this provider.
package openai

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
	"sync/atomic"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/providerhttp"
	"github.com/dirstral/dir2mcp/internal/usage"
)

const (
	defaultBaseURL           = "https://api.openai.com/v1"
	defaultRequestTimeout    = 30 * time.Second
	defaultGenerationTimeout = 120 * time.Second
	// defaultGenerationMaxTokens bounds a single /chat/completions
	// completion. Without a cap a misbehaving model (common with small,
	// self-hosted/CPU chat backends) can generate far past the requested
	// output — e.g. a subtitle-line translation that never stops — until the
	// generation timeout fires and the whole file fails with OPENAI_FAILED
	// (issue #500). A finite cap turns that unbounded runaway into a bounded
	// request. It matches the anthropic sibling's 4096 so it is generous enough
	// for RAG answer synthesis and annotate JSON (a tighter cap silently
	// truncated long answers and could corrupt annotate JSON → ANNOTATE_FAILED);
	// short-output callers pass a per-call cap via GenerateWithMaxTokens instead
	// of lowering this. Operators on very slow backends can lower it via
	// Client.GenerationMaxTokens.
	defaultGenerationMaxTokens = 4096
	defaultMaxRetries          = 3
	defaultInitialBackoff      = 250 * time.Millisecond
	defaultMaxBackoff          = 2 * time.Second
	defaultBatchSize           = 64

	// DefaultEmbedModel / DefaultChatModel / DefaultSTTModel /
	// DefaultTTSModel / DefaultTTSVoice are fallbacks used only when the
	// caller passes an empty value. The config resolver normally
	// supplies explicit models per profile.
	DefaultEmbedModel = "text-embedding-3-small"
	DefaultChatModel  = "gpt-4o-mini"
	DefaultSTTModel   = "whisper-1"
	DefaultTTSModel   = "tts-1"
	DefaultTTSVoice   = "alloy"
)

// Client is an OpenAI-compatible API adapter.
//
// Error codes (carried on *model.ProviderError):
//   - OPENAI_AUTH (non-retryable): missing key, 401/403
//   - OPENAI_RATE_LIMIT (retryable): 429
//   - OPENAI_FAILED (retryable for network/5xx, else non-retryable)
type Client struct {
	BaseURL           string
	APIKey            string
	HTTPClient        *http.Client
	MaxRetries        int
	InitialBackoff    time.Duration
	MaxBackoff        time.Duration
	BatchSize         int
	GenerationTimeout time.Duration
	// GenerationMaxTokens caps a single chat completion (max_tokens). <= 0
	// falls back to defaultGenerationMaxTokens; it is never sent unbounded.
	// See issue #500.
	GenerationMaxTokens int

	// capName remembers which spelling of the completion cap this endpoint
	// accepts (issue #958). Zero value is capModern, so a fresh client tries the
	// current OpenAI parameter first and only falls back when a server refuses
	// it by name. Atomic: one Client serves concurrent embed/generate workers.
	capName atomic.Int32
	// temperatureRefused remembers that this endpoint rejects the temperature
	// parameter by name (some hosted reasoning models do). A fresh client sends
	// temperature 0 on every generation; after one refusal it drops the field
	// for the rest of the client's life, so determinism is kept wherever it IS
	// supported and one probe is spent per client rather than one per request.
	// Bound to the parameter the same way the cap fallback is (#959): a 400 that
	// merely mentions the word does not flip it.
	temperatureRefused atomic.Bool
	// DefaultEmbedModel/DefaultChatModel/DefaultSTTModel/DefaultTTSModel/
	// DefaultTTSVoice are used when the corresponding call is made with
	// an empty value.
	DefaultEmbedModel string
	DefaultChatModel  string
	DefaultSTTModel   string
	DefaultTTSModel   string
	DefaultTTSVoice   string
}

// compile-time assertions that *Client implements the model contracts.
var (
	_ model.Embedder         = (*Client)(nil)
	_ model.Generator        = (*Client)(nil)
	_ model.BoundedGenerator = (*Client)(nil)
	_ model.Transcriber      = (*Client)(nil)
)

// NewClient constructs a client with safe default retry/timeout settings.
// An empty baseURL falls back to the public OpenAI endpoint; point it at
// any OpenAI-compatible base (OpenRouter, Groq, Azure, local) to reuse
// this adapter.
func NewClient(baseURL, apiKey string) *Client {
	baseURL = strings.TrimSpace(baseURL)
	apiKey = strings.TrimSpace(apiKey)
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		BaseURL:             strings.TrimRight(baseURL, "/"),
		APIKey:              apiKey,
		HTTPClient:          providerhttp.NewClient(defaultRequestTimeout),
		MaxRetries:          defaultMaxRetries,
		InitialBackoff:      defaultInitialBackoff,
		MaxBackoff:          defaultMaxBackoff,
		BatchSize:           defaultBatchSize,
		GenerationTimeout:   defaultGenerationTimeout,
		GenerationMaxTokens: defaultGenerationMaxTokens,
		DefaultEmbedModel:   DefaultEmbedModel,
		DefaultChatModel:    DefaultChatModel,
		DefaultSTTModel:     DefaultSTTModel,
		DefaultTTSModel:     DefaultTTSModel,
		DefaultTTSVoice:     DefaultTTSVoice,
	}
}

// missingRequiredKey reports whether an empty api_key is a hard error for
// this client. A missing key is only fatal against the public OpenAI cloud
// endpoint (the default base_url, or an unset base that falls back to it); a
// custom base_url points at a self-hosted / local OpenAI-compatible endpoint
// on a trusted network, which is credential-less by design (SPEC §8.5) and is
// called with no Bearer header. When this returns false the caller proceeds
// credential-less rather than returning OPENAI_AUTH.
func (c *Client) missingRequiredKey() bool {
	if strings.TrimSpace(c.APIKey) != "" {
		return false
	}
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	return base == "" || base == defaultBaseURL
}

// setAuthHeader sets the Bearer Authorization header only when an api_key is
// configured, so a credential-less local endpoint receives no auth header
// (mirrors the whisper/omniembed/colbert self-hosted clients).
func (c *Client) setAuthHeader(req *http.Request) {
	if key := strings.TrimSpace(c.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Usage usage.OpenAIUsage `json:"usage"`
}

// Embed implements model.Embedder. OpenAI embeddings are symmetric, so
// the input role is accepted and ignored (SPEC 8.1.5). Inputs are sent
// in BatchSize-sized batches; each batch is retried with bounded
// exponential backoff, and vectors are reordered to match input order.
func (c *Client) Embed(ctx context.Context, modelName string, _ model.EmbedRole, inputs []string) ([][]float32, error) {
	if c.missingRequiredKey() {
		return nil, &model.ProviderError{Code: "OPENAI_AUTH", Message: "missing OpenAI API key", Retryable: false}
	}
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		modelName = c.DefaultEmbedModel
		if modelName == "" {
			modelName = DefaultEmbedModel
		}
	}
	if len(inputs) == 0 {
		return [][]float32{}, nil
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
		vectors, err := c.embedBatchWithRetry(ctx, modelName, inputs[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vectors...)
	}
	// Reject empty / non-finite / zero-norm vectors at the provider boundary
	// (issue #703) so they never reach an index.
	if err := model.ValidateEmbedVectors("OPENAI_FAILED", 0, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) embedBatchWithRetry(ctx context.Context, modelName string, inputs []string) ([][]float32, error) {
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
		vectors, err := c.embedBatch(ctx, modelName, inputs)
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

func (c *Client) embedBatch(ctx context.Context, modelName string, inputs []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Model: modelName, Input: inputs})
	if err != nil {
		return nil, &model.ProviderError{Code: "OPENAI_FAILED", Message: "failed to marshal embedding request", Retryable: false, Cause: err}
	}
	resp, err := c.doJSON(ctx, "/embeddings", body, defaultRequestTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, httpError(resp)
	}

	raw, err := providerhttp.ReadLimitedJSONBody(resp, "OPENAI_FAILED")
	if err != nil {
		return nil, err
	}
	var parsed embedResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, &model.ProviderError{Code: "OPENAI_FAILED", Message: "failed to decode embedding response", Retryable: false, StatusCode: resp.StatusCode, Cause: err}
	}
	if !parsed.Usage.Empty() {
		usage.Report(ctx, usage.StageEmbed, parsed.Usage.ToUsage())
	}
	if len(parsed.Data) != len(inputs) {
		return nil, &model.ProviderError{Code: "OPENAI_FAILED", Message: fmt.Sprintf("embedding response size mismatch: got %d vectors for %d inputs", len(parsed.Data), len(inputs)), Retryable: false, StatusCode: resp.StatusCode}
	}

	vectors := make([][]float32, len(inputs))
	seen := make([]bool, len(inputs))
	for _, item := range parsed.Data {
		if item.Index < 0 || item.Index >= len(inputs) {
			return nil, &model.ProviderError{Code: "OPENAI_FAILED", Message: fmt.Sprintf("embedding response contains out-of-range index %d", item.Index), Retryable: false, StatusCode: resp.StatusCode}
		}
		if seen[item.Index] {
			return nil, &model.ProviderError{Code: "OPENAI_FAILED", Message: fmt.Sprintf("embedding response contains duplicate index %d", item.Index), Retryable: false, StatusCode: resp.StatusCode}
		}
		vec := make([]float32, len(item.Embedding))
		for i, v := range item.Embedding {
			vec[i] = float32(v)
		}
		vectors[item.Index] = vec
		seen[item.Index] = true
	}
	for i := range seen {
		if !seen[i] {
			return nil, &model.ProviderError{Code: "OPENAI_FAILED", Message: fmt.Sprintf("embedding response missing index %d", i), Retryable: false, StatusCode: resp.StatusCode}
		}
	}
	return vectors, nil
}

type generateMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// generateRequest carries the completion cap under ONE of two names, because
// the two spellings do not overlap across the model generations (issue #958):
//
//   - `max_completion_tokens` is the current OpenAI spelling and the only one
//     GPT-5-era models accept. Measured 2026-09-11: gpt-5.6-sol, gpt-5.5 and
//     gpt-5.4-mini all answer `400 Unsupported parameter: 'max_tokens' is not
//     supported with this model` and accept `max_completion_tokens`.
//   - `max_tokens` is the legacy spelling. gpt-4o and gpt-4o-mini accept both,
//     but an OpenAI-COMPATIBLE server (this client also drives Mistral,
//     OpenRouter, Ollama and self-hosted llama.cpp/vLLM under `kind: openai`)
//     may know only this one.
//
// So the cap is sent under the modern name first and the legacy name is the
// fallback, chosen per request by generateOnce. Exactly one field is emitted:
// sending both is itself a 400 on some servers. The value is always > 0 after
// clamping (see generate), so the emitted field never carries a zero and every
// completion stays bounded (issue #500).
type generateRequest struct {
	Model               string            `json:"model"`
	Messages            []generateMessage `json:"messages"`
	MaxCompletionTokens int               `json:"max_completion_tokens,omitempty"`
	MaxTokens           int               `json:"max_tokens,omitempty"`
	// Temperature is pinned to 0 rather than left to the provider default so a
	// derived representation is REPRODUCIBLE: the same transcript translated
	// twice yields the same text. Left to the server, a local OpenAI-compatible
	// endpoint samples at its own default (ollama: 0.8), and on the pilot corpus
	// two runs of an unmodified archive disagreed on 96.7% of subtitle cues,
	// which makes a delivery impossible to reproduce and swamps any A/B
	// comparison in sampling noise. A pointer with omitempty because some hosted
	// reasoning models refuse the parameter outright; see temperatureRefused.
	Temperature *float64 `json:"temperature,omitempty"`
}

type generateResponse struct {
	Choices []struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage usage.OpenAIUsage `json:"usage"`
}

// Generate implements model.Generator via {base}/chat/completions. It uses
// the client's generous default cap (GenerationMaxTokens, else
// defaultGenerationMaxTokens) so answer synthesis and annotation are not
// truncated.
func (c *Client) Generate(ctx context.Context, prompt string) (string, error) {
	return c.generate(ctx, prompt, 0)
}

// GenerateWithMaxTokens is like Generate but caps this single completion at
// maxTokens (max_tokens). A maxTokens <= 0 falls back to the client default,
// so it is safe to pass 0. It lets a caller with a known-short output (e.g. a
// single translated transcript line) request a tight bound WITHOUT lowering the
// generous default that ask/annotate rely on. It satisfies model.BoundedGenerator.
func (c *Client) GenerateWithMaxTokens(ctx context.Context, prompt string, maxTokens int) (string, error) {
	return c.generate(ctx, prompt, maxTokens)
}

func (c *Client) generate(ctx context.Context, prompt string, maxTokensOverride int) (string, error) {
	if c.missingRequiredKey() {
		return "", &model.ProviderError{Code: "OPENAI_AUTH", Message: "missing OpenAI API key", Retryable: false}
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
	// Per-call override wins when positive; otherwise fall back to the client
	// default, then the package default. The value sent is always > 0.
	maxTokens := maxTokensOverride
	if maxTokens <= 0 {
		maxTokens = c.GenerationMaxTokens
	}
	if maxTokens <= 0 {
		maxTokens = defaultGenerationMaxTokens
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if err := c.wait(ctx, c.backoffForAttempt(attempt-1)); err != nil {
				return "", err
			}
		}
		text, err := c.generateOnce(ctx, chatModel, prompt, maxTokens, timeout)
		text, err = c.retryRefusedParams(ctx, chatModel, prompt, maxTokens, timeout, text, err)
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

// retryRefusedParams turns a 400 that refuses one of the client's own request
// parameters BY NAME into one retry without it, remembering the refusal for
// the client's life so each probe is spent once per client, not once per
// request. Two parameters can be refused: the modern completion-cap spelling
// (an OpenAI-compatible server that predates the rename, #958) and the pinned
// temperature (some hosted reasoning models). A server reports one refusal per
// response, in whichever order it validates, so the loop keeps applying
// fallbacks while the current error names a parameter whose fallback is still
// untried; an endpoint that refuses both gets an answer after two probes
// whichever it rejects first. Each fallback runs at most once per call, so the
// loop always ends. Any other error, and the last retry's own error, pass
// through unchanged into the caller's retryability check.
func (c *Client) retryRefusedParams(ctx context.Context, chatModel, prompt string, maxTokens int, timeout time.Duration, text string, err error) (string, error) {
	triedCap, triedTemperature := false, false
	for err != nil {
		switch {
		case !triedCap && unsupportedCapParam(err, capModern):
			triedCap = true
			c.capName.Store(int32(capLegacy))
			text, err = c.generateOnceWithCapName(ctx, chatModel, prompt, maxTokens, timeout, capLegacy)
		case !triedTemperature && providerhttp.RefusesParam(err, "temperature"):
			// Decided on THIS request's error, not on the flag: a concurrent worker
			// can record the refusal between this request's send and this check, and
			// a flag-guarded branch would then skip the retry and surface the
			// rejection for a request that did carry the parameter. The retry reads
			// the flag when it builds its body, so it goes out without temperature
			// whoever stored the refusal first.
			triedTemperature = true
			c.temperatureRefused.Store(true)
			text, err = c.generateOnce(ctx, chatModel, prompt, maxTokens, timeout)
		default:
			return text, err
		}
	}
	return text, err
}

func (c *Client) generateOnce(ctx context.Context, chatModel, prompt string, maxTokens int, timeout time.Duration) (string, error) {
	return c.generateOnceWithCapName(ctx, chatModel, prompt, maxTokens, timeout, c.completionCap())
}

// completionCap is the spelling this client uses now: the modern one until a
// server refuses it, then the legacy one for the rest of the client's life. It
// is atomic because one Client is shared by concurrent workers.
func (c *Client) completionCap() completionCapName {
	return completionCapName(c.capName.Load())
}

// completionCapName selects which spelling of the completion cap goes on the
// wire. See generateRequest for why there are two.
type completionCapName int

const (
	capModern completionCapName = iota // max_completion_tokens
	capLegacy                          // max_tokens
)

// unsupportedCapParam reports whether err is the provider refusing the cap
// parameter ITSELF, which is the one error worth retrying under the other
// spelling (#958, #959). The binding rule lives in providerhttp.RefusesParam and
// is shared with the temperature fallback and the other chat adapters.
func unsupportedCapParam(err error, sent completionCapName) bool {
	name := "max_completion_tokens"
	if sent == capLegacy {
		name = "max_tokens"
	}
	return providerhttp.RefusesParam(err, name)
}

func (c *Client) generateOnceWithCapName(ctx context.Context, chatModel, prompt string, maxTokens int, timeout time.Duration, cap completionCapName) (string, error) {
	req := generateRequest{
		Model:    chatModel,
		Messages: []generateMessage{{Role: "user", Content: prompt}},
	}
	if cap == capLegacy {
		req.MaxTokens = maxTokens
	} else {
		req.MaxCompletionTokens = maxTokens
	}
	if !c.temperatureRefused.Load() {
		zero := 0.0
		req.Temperature = &zero
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", &model.ProviderError{Code: "OPENAI_FAILED", Message: "failed to marshal generation request", Retryable: false, Cause: err}
	}
	resp, err := c.doJSON(ctx, "/chat/completions", body, timeout)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", httpError(resp)
	}

	raw, err := providerhttp.ReadLimitedJSONBody(resp, "OPENAI_FAILED")
	if err != nil {
		return "", err
	}
	var parsed generateResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", &model.ProviderError{Code: "OPENAI_FAILED", Message: "failed to decode generation response", Retryable: false, StatusCode: resp.StatusCode, Cause: err}
	}
	if !parsed.Usage.Empty() {
		usage.Report(ctx, usage.StageGenerate, parsed.Usage.ToUsage())
	}
	if len(parsed.Choices) == 0 {
		return "", &model.ProviderError{Code: "OPENAI_FAILED", Message: "generation response had no choices", Retryable: false, StatusCode: resp.StatusCode}
	}
	text := strings.TrimSpace(contentToText(parsed.Choices[0].Message.Content))
	if text == "" {
		return "", &model.ProviderError{Code: "OPENAI_FAILED", Message: "generation response had empty content", Retryable: false, StatusCode: resp.StatusCode}
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
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "" || p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

type transcriptionResponse struct {
	Text string `json:"text"`
}

// Transcribe implements model.Transcriber via {base}/audio/transcriptions
// (OpenAI multipart Whisper / gpt-4o-transcribe). Endpoint-dependent per
// SPEC 8.1.2 ³: a compatible base lacking audio surfaces a provider
// error here, never CONFIG_INVALID.
func (c *Client) Transcribe(ctx context.Context, relPath string, data []byte) (string, error) {
	if c.missingRequiredKey() {
		return "", &model.ProviderError{Code: "OPENAI_AUTH", Message: "missing OpenAI API key", Retryable: false}
	}
	if len(data) == 0 {
		return "", &model.ProviderError{Code: "OPENAI_FAILED", Message: "transcription input is empty", Retryable: false}
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
		return "", &model.ProviderError{Code: "OPENAI_FAILED", Message: "failed to build transcription body", Retryable: false, Cause: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/audio/transcriptions", bytes.NewReader(buf.Bytes()))
	if err != nil {
		return "", &model.ProviderError{Code: "OPENAI_FAILED", Message: "failed to build transcription request", Retryable: false, Cause: err}
	}
	c.setAuthHeader(req)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := providerhttp.WithTimeout(c.HTTPClient, timeout).Do(req)
	if err != nil {
		return "", &model.ProviderError{Code: "OPENAI_FAILED", Message: "transcription request failed", Retryable: true, Cause: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", httpError(resp)
	}
	raw, err := providerhttp.ReadLimitedJSONBody(resp, "OPENAI_FAILED")
	if err != nil {
		return "", err
	}
	var parsed transcriptionResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", &model.ProviderError{Code: "OPENAI_FAILED", Message: "failed to decode transcription response", Retryable: false, StatusCode: resp.StatusCode, Cause: err}
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
// fail-open per SPEC 8.3 — callers proceed without audio on error.
func (c *Client) Synthesize(ctx context.Context, text string) ([]byte, error) {
	if c.missingRequiredKey() {
		return nil, &model.ProviderError{Code: "OPENAI_AUTH", Message: "missing OpenAI API key", Retryable: false}
	}
	if strings.TrimSpace(text) == "" {
		return nil, &model.ProviderError{Code: "OPENAI_FAILED", Message: "tts input is empty", Retryable: false}
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
		return nil, &model.ProviderError{Code: "OPENAI_FAILED", Message: "failed to marshal tts request", Retryable: false, Cause: err}
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
		audio, rerr := providerhttp.ReadLimitedBody(resp, providerhttp.MaxAudioResponseBytes, "OPENAI_FAILED")
		if rerr != nil {
			return nil, rerr
		}
		if len(audio) == 0 {
			return nil, &model.ProviderError{Code: "OPENAI_FAILED", Message: "tts returned no audio", Retryable: false, StatusCode: resp.StatusCode}
		}
		return audio, nil
	})
}

// retryAudio runs op with the client's bounded exponential backoff,
// stopping early on a non-retryable *model.ProviderError (shared by
// Transcribe/Synthesize).
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
		return nil, &model.ProviderError{Code: "OPENAI_FAILED", Message: "failed to build request", Retryable: false, Cause: err}
	}
	c.setAuthHeader(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := providerhttp.WithTimeout(c.HTTPClient, timeout).Do(req)
	if err != nil {
		return nil, &model.ProviderError{Code: "OPENAI_FAILED", Message: "request failed", Retryable: true, Cause: err}
	}
	return resp, nil
}

func httpError(resp *http.Response) error {
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(bodyBytes))
	if msg == "" {
		msg = "upstream returned non-200 response"
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &model.ProviderError{Code: "OPENAI_AUTH", Message: msg, Retryable: false, StatusCode: resp.StatusCode}
	case resp.StatusCode == http.StatusTooManyRequests:
		return &model.ProviderError{Code: "OPENAI_RATE_LIMIT", Message: msg, Retryable: true, StatusCode: resp.StatusCode}
	case resp.StatusCode >= http.StatusInternalServerError:
		return &model.ProviderError{Code: "OPENAI_FAILED", Message: msg, Retryable: true, StatusCode: resp.StatusCode}
	default:
		return &model.ProviderError{Code: "OPENAI_FAILED", Message: msg, Retryable: false, StatusCode: resp.StatusCode}
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

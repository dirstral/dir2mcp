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
	"net/http"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
)

const (
	defaultBaseURL           = "https://api.openai.com/v1"
	defaultRequestTimeout    = 30 * time.Second
	defaultGenerationTimeout = 120 * time.Second
	defaultMaxRetries        = 3
	defaultInitialBackoff    = 250 * time.Millisecond
	defaultMaxBackoff        = 2 * time.Second
	defaultBatchSize         = 64

	// DefaultEmbedModel / DefaultChatModel are fallbacks used only when
	// the caller passes an empty model name. The config resolver
	// normally supplies explicit models per profile.
	DefaultEmbedModel = "text-embedding-3-small"
	DefaultChatModel  = "gpt-4o-mini"
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
	// DefaultEmbedModel/DefaultChatModel are used when the corresponding
	// call is made with an empty model name.
	DefaultEmbedModel string
	DefaultChatModel  string
}

// compile-time assertions that *Client implements the model contracts.
var (
	_ model.Embedder  = (*Client)(nil)
	_ model.Generator = (*Client)(nil)
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
}

// Embed implements model.Embedder. OpenAI embeddings are symmetric, so
// the input role is accepted and ignored (SPEC 8.1.5). Inputs are sent
// in BatchSize-sized batches; each batch is retried with bounded
// exponential backoff, and vectors are reordered to match input order.
func (c *Client) Embed(ctx context.Context, modelName string, _ model.EmbedRole, inputs []string) ([][]float32, error) {
	if strings.TrimSpace(c.APIKey) == "" {
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

	var parsed embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, &model.ProviderError{Code: "OPENAI_FAILED", Message: "failed to decode embedding response", Retryable: false, StatusCode: resp.StatusCode, Cause: err}
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

	var parsed generateResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", &model.ProviderError{Code: "OPENAI_FAILED", Message: "failed to decode generation response", Retryable: false, StatusCode: resp.StatusCode, Cause: err}
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

func (c *Client) doJSON(ctx context.Context, path string, body []byte, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, &model.ProviderError{Code: "OPENAI_FAILED", Message: "failed to build request", Retryable: false, Cause: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := clientWithTimeout(c.HTTPClient, timeout).Do(req)
	if err != nil {
		return nil, &model.ProviderError{Code: "OPENAI_FAILED", Message: "request failed", Retryable: true, Cause: err}
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

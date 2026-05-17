// Package anthropic provides a direct-HTTP adapter for the Anthropic
// Messages API (POST {base}/v1/messages). It is chat-only: Anthropic
// exposes no embeddings/OCR/STT/TTS/rerank surface, so this package
// implements model.Generator and nothing else (SPEC 8.1.2).
//
// It mirrors internal/openai and internal/cohere (BaseURL/APIKey/
// HTTPClient, bounded exponential retry, typed model.ProviderError,
// per-call GenerationTimeout via a shallow-copy of HTTPClient) so
// callers can depend on model.Generator without taking a hard
// dependency on this package.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
)

const (
	defaultBaseURL           = "https://api.anthropic.com"
	defaultRequestTimeout    = 30 * time.Second
	defaultGenerationTimeout = 120 * time.Second
	defaultMaxRetries        = 3
	defaultInitialBackoff    = 250 * time.Millisecond
	defaultMaxBackoff        = 2 * time.Second
	defaultMaxTokens         = 4096

	// anthropicVersion is the required API version header value pinned
	// by Anthropic's Messages API.
	anthropicVersion = "2023-06-01"

	// DefaultChatModel is a fallback used only when the caller passes
	// an empty model name. The config resolver normally supplies an
	// explicit model per profile.
	DefaultChatModel = "claude-sonnet-4-6"
)

// Client is an Anthropic Messages API adapter.
//
// Error codes (carried on *model.ProviderError):
//   - ANTHROPIC_AUTH (non-retryable): missing key, 401/403
//   - ANTHROPIC_RATE_LIMIT (retryable): 429
//   - ANTHROPIC_FAILED (retryable for network/5xx, else non-retryable)
type Client struct {
	BaseURL           string
	APIKey            string
	HTTPClient        *http.Client
	MaxRetries        int
	InitialBackoff    time.Duration
	MaxBackoff        time.Duration
	GenerationTimeout time.Duration
	// DefaultChatModel is used when Generate is called and no model has
	// been configured on the client.
	DefaultChatModel string
}

// compile-time assertion that *Client implements model.Generator.
var _ model.Generator = (*Client)(nil)

// NewClient constructs a client with safe default retry/timeout settings.
// An empty baseURL falls back to the public Anthropic endpoint.
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
		GenerationTimeout: defaultGenerationTimeout,
		DefaultChatModel:  DefaultChatModel,
	}
}

type generateMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type generateRequest struct {
	Model     string            `json:"model"`
	MaxTokens int               `json:"max_tokens"`
	Messages  []generateMessage `json:"messages"`
}

type generateResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// Generate implements model.Generator via {base}/v1/messages.
func (c *Client) Generate(ctx context.Context, prompt string) (string, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return "", &model.ProviderError{Code: "ANTHROPIC_AUTH", Message: "missing Anthropic API key", Retryable: false}
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
		Model:     chatModel,
		MaxTokens: defaultMaxTokens,
		Messages:  []generateMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return "", &model.ProviderError{Code: "ANTHROPIC_FAILED", Message: "failed to marshal generation request", Retryable: false, Cause: err}
	}
	resp, err := c.doJSON(ctx, "/v1/messages", body, timeout)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", httpError(resp)
	}

	var parsed generateResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", &model.ProviderError{Code: "ANTHROPIC_FAILED", Message: "failed to decode generation response", Retryable: false, StatusCode: resp.StatusCode, Cause: err}
	}
	text := strings.TrimSpace(contentToText(parsed))
	if text == "" {
		return "", &model.ProviderError{Code: "ANTHROPIC_FAILED", Message: "generation response had empty content", Retryable: false, StatusCode: resp.StatusCode}
	}
	return text, nil
}

// contentToText concatenates the text parts of an Anthropic Messages
// response ([{"type":"text","text":"..."}]); non-text blocks are
// ignored.
func contentToText(parsed generateResponse) string {
	var b strings.Builder
	for _, p := range parsed.Content {
		if p.Type == "" || p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func (c *Client) doJSON(ctx context.Context, path string, body []byte, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, &model.ProviderError{Code: "ANTHROPIC_FAILED", Message: "failed to build request", Retryable: false, Cause: err}
	}
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("content-type", "application/json")

	resp, err := clientWithTimeout(c.HTTPClient, timeout).Do(req)
	if err != nil {
		return nil, &model.ProviderError{Code: "ANTHROPIC_FAILED", Message: "request failed", Retryable: true, Cause: err}
	}
	return resp, nil
}

// clientWithTimeout returns an *http.Client that uses the per-call
// timeout. The default client built by NewClient carries the short
// (30s) request timeout, so Messages calls — which use the longer
// GenerationTimeout — must override it even when HTTPClient is set
// (mirrors internal/openai). The base client's Transport is shared via
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
		return &model.ProviderError{Code: "ANTHROPIC_AUTH", Message: msg, Retryable: false, StatusCode: resp.StatusCode}
	case resp.StatusCode == http.StatusTooManyRequests:
		return &model.ProviderError{Code: "ANTHROPIC_RATE_LIMIT", Message: msg, Retryable: true, StatusCode: resp.StatusCode}
	case resp.StatusCode >= http.StatusInternalServerError:
		return &model.ProviderError{Code: "ANTHROPIC_FAILED", Message: msg, Retryable: true, StatusCode: resp.StatusCode}
	default:
		return &model.ProviderError{Code: "ANTHROPIC_FAILED", Message: msg, Retryable: false, StatusCode: resp.StatusCode}
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

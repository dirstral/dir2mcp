// Package cohere provides a minimal direct-HTTP adapter for the Cohere
// v2 API: Rerank (POST /v2/rerank), Embed (POST /v2/embed), and Chat
// (POST /v2/chat). It mirrors internal/mistral and internal/openai's
// client shape (BaseURL/APIKey/HTTPClient, bounded exponential retry,
// typed model.ProviderError) so retrieval can depend on model.Reranker /
// model.Embedder / model.Generator without taking a hard dependency on
// this package.
//
// Cohere embeddings are ASYMMETRIC (SPEC 8.1.5): the input role is
// mapped onto Cohere's `input_type` (search_document / search_query) and
// observably changes the request — unlike the symmetric OpenAI adapter.
//
// Reranking in dir2mcp is fail-open: callers treat any error from this
// client as "skip rerank, keep fused order" (see internal/retrieval).
package cohere

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
	defaultBaseURL           = "https://api.cohere.com"
	defaultRequestTimeout    = 30 * time.Second
	defaultGenerationTimeout = 120 * time.Second
	defaultMaxRetries        = 3
	defaultInitialBackoff    = 250 * time.Millisecond
	defaultMaxBackoff        = 2 * time.Second
	defaultBatchSize         = 96

	// DefaultRerankModel is Cohere's current GA cross-encoder rerank
	// model. Callers may override via Client.DefaultModel or the
	// modelName argument to Rerank.
	DefaultRerankModel = "rerank-v3.5"

	// DefaultEmbedModel / DefaultChatModel are fallbacks used only when
	// the caller passes an empty model name. The config resolver
	// normally supplies explicit models per profile.
	DefaultEmbedModel = "embed-v4.0"
	DefaultChatModel  = "command-a-03-2025"
)

// Client is a Cohere v2 API adapter (rerank + embed + chat).
//
// Error codes (carried on *model.ProviderError):
//   - COHERE_AUTH (non-retryable): missing key, 401/403
//   - COHERE_RATE_LIMIT (retryable): 429
//   - COHERE_FAILED (retryable for network/5xx, else non-retryable)
type Client struct {
	BaseURL           string
	APIKey            string
	HTTPClient        *http.Client
	MaxRetries        int
	InitialBackoff    time.Duration
	MaxBackoff        time.Duration
	BatchSize         int
	GenerationTimeout time.Duration
	// DefaultModel is used when Rerank is called with an empty modelName.
	DefaultModel string
	// DefaultEmbedModel/DefaultChatModel are used when the corresponding
	// call is made with an empty model name.
	DefaultEmbedModel string
	DefaultChatModel  string
}

// compile-time assertions that *Client implements the model contracts.
var (
	_ model.Reranker  = (*Client)(nil)
	_ model.Embedder  = (*Client)(nil)
	_ model.Generator = (*Client)(nil)
)

// NewClient constructs a client with safe default retry/timeout settings.
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
		DefaultModel:      DefaultRerankModel,
		DefaultEmbedModel: DefaultEmbedModel,
		DefaultChatModel:  DefaultChatModel,
	}
}

// retry runs op with bounded exponential backoff, stopping early on a
// non-retryable *model.ProviderError. T is the per-attempt result type.
func retry[T any](ctx context.Context, c *Client, op func() (T, error)) (T, error) {
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

type rerankRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n,omitempty"`
}

type rerankResponse struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
}

// Rerank re-scores documents against query using Cohere's /v2/rerank
// endpoint and returns results ordered best-first. topN <= 0 requests
// all documents back. An empty documents slice short-circuits to an
// empty result without an API call.
func (c *Client) Rerank(ctx context.Context, modelName, query string, documents []string, topN int) ([]model.Reranked, error) {
	if len(documents) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return nil, &model.ProviderError{Code: "COHERE_AUTH", Message: "missing Cohere API key", Retryable: false}
	}
	if strings.TrimSpace(modelName) == "" {
		modelName = c.DefaultModel
		if modelName == "" {
			modelName = DefaultRerankModel
		}
	}
	return retry(ctx, c, func() ([]model.Reranked, error) {
		return c.rerankOnce(ctx, modelName, query, documents, topN)
	})
}

func (c *Client) rerankOnce(ctx context.Context, modelName, query string, documents []string, topN int) ([]model.Reranked, error) {
	payload := rerankRequest{Model: modelName, Query: query, Documents: documents}
	if topN > 0 {
		payload.TopN = topN
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, &model.ProviderError{Code: "COHERE_FAILED", Message: "failed to marshal rerank request", Retryable: false, Cause: err}
	}

	resp, err := c.doJSON(ctx, "/v2/rerank", body, defaultRequestTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, cohereHTTPError(resp)
	}

	var parsed rerankResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, &model.ProviderError{Code: "COHERE_FAILED", Message: "failed to decode rerank response", Retryable: false, StatusCode: resp.StatusCode, Cause: err}
	}

	out := make([]model.Reranked, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		if r.Index < 0 || r.Index >= len(documents) {
			return nil, &model.ProviderError{Code: "COHERE_FAILED", Message: fmt.Sprintf("rerank response contains out-of-range index %d", r.Index), Retryable: false, StatusCode: resp.StatusCode}
		}
		out = append(out, model.Reranked{Index: r.Index, RelevanceScore: r.RelevanceScore})
	}
	return out, nil
}

type embedRequest struct {
	Model          string   `json:"model"`
	Texts          []string `json:"texts"`
	InputType      string   `json:"input_type"`
	EmbeddingTypes []string `json:"embedding_types"`
}

type embedResponse struct {
	Embeddings struct {
		Float [][]float64 `json:"float"`
	} `json:"embeddings"`
}

// inputTypeForRole maps the SPEC 8.1.5 input role onto Cohere's
// asymmetric `input_type`. EmbedQuery -> "search_query"; everything
// else (including EmbedDocument) -> "search_document".
func inputTypeForRole(role model.EmbedRole) string {
	if role == model.EmbedQuery {
		return "search_query"
	}
	return "search_document"
}

// Embed implements model.Embedder. Cohere embeddings are ASYMMETRIC
// (SPEC 8.1.5): the role is mapped onto `input_type`
// (search_document / search_query) and observably changes the request.
// Inputs are sent in BatchSize-sized batches; each batch is retried with
// bounded exponential backoff, and vectors preserve input order.
func (c *Client) Embed(ctx context.Context, modelName string, role model.EmbedRole, inputs []string) ([][]float32, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return nil, &model.ProviderError{Code: "COHERE_AUTH", Message: "missing Cohere API key", Retryable: false}
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
	inputType := inputTypeForRole(role)

	out := make([][]float32, 0, len(inputs))
	for start := 0; start < len(inputs); start += batchSize {
		end := start + batchSize
		if end > len(inputs) {
			end = len(inputs)
		}
		batch := inputs[start:end]
		vectors, err := retry(ctx, c, func() ([][]float32, error) {
			return c.embedBatch(ctx, modelName, inputType, batch)
		})
		if err != nil {
			return nil, err
		}
		out = append(out, vectors...)
	}
	return out, nil
}

func (c *Client) embedBatch(ctx context.Context, modelName, inputType string, inputs []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{
		Model:          modelName,
		Texts:          inputs,
		InputType:      inputType,
		EmbeddingTypes: []string{"float"},
	})
	if err != nil {
		return nil, &model.ProviderError{Code: "COHERE_FAILED", Message: "failed to marshal embedding request", Retryable: false, Cause: err}
	}
	resp, err := c.doJSON(ctx, "/v2/embed", body, defaultRequestTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, cohereHTTPError(resp)
	}

	var parsed embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, &model.ProviderError{Code: "COHERE_FAILED", Message: "failed to decode embedding response", Retryable: false, StatusCode: resp.StatusCode, Cause: err}
	}
	floats := parsed.Embeddings.Float
	if len(floats) != len(inputs) {
		return nil, &model.ProviderError{Code: "COHERE_FAILED", Message: fmt.Sprintf("embedding response size mismatch: got %d vectors for %d inputs", len(floats), len(inputs)), Retryable: false, StatusCode: resp.StatusCode}
	}

	vectors := make([][]float32, len(inputs))
	for i, v := range floats {
		vec := make([]float32, len(v))
		for j, f := range v {
			vec[j] = float32(f)
		}
		vectors[i] = vec
	}
	return vectors, nil
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatResponse struct {
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// Generate implements model.Generator via Cohere's v2 /v2/chat endpoint.
func (c *Client) Generate(ctx context.Context, prompt string) (string, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return "", &model.ProviderError{Code: "COHERE_AUTH", Message: "missing Cohere API key", Retryable: false}
	}
	chatModel := strings.TrimSpace(c.DefaultChatModel)
	if chatModel == "" {
		chatModel = DefaultChatModel
	}
	timeout := c.GenerationTimeout
	if timeout <= 0 {
		timeout = defaultGenerationTimeout
	}
	return retry(ctx, c, func() (string, error) {
		return c.generateOnce(ctx, chatModel, prompt, timeout)
	})
}

func (c *Client) generateOnce(ctx context.Context, chatModel, prompt string, timeout time.Duration) (string, error) {
	body, err := json.Marshal(chatRequest{
		Model:    chatModel,
		Messages: []chatMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return "", &model.ProviderError{Code: "COHERE_FAILED", Message: "failed to marshal generation request", Retryable: false, Cause: err}
	}
	resp, err := c.doJSON(ctx, "/v2/chat", body, timeout)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", cohereHTTPError(resp)
	}

	var parsed chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", &model.ProviderError{Code: "COHERE_FAILED", Message: "failed to decode generation response", Retryable: false, StatusCode: resp.StatusCode, Cause: err}
	}
	var b strings.Builder
	for _, p := range parsed.Message.Content {
		if p.Type == "" || p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	text := strings.TrimSpace(b.String())
	if text == "" {
		return "", &model.ProviderError{Code: "COHERE_FAILED", Message: "generation response had empty content", Retryable: false, StatusCode: resp.StatusCode}
	}
	return text, nil
}

// doJSON issues an authenticated POST with a JSON body. The per-call
// timeout overrides the (short) default request timeout carried by the
// client built in NewClient, so chat completions can use the longer
// GenerationTimeout even when HTTPClient is set.
func (c *Client) doJSON(ctx context.Context, path string, body []byte, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, &model.ProviderError{Code: "COHERE_FAILED", Message: "failed to build request", Retryable: false, Cause: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := clientWithTimeout(c.HTTPClient, timeout).Do(req)
	if err != nil {
		return nil, &model.ProviderError{Code: "COHERE_FAILED", Message: "request failed", Retryable: true, Cause: err}
	}
	return resp, nil
}

// clientWithTimeout returns an *http.Client that uses the per-call
// timeout. The default client built by NewClient carries the short
// (30s) request timeout, so chat completions — which use the longer
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

func cohereHTTPError(resp *http.Response) error {
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(bodyBytes))
	if msg == "" {
		msg = "upstream returned non-200 response"
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &model.ProviderError{Code: "COHERE_AUTH", Message: msg, Retryable: false, StatusCode: resp.StatusCode}
	case resp.StatusCode == http.StatusTooManyRequests:
		return &model.ProviderError{Code: "COHERE_RATE_LIMIT", Message: msg, Retryable: true, StatusCode: resp.StatusCode}
	case resp.StatusCode >= http.StatusInternalServerError:
		return &model.ProviderError{Code: "COHERE_FAILED", Message: msg, Retryable: true, StatusCode: resp.StatusCode}
	default:
		return &model.ProviderError{Code: "COHERE_FAILED", Message: msg, Retryable: false, StatusCode: resp.StatusCode}
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

// Package cohere provides a minimal direct-HTTP adapter for the Cohere
// Rerank v2 API (POST /v2/rerank). It mirrors internal/mistral's client
// shape (BaseURL/APIKey/HTTPClient, bounded exponential retry, typed
// model.ProviderError) so retrieval can depend on a model.Reranker
// without taking a hard dependency on this package.
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
	defaultBaseURL        = "https://api.cohere.com"
	defaultRequestTimeout = 30 * time.Second
	defaultMaxRetries     = 3
	defaultInitialBackoff = 250 * time.Millisecond
	defaultMaxBackoff     = 2 * time.Second

	// DefaultRerankModel is Cohere's current GA cross-encoder rerank
	// model. Callers may override via Client.DefaultModel or the
	// modelName argument to Rerank.
	DefaultRerankModel = "rerank-v3.5"
)

// Client is a Cohere Rerank API adapter.
//
// Error codes (carried on *model.ProviderError):
//   - COHERE_AUTH (non-retryable): 401/403
//   - COHERE_RATE_LIMIT (retryable): 429
//   - COHERE_FAILED (retryable for network/5xx, else non-retryable)
type Client struct {
	BaseURL        string
	APIKey         string
	HTTPClient     *http.Client
	MaxRetries     int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// DefaultModel is used when Rerank is called with an empty modelName.
	DefaultModel string
}

// compile-time assertion that *Client implements model.Reranker.
var _ model.Reranker = (*Client)(nil)

// NewClient constructs a client with safe default retry/timeout settings.
func NewClient(baseURL, apiKey string) *Client {
	baseURL = strings.TrimSpace(baseURL)
	apiKey = strings.TrimSpace(apiKey)
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		BaseURL:        strings.TrimRight(baseURL, "/"),
		APIKey:         apiKey,
		HTTPClient:     &http.Client{Timeout: defaultRequestTimeout},
		MaxRetries:     defaultMaxRetries,
		InitialBackoff: defaultInitialBackoff,
		MaxBackoff:     defaultMaxBackoff,
		DefaultModel:   DefaultRerankModel,
	}
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
		results, err := c.rerankOnce(ctx, modelName, query, documents, topN)
		if err == nil {
			return results, nil
		}
		lastErr = err
		var pErr *model.ProviderError
		if errors.As(err, &pErr) && !pErr.Retryable {
			return nil, err
		}
	}
	return nil, lastErr
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v2/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, &model.ProviderError{Code: "COHERE_FAILED", Message: "failed to build rerank request", Retryable: false, Cause: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultRequestTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &model.ProviderError{Code: "COHERE_FAILED", Message: "rerank request failed", Retryable: true, Cause: err}
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

// Package colbertrerank is a pure-Go client for a self-hosted
// late-interaction / multi-vector (ColBERT-style) reranking endpoint
// (dir2mcp#337).
//
// It targets a portable JSON rerank contract — POST {BaseURL}/rerank with
// a query plus the candidate documents, returning a relevance score per
// document — that common self-hosted late-interaction servers expose
// (ColBERTv2/PLAID, Jina-ColBERT-v2, GTE-ModernColBERT shims). The wire
// shape deliberately mirrors the Cohere /v2/rerank request/response
// (model/query/documents -> results[].{index,relevance_score}) so the same
// retrieval seam (model.Reranker) drives either backend unchanged.
//
// The endpoint may be credential-less (a self-hosted box on a private
// network): a Bearer token is sent only when an api_key is configured —
// the same self-hosted shape as internal/whisperapi for STT.
//
// Reranking in dir2mcp is fail-open: callers treat any error from this
// client as "skip rerank, keep the fused order" (see internal/retrieval).
// The client never logs document text or secrets; HTTP errors carry only a
// status-derived message (CLAUDE.md: no raw sensitive payloads).
package colbertrerank

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
	defaultRequestTimeout = 30 * time.Second
	defaultMaxRetries     = 3
	defaultInitialBackoff = 250 * time.Millisecond
	defaultMaxBackoff     = 2 * time.Second

	// DefaultModel is a sensible fallback model name. Self-hosted servers
	// frequently ignore the field or serve a single fixed checkpoint, but
	// operators SHOULD set an explicit model via the provider profile.
	DefaultModel = "colbert"
)

// Client speaks a JSON late-interaction rerank contract against a
// self-hosted base URL.
//
// Error codes (carried on *model.ProviderError):
//   - COLBERT_AUTH (non-retryable): upstream 401/403.
//   - COLBERT_RATE_LIMIT (retryable): upstream 429.
//   - COLBERT_FAILED (retryable for network/5xx, non-retryable otherwise).
type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client

	MaxRetries     int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration

	// DefaultModel is sent as the request `model` field when Rerank is
	// called with an empty modelName. Empty falls back to the package
	// DefaultModel constant.
	DefaultModel string
}

// compile-time assertion that *Client implements the rerank contract.
var _ model.Reranker = (*Client)(nil)

// NewClient constructs a client with safe default retry/timeout settings.
// apiKey may be empty for a credential-less self-hosted endpoint.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL:        strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:         strings.TrimSpace(apiKey),
		HTTPClient:     &http.Client{Timeout: defaultRequestTimeout},
		MaxRetries:     defaultMaxRetries,
		InitialBackoff: defaultInitialBackoff,
		MaxBackoff:     defaultMaxBackoff,
		DefaultModel:   DefaultModel,
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

// Rerank re-scores documents against query via the self-hosted late-interaction
// endpoint and returns results ordered best-first. topN <= 0 requests all
// documents back. An empty documents slice short-circuits to an empty result
// without an API call. A credential-less endpoint is allowed (no api_key); only
// a missing base_url is a hard error.
func (c *Client) Rerank(ctx context.Context, modelName, query string, documents []string, topN int) ([]model.Reranked, error) {
	if len(documents) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		return nil, &model.ProviderError{Code: "COLBERT_FAILED", Message: "missing colbert base_url", Retryable: false}
	}
	if strings.TrimSpace(modelName) == "" {
		modelName = strings.TrimSpace(c.DefaultModel)
		if modelName == "" {
			modelName = DefaultModel
		}
	}
	return c.rerankWithRetry(ctx, modelName, query, documents, topN)
}

func (c *Client) rerankWithRetry(ctx context.Context, modelName, query string, documents []string, topN int) ([]model.Reranked, error) {
	maxAttempts := c.MaxRetries + 1
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		out, err := c.rerankOnce(ctx, modelName, query, documents, topN)
		if err == nil {
			return out, nil
		}
		lastErr = err

		var providerErr *model.ProviderError
		if !errors.As(err, &providerErr) || !providerErr.Retryable || attempt == maxAttempts-1 {
			return nil, err
		}
		if waitErr := c.wait(ctx, c.backoffForAttempt(attempt)); waitErr != nil {
			return nil, waitErr
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
		return nil, &model.ProviderError{Code: "COLBERT_FAILED", Message: "failed to marshal rerank request", Retryable: false, Cause: err}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, &model.ProviderError{Code: "COLBERT_FAILED", Message: "failed to build rerank request", Retryable: false, Cause: err}
	}
	// Bearer auth is optional: only set it for credentialed endpoints.
	if key := strings.TrimSpace(c.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultRequestTimeout}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, &model.ProviderError{Code: "COLBERT_FAILED", Message: "rerank request failed", Retryable: true, Cause: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, httpError(resp)
	}

	var parsed rerankResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, &model.ProviderError{Code: "COLBERT_FAILED", Message: "failed to decode rerank response", Retryable: false, StatusCode: resp.StatusCode, Cause: err}
	}

	out := make([]model.Reranked, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		if r.Index < 0 || r.Index >= len(documents) {
			return nil, &model.ProviderError{Code: "COLBERT_FAILED", Message: fmt.Sprintf("rerank response contains out-of-range index %d", r.Index), Retryable: false, StatusCode: resp.StatusCode}
		}
		out = append(out, model.Reranked{Index: r.Index, RelevanceScore: r.RelevanceScore})
	}
	return out, nil
}

// httpError maps an upstream non-200 onto a typed *model.ProviderError. The
// raw upstream body is drained (bounded) but never surfaced: it can echo
// prompt/document content (CLAUDE.md: no raw sensitive payloads in errors).
// The HTTP status is sufficient and machine-parseable via StatusCode.
func httpError(resp *http.Response) error {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &model.ProviderError{Code: "COLBERT_AUTH", Message: fmt.Sprintf("colbert rerank rejected (HTTP %d)", resp.StatusCode), Retryable: false, StatusCode: resp.StatusCode}
	case resp.StatusCode == http.StatusTooManyRequests:
		return &model.ProviderError{Code: "COLBERT_RATE_LIMIT", Message: "colbert rerank rate limited (HTTP 429)", Retryable: true, StatusCode: resp.StatusCode}
	case resp.StatusCode >= http.StatusInternalServerError:
		return &model.ProviderError{Code: "COLBERT_FAILED", Message: fmt.Sprintf("colbert upstream error (HTTP %d)", resp.StatusCode), Retryable: true, StatusCode: resp.StatusCode}
	default:
		return &model.ProviderError{Code: "COLBERT_FAILED", Message: fmt.Sprintf("colbert rerank failed (HTTP %d)", resp.StatusCode), Retryable: false, StatusCode: resp.StatusCode}
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

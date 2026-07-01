// Package omniembed is a pure-Go client for a self-hosted, unified
// multimodal embedding endpoint — the off-API multimodal path
// (dir2mcp#334), extending the self-hosted-endpoint capability (#240) and
// the GPU-VPS / dual-machine epic (#250) to multimodal embedding.
//
// It targets an OmniEmbed / Qwen2.5-Omni model (Tevatron/OmniEmbed-v0.1)
// served behind an OpenAI-compatible POST {BaseURL}/v1/embeddings surface,
// which vLLM exposes for embedding models. Text is sent as plain strings;
// non-text media (images, audio, video, PDFs) are sent as RFC 2397 data
// URIs in the same `input` array — the convention OpenAI-compatible
// multimodal embedding servers accept — so a single request mixes
// modalities into ONE shared vector space (the property OmniEmbed provides
// and that SPEC 8.1.7 requires).
//
// The endpoint may be credential-less (a self-hosted box on a private
// network): a Bearer token is sent only when an api_key is configured.
//
// This client never logs media bytes, the request body, or the API key.
package omniembed

import (
	"bytes"
	"context"
	"encoding/base64"
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
	defaultRequestTimeout = 120 * time.Second
	defaultMaxRetries     = 3
	defaultInitialBackoff = 250 * time.Millisecond
	defaultMaxBackoff     = 2 * time.Second
	defaultBatchSize      = 32
	// defaultMaxItemBytes bounds a single media payload so callers avoid
	// sending absurdly large blobs that fail upstream or time out. Media
	// chunks are already reduced to one PDF page / one A-V window upstream
	// (SPEC 8.1.7), so this is a defensive ceiling, not a tuning knob.
	defaultMaxItemBytes = 50 * 1024 * 1024

	// DefaultModel is a sensible fallback model name. Self-hosted servers
	// frequently ignore the field or accept the served model alias, but
	// operators SHOULD set an explicit model via the provider profile.
	DefaultModel = "omniembed"

	// maxResponseBytes caps a success response body so a malicious or buggy
	// self-hosted endpoint (the omniembed base_url is operator-supplied and
	// often unauthenticated) cannot drive unbounded memory use via a
	// giant/gzip-bombed 200 response (issue #416).
	maxResponseBytes = 64 << 20 // 64 MiB
)

// Client speaks the OpenAI-compatible /v1/embeddings contract against a
// self-hosted base URL serving a unified multimodal embedding model.
//
// Error codes:
//   - OMNIEMBED_AUTH (non-retryable): upstream 401/403.
//   - OMNIEMBED_RATE_LIMIT (retryable): upstream 429.
//   - OMNIEMBED_FAILED (retryable for network/5xx, non-retryable otherwise).
type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client

	MaxRetries     int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// BatchSize bounds how many inputs are sent per request. Values <= 0
	// fall back to defaultBatchSize.
	BatchSize int
	// MaxItemBytes bounds a single media payload (bytes). Values <= 0 fall
	// back to defaultMaxItemBytes.
	MaxItemBytes int

	// DefaultEmbedModel is sent as the `model` field. Empty falls back to
	// the package DefaultModel constant.
	DefaultEmbedModel string
}

// compile-time assertions that *Client implements the model contracts.
var (
	_ model.Embedder           = (*Client)(nil)
	_ model.MultimodalEmbedder = (*Client)(nil)
)

// NewClient constructs a client with safe default retry/timeout settings.
// apiKey may be empty for a credential-less self-hosted endpoint.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL:           strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:            strings.TrimSpace(apiKey),
		HTTPClient:        &http.Client{Timeout: defaultRequestTimeout},
		MaxRetries:        defaultMaxRetries,
		InitialBackoff:    defaultInitialBackoff,
		MaxBackoff:        defaultMaxBackoff,
		BatchSize:         defaultBatchSize,
		MaxItemBytes:      defaultMaxItemBytes,
		DefaultEmbedModel: DefaultModel,
	}
}

// embedRequest is the OpenAI-compatible embeddings request. Input carries
// plain text strings and/or data-URI media strings in one array; the
// server embeds every element into the same vector space.
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

// Embed implements model.Embedder. Self-hosted unified embeddings are
// symmetric, so the input role is accepted and ignored (SPEC 8.1.5).
// Inputs are sent in BatchSize-sized batches; each batch is retried with
// bounded exponential backoff, and vectors are reordered to match input
// order.
func (c *Client) Embed(ctx context.Context, modelName string, _ model.EmbedRole, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return [][]float32{}, nil
	}
	encoded := make([]string, len(inputs))
	copy(encoded, inputs)
	return c.embedAll(ctx, modelName, encoded)
}

// EmbedMedia implements model.MultimodalEmbedder. Each media item is
// encoded as an RFC 2397 base64 data URI and sent in the SAME `input`
// array Embed uses, so media vectors are comparable to text vectors (SPEC
// 8.1.7). Role is accepted and ignored (symmetric model). Bytes are never
// logged.
func (c *Client) EmbedMedia(ctx context.Context, modelName string, _ model.EmbedRole, items []model.MediaInput) ([][]float32, error) {
	if len(items) == 0 {
		return [][]float32{}, nil
	}
	maxItem := c.MaxItemBytes
	if maxItem <= 0 {
		maxItem = defaultMaxItemBytes
	}
	encoded := make([]string, len(items))
	for i, it := range items {
		if len(it.Data) == 0 {
			return nil, &model.ProviderError{Code: "OMNIEMBED_FAILED", Message: "media input is empty", Retryable: false}
		}
		if len(it.Data) > maxItem {
			return nil, &model.ProviderError{
				Code:      "OMNIEMBED_FAILED",
				Message:   fmt.Sprintf("media input too large (%d bytes, limit %d)", len(it.Data), maxItem),
				Retryable: false,
			}
		}
		encoded[i] = dataURI(it.MimeType, it.Data)
	}
	return c.embedAll(ctx, modelName, encoded)
}

// dataURI builds an RFC 2397 base64 data URI for the media bytes. An empty
// MIME type defaults to application/octet-stream so the URI stays
// well-formed.
func dataURI(mimeType string, data []byte) string {
	mt := strings.TrimSpace(mimeType)
	if mt == "" {
		mt = "application/octet-stream"
	}
	return "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// embedAll resolves the model name, sends inputs in BatchSize-sized
// batches (each retried), and concatenates vectors in input order.
func (c *Client) embedAll(ctx context.Context, modelName string, inputs []string) ([][]float32, error) {
	if strings.TrimSpace(c.BaseURL) == "" {
		return nil, &model.ProviderError{Code: "OMNIEMBED_FAILED", Message: "missing omniembed base_url", Retryable: false}
	}
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		modelName = strings.TrimSpace(c.DefaultEmbedModel)
		if modelName == "" {
			modelName = DefaultModel
		}
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
		return nil, &model.ProviderError{Code: "OMNIEMBED_FAILED", Message: "failed to marshal embedding request", Retryable: false, Cause: err}
	}
	resp, err := c.doJSON(ctx, "/v1/embeddings", body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, httpError(resp)
	}

	raw, err := readLimitedBody(resp, maxResponseBytes)
	if err != nil {
		return nil, err
	}
	var parsed embedResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, &model.ProviderError{Code: "OMNIEMBED_FAILED", Message: "failed to decode embedding response", Retryable: false, StatusCode: resp.StatusCode, Cause: err}
	}
	return collectVectors(parsed, len(inputs), resp.StatusCode)
}

// collectVectors validates the response covers every input exactly once
// and reorders vectors by the server-reported index to match input order.
func collectVectors(parsed embedResponse, n, statusCode int) ([][]float32, error) {
	fail := func(msg string) error {
		return &model.ProviderError{Code: "OMNIEMBED_FAILED", Message: msg, Retryable: false, StatusCode: statusCode}
	}
	if len(parsed.Data) != n {
		return nil, fail(fmt.Sprintf("embedding response size mismatch: got %d vectors for %d inputs", len(parsed.Data), n))
	}
	vectors := make([][]float32, n)
	seen := make([]bool, n)
	for _, item := range parsed.Data {
		if item.Index < 0 || item.Index >= n {
			return nil, fail(fmt.Sprintf("embedding response contains out-of-range index %d", item.Index))
		}
		if seen[item.Index] {
			return nil, fail(fmt.Sprintf("embedding response contains duplicate index %d", item.Index))
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
			return nil, fail(fmt.Sprintf("embedding response missing index %d", i))
		}
	}
	return vectors, nil
}

func (c *Client) doJSON(ctx context.Context, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, &model.ProviderError{Code: "OMNIEMBED_FAILED", Message: "failed to build request", Retryable: false, Cause: err}
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
		return nil, &model.ProviderError{Code: "OMNIEMBED_FAILED", Message: "request failed", Retryable: true, Cause: err}
	}
	return resp, nil
}

// readLimitedBody buffers a success response body under maxResponseBytes,
// returning a clear error rather than reading unbounded if the upstream sends
// more (issue #416). It reads one byte past the cap to detect an over-limit
// body without buffering the whole thing.
func readLimitedBody(resp *http.Response, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, &model.ProviderError{Code: "OMNIEMBED_FAILED", Message: "failed to read response", Retryable: true, StatusCode: resp.StatusCode, Cause: err}
	}
	if int64(len(data)) > limit {
		return nil, &model.ProviderError{Code: "OMNIEMBED_FAILED", Message: fmt.Sprintf("response exceeds %d-byte limit", limit), Retryable: false, StatusCode: resp.StatusCode}
	}
	return data, nil
}

func httpError(resp *http.Response) error {
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	errMsg := strings.TrimSpace(string(bodyBytes))
	if errMsg == "" {
		errMsg = "upstream returned non-200 response"
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &model.ProviderError{Code: "OMNIEMBED_AUTH", Message: errMsg, Retryable: false, StatusCode: resp.StatusCode}
	case resp.StatusCode == http.StatusTooManyRequests:
		return &model.ProviderError{Code: "OMNIEMBED_RATE_LIMIT", Message: errMsg, Retryable: true, StatusCode: resp.StatusCode}
	case resp.StatusCode >= http.StatusInternalServerError:
		return &model.ProviderError{Code: "OMNIEMBED_FAILED", Message: errMsg, Retryable: true, StatusCode: resp.StatusCode}
	default:
		return &model.ProviderError{Code: "OMNIEMBED_FAILED", Message: errMsg, Retryable: false, StatusCode: resp.StatusCode}
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

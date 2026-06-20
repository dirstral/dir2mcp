package mistral

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/usage"
)

const (
	defaultBaseURL        = "https://api.mistral.ai"
	defaultBatchSize      = 32
	defaultRequestTimeout = 30 * time.Second
	// defaultGenerationTimeout is the timeout used for /v1/chat/completions when
	// no explicit GenerationTimeout is supplied by the caller.
	defaultGenerationTimeout = 120 * time.Second
	defaultMaxRetries        = 3
	defaultInitialBackoff    = 250 * time.Millisecond
	defaultMaxBackoff        = 2 * time.Second
	// Bound OCR request payload size (data URL + base64) to avoid oversized
	// requests that frequently fail upstream or time out in transit.
	defaultMaxOCRPayloadBytes = 20 * 1024 * 1024

	// DefaultOCRModel is the default Mistral model used for OCR requests when
	// no other model is specified.  Consumers may override the value on the
	// Client struct if they need to target a different version in the future.
	DefaultOCRModel = "mistral-ocr-latest"
	// DefaultChatModel is the default model name used for chat/completion
	// requests.  Operators may override this via Client.DefaultChatModel to use
	// aliases (e.g. "mistral-small-latest") without editing code.
	DefaultChatModel = "mistral-small-2506"
	// DefaultTranscribeModel is the default model used for audio
	// transcription requests.
	DefaultTranscribeModel = "voxtral-mini-latest"
)

// Client provides Mistral API integrations.
//
// Embedding defaults:
// - base URL: https://api.mistral.ai
// - batch size: 32
// - request timeout: 30s
// - max retries: 3
// - backoff: 250ms exponential up to 2s
//
// Embed error codes:
// - MISTRAL_AUTH (non-retryable): missing/invalid credentials (401/403)
// - MISTRAL_RATE_LIMIT (retryable): upstream 429 responses
// - MISTRAL_FAILED (retryable for network/5xx, non-retryable for other 4xx/validation)
type Client struct {
	BaseURL        string
	APIKey         string
	HTTPClient     *http.Client
	BatchSize      int
	MaxRetries     int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// MaxOCRPayloadBytes bounds the encoded OCR payload size (bytes) before
	// issuing an OCR request. Values <= 0 fall back to defaultMaxOCRPayloadBytes.
	MaxOCRPayloadBytes int

	// GenerationTimeout is the HTTP client timeout used for the chat
	// completions endpoint.  Generation requests can take longer than other
	// API calls, so this field allows configuration without affecting the
	// timeout used for embeds/ocr/transcribe.  If zero, the regular
	// defaultRequestTimeout is used.
	GenerationTimeout time.Duration

	// DefaultChatModel controls which chat/completions model is used for
	// Generate requests.  The package-level constant DefaultChatModel is used
	// when this field is empty to preserve existing behaviour.  Tests and
	// applications may override this to experiment with alternative models
	// or aliases (e.g. "mistral-small-latest").
	DefaultChatModel string
	// DefaultTranscribeModel controls the model string sent with audio
	// transcription requests.  Callers may override it on the client instance.
	DefaultTranscribeModel string
	// DefaultTranscribeLanguage optionally sets a language hint for
	// transcription requests. Empty means provider auto-detection.
	DefaultTranscribeLanguage string

	// DefaultOCRModel will be sent to the OCR endpoint if the caller does not
	// specify an explicit model.  It is initialized to DefaultOCRModel by
	// NewClient and can be mutated by callers to customize behaviour.
	DefaultOCRModel string
}

// NewClient constructs a client with safe default retry/timeout settings.
func NewClient(baseURL, apiKey string) *Client {
	baseURL = strings.TrimSpace(baseURL)
	apiKey = strings.TrimSpace(apiKey)
	if baseURL == "" {
		baseURL = defaultBaseURL
	}

	return &Client{
		BaseURL:                strings.TrimRight(baseURL, "/"),
		APIKey:                 apiKey,
		HTTPClient:             &http.Client{Timeout: defaultRequestTimeout},
		BatchSize:              defaultBatchSize,
		MaxRetries:             defaultMaxRetries,
		InitialBackoff:         defaultInitialBackoff,
		MaxBackoff:             defaultMaxBackoff,
		MaxOCRPayloadBytes:     defaultMaxOCRPayloadBytes,
		GenerationTimeout:      defaultGenerationTimeout,
		DefaultChatModel:       DefaultChatModel,
		DefaultTranscribeModel: DefaultTranscribeModel,
		// ensure we always have a sensible default even if callers forget to
		// set a model explicitly later on.
		DefaultOCRModel: DefaultOCRModel,
	}
}

// Embed implements model.Embedder. Mistral embeddings are symmetric, so
// the input role is accepted and intentionally ignored (SPEC 8.1.5).
func (c *Client) Embed(ctx context.Context, modelName string, _ model.EmbedRole, inputs []string) ([][]float32, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return nil, &model.ProviderError{
			Code:      "MISTRAL_AUTH",
			Message:   "missing Mistral API key",
			Retryable: false,
		}
	}
	if strings.TrimSpace(modelName) == "" {
		return nil, &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   "model name is required",
			Retryable: false,
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

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedDataItem struct {
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

type embedResponse struct {
	Data  []embedDataItem   `json:"data"`
	Usage usage.OpenAIUsage `json:"usage"`
}

type ocrRequest struct {
	Model    string      `json:"model"`
	Document ocrDocument `json:"document"`
}

type ocrDocument struct {
	Type        string `json:"type"`
	DocumentURL string `json:"document_url"`
}

type ocrResponse struct {
	Pages []struct {
		Markdown string `json:"markdown"`
		Text     string `json:"text"`
	} `json:"pages"`
	Text     string `json:"text"`
	Markdown string `json:"markdown"`
}

type transcribeResponse struct {
	Text     string `json:"text"`
	Language string `json:"language,omitempty"`
	Model    string `json:"model,omitempty"`
	Segments []struct {
		Start float64 `json:"start"`
		End   float64 `json:"end"`
		Text  string  `json:"text"`
	} `json:"segments"`
}

type generateRequest struct {
	Model    string            `json:"model"`
	Messages []generateMessage `json:"messages"`
}

type generateMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type generateResponse struct {
	Choices []struct {
		Message struct {
			Content interface{} `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage usage.OpenAIUsage `json:"usage"`
}

// reportUsage forwards a provider-reported usage object to the per-query sink
// when present (issue #327). A missing usage object leaves the stage unknown
// (not zero). Inert when no sink is attached to ctx.
func reportUsage(ctx context.Context, stage usage.Stage, u usage.OpenAIUsage) {
	if !u.Empty() {
		usage.Report(ctx, stage, u.ToUsage())
	}
}

func (c *Client) embedBatchWithRetry(ctx context.Context, modelName string, inputs []string) ([][]float32, error) {
	maxRetries := c.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		vectors, err := c.embedBatch(ctx, modelName, inputs)
		if err == nil {
			return vectors, nil
		}
		lastErr = err

		var providerErr *model.ProviderError
		if !errors.As(err, &providerErr) || !providerErr.Retryable || attempt == maxRetries {
			return nil, err
		}

		backoff := c.backoffForAttempt(attempt)
		if waitErr := c.wait(ctx, backoff); waitErr != nil {
			return nil, waitErr
		}
	}

	return nil, lastErr
}

func (c *Client) embedBatch(ctx context.Context, modelName string, inputs []string) ([][]float32, error) {
	reqPayload := embedRequest{
		Model: modelName,
		Input: inputs,
	}

	body, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   "failed to marshal embedding request",
			Retryable: false,
			Cause:     err,
		}
	}

	url := c.BaseURL + "/v1/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   "failed to build embedding request",
			Retryable: false,
			Cause:     err,
		}
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")

	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultRequestTimeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   "embedding request failed",
			Retryable: true,
			Cause:     err,
		}
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, mistralHTTPError(resp)
	}

	var parsed embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, &model.ProviderError{
			Code:       "MISTRAL_FAILED",
			Message:    "failed to decode embedding response",
			Retryable:  false,
			StatusCode: resp.StatusCode,
			Cause:      err,
		}
	}

	reportUsage(ctx, usage.StageEmbed, parsed.Usage)

	if len(parsed.Data) != len(inputs) {
		return nil, &model.ProviderError{
			Code:       "MISTRAL_FAILED",
			Message:    fmt.Sprintf("embedding response size mismatch: got %d vectors for %d inputs", len(parsed.Data), len(inputs)),
			Retryable:  false,
			StatusCode: resp.StatusCode,
		}
	}

	vectors := make([][]float32, len(inputs))
	seen := make([]bool, len(inputs))
	for _, item := range parsed.Data {
		if item.Index < 0 || item.Index >= len(inputs) {
			return nil, &model.ProviderError{
				Code:       "MISTRAL_FAILED",
				Message:    "embedding response contains invalid index",
				Retryable:  false,
				StatusCode: resp.StatusCode,
			}
		}
		if seen[item.Index] {
			return nil, &model.ProviderError{
				Code:       "MISTRAL_FAILED",
				Message:    fmt.Sprintf("embedding response contains duplicate index: %d", item.Index),
				Retryable:  false,
				StatusCode: resp.StatusCode,
			}
		}
		vector := make([]float32, len(item.Embedding))
		for i, val := range item.Embedding {
			vector[i] = float32(val)
		}
		vectors[item.Index] = vector
		seen[item.Index] = true
	}

	for i := range seen {
		if !seen[i] {
			return nil, &model.ProviderError{
				Code:       "MISTRAL_FAILED",
				Message:    fmt.Sprintf("embedding response missing index: %d", i),
				Retryable:  false,
				StatusCode: resp.StatusCode,
			}
		}
	}

	return vectors, nil
}

// mistralHTTPError reads the response body and returns a *model.ProviderError
// appropriate for the HTTP status code. It is a shared helper used by all
// single-attempt functions (embedBatch, extractOnce, transcribeOnce,
// generateOnce) to avoid duplicating the status-code switch.
func mistralHTTPError(resp *http.Response) error {
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	errMsg := strings.TrimSpace(string(bodyBytes))
	if errMsg == "" {
		errMsg = "upstream returned non-200 response"
	}
	isAuthError := resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden
	isRateLimit := resp.StatusCode == http.StatusTooManyRequests
	isServerError := resp.StatusCode >= http.StatusInternalServerError
	switch {
	case isAuthError:
		return &model.ProviderError{Code: "MISTRAL_AUTH", Message: errMsg, Retryable: false, StatusCode: resp.StatusCode}
	case isRateLimit:
		return &model.ProviderError{Code: "MISTRAL_RATE_LIMIT", Message: errMsg, Retryable: true, StatusCode: resp.StatusCode}
	case isServerError:
		return &model.ProviderError{Code: "MISTRAL_FAILED", Message: errMsg, Retryable: true, StatusCode: resp.StatusCode}
	default:
		return &model.ProviderError{Code: "MISTRAL_FAILED", Message: errMsg, Retryable: false, StatusCode: resp.StatusCode}
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

func (c *Client) Extract(ctx context.Context, relPath string, data []byte) (string, error) {
	// the public entrypoint simply delegates to a retry-capable helper. the
	// retry logic mirrors embedBatchWithRetry so that network/rate-limit/5xx
	// conditions are automatically retried up to configured limits.
	return c.extractWithRetry(ctx, relPath, data)
}

// extractWithRetry wraps extractOnce with retry logic similar to
// embedBatchWithRetry.  Only provider errors marked Retryable will be
// retried, up to Client.MaxRetries attempts with exponential backoff.
// The helper intentionally mirrors the structure of embedBatchWithRetry so
// behaviour is consistent between embedding and OCR operations.
func (c *Client) extractWithRetry(ctx context.Context, relPath string, data []byte) (string, error) {
	maxAttempts := c.MaxRetries
	if maxAttempts <= 0 {
		maxAttempts = 1
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		out, err := c.extractOnce(ctx, relPath, data)
		if err == nil {
			return out, nil
		}
		lastErr = err

		var providerErr *model.ProviderError
		if !errors.As(err, &providerErr) || !providerErr.Retryable || attempt == maxAttempts-1 {
			return "", err
		}

		backoff := c.backoffForAttempt(attempt)
		if waitErr := c.wait(ctx, backoff); waitErr != nil {
			return "", waitErr
		}
	}

	return "", lastErr
}

// extractOnce contains the previous implementation of Extract and performs a
// single attempt without any retry behaviour.  The logic is kept separate so
// that extractWithRetry can invoke it repeatedly.
// ocrMIMEType returns the MIME type for the given file extension, or ("", false)
// if the extension is not supported for OCR.
func ocrMIMEType(ext string) (string, bool) {
	switch ext {
	case ".pdf":
		return "application/pdf", true
	case ".png":
		return "image/png", true
	case ".jpg", ".jpeg":
		return "image/jpeg", true
	case ".webp":
		return "image/webp", true
	default:
		return "", false
	}
}

// extractOCRText assembles the text content from an OCR response, preferring
// page-level markdown when available.  Returns ("", false) when no text was
// found.
func extractOCRText(parsed ocrResponse) (string, bool) {
	if len(parsed.Pages) > 0 {
		parts := make([]string, 0, len(parsed.Pages))
		for _, p := range parsed.Pages {
			pageText := strings.TrimSpace(p.Markdown)
			if pageText == "" {
				pageText = strings.TrimSpace(p.Text)
			}
			if pageText == "" {
				continue
			}
			parts = append(parts, pageText)
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\f"), true
		}
	}
	if text := strings.TrimSpace(parsed.Markdown); text != "" {
		return text, true
	}
	if text := strings.TrimSpace(parsed.Text); text != "" {
		return text, true
	}
	return "", false
}

func (c *Client) extractOnce(ctx context.Context, relPath string, data []byte) (string, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return "", &model.ProviderError{
			Code:      "MISTRAL_AUTH",
			Message:   "missing Mistral API key",
			Retryable: false,
		}
	}
	if len(data) == 0 {
		return "", &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   "ocr input is empty",
			Retryable: false,
		}
	}

	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(relPath)))
	mimeType, ok := ocrMIMEType(ext)
	if !ok {
		// report supported file types explicitly; falling back to a generic MIME
		// value risks the upstream API rejecting the request and makes debugging
		// harder.
		return "", &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   fmt.Sprintf("unsupported file extension for OCR: %s", ext),
			Retryable: false,
		}
	}

	ocrModel := c.DefaultOCRModel
	if strings.TrimSpace(ocrModel) == "" {
		ocrModel = DefaultOCRModel
	}
	maxPayloadBytes := c.MaxOCRPayloadBytes
	if maxPayloadBytes <= 0 {
		maxPayloadBytes = defaultMaxOCRPayloadBytes
	}
	estimatedPayloadBytes := len("data:"+mimeType+";base64,") + base64.StdEncoding.EncodedLen(len(data))
	if estimatedPayloadBytes > maxPayloadBytes {
		return "", &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   fmt.Sprintf("ocr payload too large (%d bytes, limit %d): reduce file size or increase MaxOCRPayloadBytes", estimatedPayloadBytes, maxPayloadBytes),
			Retryable: false,
		}
	}
	payload := ocrRequest{
		Model: ocrModel,
		Document: ocrDocument{
			Type:        "document_url",
			DocumentURL: "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data),
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   "failed to marshal ocr request",
			Retryable: false,
			Cause:     err,
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/ocr", bytes.NewReader(body))
	if err != nil {
		return "", &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   "failed to build ocr request",
			Retryable: false,
			Cause:     err,
		}
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")

	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultRequestTimeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   "ocr request failed",
			Retryable: true,
			Cause:     err,
		}
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return "", mistralHTTPError(resp)
	}

	var parsed ocrResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   "failed to decode ocr response",
			Retryable: false,
			Cause:     err,
		}
	}

	text, ok := extractOCRText(parsed)
	if !ok {
		return "", &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   "ocr response had no text content",
			Retryable: false,
		}
	}
	return text, nil
}

func (c *Client) Transcribe(ctx context.Context, relPath string, data []byte) (string, error) {
	return c.transcribeWithRetry(ctx, relPath, data)
}

func (c *Client) Generate(ctx context.Context, prompt string) (string, error) {
	return c.generateWithRetry(ctx, prompt)
}

func (c *Client) transcribeWithRetry(ctx context.Context, relPath string, data []byte) (string, error) {
	// allow initial try plus MaxRetries retries, matching embedBatchWithRetry
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
			return "", err
		}

		backoff := c.backoffForAttempt(attempt)
		if waitErr := c.wait(ctx, backoff); waitErr != nil {
			return "", waitErr
		}
	}

	return "", lastErr
}

// buildTranscribeBody builds the multipart form body for a transcription
// request. Returns the raw body bytes and the writer (for the Content-Type
// boundary), or a ProviderError on failure.
func (c *Client) buildTranscribeBody(relPath string, data []byte) ([]byte, *multipart.Writer, error) {
	fileName := strings.TrimSpace(filepath.Base(relPath))
	if fileName == "" || fileName == "." || fileName == string(filepath.Separator) {
		fileName = "audio.wav"
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	fileField, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return nil, nil, &model.ProviderError{Code: "MISTRAL_FAILED", Message: "failed to build transcription request body", Retryable: false, Cause: err}
	}
	if _, err := fileField.Write(data); err != nil {
		return nil, nil, &model.ProviderError{Code: "MISTRAL_FAILED", Message: "failed to write transcription input", Retryable: false, Cause: err}
	}
	// model selection mirrors OCR logic: prefer the client-configured value
	// and only fall back to the package constant if the field is empty.
	modelName := strings.TrimSpace(c.DefaultTranscribeModel)
	if modelName == "" {
		modelName = DefaultTranscribeModel
	}
	if err := writer.WriteField("model", modelName); err != nil {
		return nil, nil, &model.ProviderError{Code: "MISTRAL_FAILED", Message: "failed to write transcription model", Retryable: false, Cause: err}
	}
	if language := strings.TrimSpace(c.DefaultTranscribeLanguage); language != "" {
		if err := writer.WriteField("language", language); err != nil {
			return nil, nil, &model.ProviderError{Code: "MISTRAL_FAILED", Message: "failed to write transcription language", Retryable: false, Cause: err}
		}
	}
	if err := writer.Close(); err != nil {
		return nil, nil, &model.ProviderError{Code: "MISTRAL_FAILED", Message: "failed to finalize transcription request body", Retryable: false, Cause: err}
	}
	return buf.Bytes(), writer, nil
}

// parseMistralTranscriptSegments converts timed segments in a transcription
// response into timestamped lines.  Returns ("", false) when no non-empty
// segments are present.
func parseMistralTranscriptSegments(parsed transcribeResponse) (string, bool) {
	if len(parsed.Segments) == 0 {
		return "", false
	}
	lines := make([]string, 0, len(parsed.Segments))
	for _, seg := range parsed.Segments {
		text := strings.TrimSpace(seg.Text)
		if text == "" {
			continue
		}
		startSec := int(seg.Start)
		mm := startSec / 60
		ss := startSec % 60
		lines = append(lines, "["+pad2(mm)+":"+pad2(ss)+"] "+text)
	}
	if len(lines) > 0 {
		return strings.Join(lines, "\n"), true
	}
	return "", false
}

func (c *Client) transcribeOnce(ctx context.Context, relPath string, data []byte) (string, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return "", &model.ProviderError{Code: "MISTRAL_AUTH", Message: "missing Mistral API key", Retryable: false}
	}
	if len(data) == 0 {
		return "", &model.ProviderError{Code: "MISTRAL_FAILED", Message: "transcription input is empty", Retryable: false}
	}
	// enforce a maximum payload size mirroring the OCR check so callers can
	// avoid sending absurdly large audio blobs.  use the same configuration
	// parameter for simplicity.
	maxPayload := c.MaxOCRPayloadBytes
	if maxPayload <= 0 {
		maxPayload = defaultMaxOCRPayloadBytes
	}
	if len(data) > maxPayload {
		return "", &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   fmt.Sprintf("transcription input too large (%d bytes, limit %d)", len(data), maxPayload),
			Retryable: false,
		}
	}

	body, writer, err := c.buildTranscribeBody(relPath, data)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/audio/transcriptions", bytes.NewReader(body))
	if err != nil {
		return "", &model.ProviderError{Code: "MISTRAL_FAILED", Message: "failed to build transcription request", Retryable: false, Cause: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultRequestTimeout}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", &model.ProviderError{Code: "MISTRAL_FAILED", Message: "transcription request failed", Retryable: true, Cause: err}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", mistralHTTPError(resp)
	}

	var parsed transcribeResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   "failed to decode transcription response",
			Retryable: false,
			Cause:     err,
		}
	}

	if segText, ok := parseMistralTranscriptSegments(parsed); ok {
		return segText, nil
	}

	text := strings.TrimSpace(parsed.Text)
	if text == "" {
		return "", &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   "transcription response had no text content",
			Retryable: false,
		}
	}
	return text, nil
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func (c *Client) generateWithRetry(ctx context.Context, prompt string) (string, error) {
	maxAttempts := c.MaxRetries + 1
	if maxAttempts <= 0 {
		maxAttempts = 1
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		out, err := c.generateOnce(ctx, prompt)
		if err == nil {
			return out, nil
		}
		lastErr = err

		var providerErr *model.ProviderError
		if !errors.As(err, &providerErr) || !providerErr.Retryable || attempt == maxAttempts-1 {
			return "", err
		}

		backoff := c.backoffForAttempt(attempt)
		if waitErr := c.wait(ctx, backoff); waitErr != nil {
			return "", waitErr
		}
	}

	return "", lastErr
}

func (c *Client) generateOnce(ctx context.Context, prompt string) (string, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return "", &model.ProviderError{
			Code:      "MISTRAL_AUTH",
			Message:   "missing Mistral API key",
			Retryable: false,
		}
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "", &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   "prompt is required",
			Retryable: false,
		}
	}

	// choose the chat model; allow caller override via Client field
	chatModel := DefaultChatModel
	if strings.TrimSpace(c.DefaultChatModel) != "" {
		chatModel = c.DefaultChatModel
	}
	reqPayload := generateRequest{
		Model: chatModel,
		Messages: []generateMessage{
			{Role: "user", Content: prompt},
		},
	}
	body, err := json.Marshal(reqPayload)
	if err != nil {
		return "", &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   "failed to marshal generation request",
			Retryable: false,
			Cause:     err,
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   "failed to build generation request",
			Retryable: false,
			Cause:     err,
		}
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")

	client := c.HTTPClient
	// generation requests may take longer than the standard default timeout.
	// apply GenerationTimeout (when configured) even if HTTPClient is already set.
	timeout := defaultRequestTimeout
	if c.GenerationTimeout > 0 {
		timeout = c.GenerationTimeout
	}
	if client == nil {
		client = &http.Client{Timeout: timeout}
	} else {
		client = cloneHTTPClientWithTimeout(client, timeout)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   "generation request failed",
			Retryable: true,
			Cause:     err,
		}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", mistralHTTPError(resp)
	}

	var parsed generateResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   "failed to decode generation response",
			Retryable: false,
			Cause:     err,
		}
	}
	reportUsage(ctx, usage.StageGenerate, parsed.Usage)
	if len(parsed.Choices) == 0 {
		return "", &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   "generation response had no choices",
			Retryable: false,
		}
	}
	text := strings.TrimSpace(contentToText(parsed.Choices[0].Message.Content))
	if text == "" {
		return "", &model.ProviderError{
			Code:      "MISTRAL_FAILED",
			Message:   "generation response had empty content",
			Retryable: false,
		}
	}
	return text, nil
}

func cloneHTTPClientWithTimeout(base *http.Client, timeout time.Duration) *http.Client {
	if base == nil {
		return &http.Client{Timeout: timeout}
	}
	cloned := *base
	cloned.Timeout = timeout
	return &cloned
}

func contentToText(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			switch typed := item.(type) {
			case map[string]interface{}:
				if text, ok := typed["text"].(string); ok && strings.TrimSpace(text) != "" {
					parts = append(parts, text)
				}
			case string:
				if strings.TrimSpace(typed) != "" {
					parts = append(parts, typed)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/ingest/docling"
)

const (
	// doclingServeConvertPath returns a structured DoclingDocument
	// (json_content) for an uploaded file.
	doclingServeConvertPath = "/v1/convert/file"
	// doclingServeReadyPath gates on model load; used by doctor probes.
	doclingServeReadyPath = "/health"
	// doclingServeTimeout is a generous backstop for conversions when the
	// caller's context carries no deadline.
	doclingServeTimeout = 10 * time.Minute
)

// doclingServeExtractor extracts structured documents by calling a docling-serve
// HTTP container instead of spawning the docling CLI. Output is byte-identical
// to the CLI path (same DoclingDocument -> Markdown + region spans), per spec
// 0.10.0 §7.4.B: the transport differs, the engine and result do not.
type doclingServeExtractor struct {
	baseURL string
	client  *http.Client
}

// NewDoclingServeExtractor returns an extractor backed by the docling-serve HTTP
// endpoint at baseURL (e.g. http://127.0.0.1:5001). A trailing slash is trimmed.
func NewDoclingServeExtractor(baseURL string) *doclingServeExtractor {
	return &doclingServeExtractor{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		client:  &http.Client{Timeout: doclingServeTimeout},
	}
}

// doclingServeResponse is the subset of the docling-serve convert response we
// consume: the structured DoclingDocument carried in document.json_content.
type doclingServeResponse struct {
	Document struct {
		JSONContent json.RawMessage `json:"json_content"`
	} `json:"document"`
	Status string `json:"status"`
}

// buildConvertRequest assembles the multipart upload requesting structured JSON
// (to_formats=json), which is what makes the output byte-compatible with the
// CLI `--to json` path.
func (d *doclingServeExtractor) buildConvertRequest(ctx context.Context, relPath string, data []byte) (*http.Request, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	filename := filepath.Base(relPath)
	if filename == "" || filename == "." || filename == string(filepath.Separator) {
		filename = "document"
	}
	part, err := mw.CreateFormFile("files", filename)
	if err != nil {
		return nil, fmt.Errorf("build docling-serve request: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return nil, fmt.Errorf("write docling-serve request: %w", err)
	}
	if err := mw.WriteField("to_formats", "json"); err != nil {
		return nil, fmt.Errorf("build docling-serve request: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("finalize docling-serve request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+doclingServeConvertPath, &body)
	if err != nil {
		return nil, fmt.Errorf("create docling-serve request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// convert uploads data to docling-serve and returns the raw DoclingDocument
// JSON (the same bytes the CLI `--to json` path emits on stdout).
func (d *doclingServeExtractor) convert(ctx context.Context, relPath string, data []byte) ([]byte, error) {
	if d.baseURL == "" {
		return nil, fmt.Errorf("docling-serve endpoint is not configured")
	}
	req, err := d.buildConvertRequest(ctx, relPath, data)
	if err != nil {
		return nil, err
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docling-serve request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read one byte past the limit so an over-size response surfaces as a clear
	// "too large" error rather than a confusing truncated-JSON decode failure.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxDoclingStdoutBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read docling-serve response: %w", err)
	}
	if len(payload) > maxDoclingStdoutBytes {
		return nil, fmt.Errorf("docling-serve response exceeded %d bytes", maxDoclingStdoutBytes)
	}
	if resp.StatusCode != http.StatusOK {
		// Deliberately do NOT echo the response body: this error is persisted as
		// Document.ErrorMessage, and an untrusted body could carry document
		// fragments or credential-bearing routing details.
		return nil, fmt.Errorf("docling-serve returned HTTP %d", resp.StatusCode)
	}

	var parsed doclingServeResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, fmt.Errorf("decode docling-serve response: %w", err)
	}
	if len(parsed.Document.JSONContent) == 0 || string(parsed.Document.JSONContent) == "null" {
		return nil, fmt.Errorf("docling-serve response missing json_content (status %q)", parsed.Status)
	}
	return parsed.Document.JSONContent, nil
}

// Extract returns the rendered Markdown representation.
func (d *doclingServeExtractor) Extract(ctx context.Context, relPath string, data []byte) (string, error) {
	res, err := d.ExtractStructured(ctx, relPath, data)
	if err != nil {
		return "", err
	}
	return res.Markdown, nil
}

// ExtractStructured uploads the file to docling-serve and parses the returned
// DoclingDocument exactly as the CLI path does, so the two transports yield the
// same Markdown, blocks, and title.
func (d *doclingServeExtractor) ExtractStructured(ctx context.Context, relPath string, data []byte) (StructuredExtraction, error) {
	raw, err := d.convert(ctx, relPath, data)
	if err != nil {
		return StructuredExtraction{}, err
	}
	doc, err := docling.Parse(raw)
	if err != nil {
		return StructuredExtraction{}, fmt.Errorf("parse structured docling-serve output for %s: %w", relPath, err)
	}
	blocks := doc.Linearize()
	return StructuredExtraction{
		Markdown: docling.RenderMarkdown(blocks),
		Blocks:   blocks,
		Title:    doc.Title(),
	}, nil
}

// ProbeDoclingServe checks that a docling-serve endpoint is reachable and
// healthy, for diagnostics (the daemon-side `dir2mcp doctor`). It returns nil
// when the endpoint responds 200 on its health path, otherwise a descriptive
// error. Per spec 0.10.0 §7.4.B/§7.7 an unreachable endpoint is a disabled
// extractor, so callers surface a probe failure as a warning, not a hard error.
func ProbeDoclingServe(ctx context.Context, baseURL string) error {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return fmt.Errorf("endpoint is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+doclingServeReadyPath, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// SanitizeServeURL reduces a docling-serve endpoint to scheme://host/path,
// dropping any userinfo, query, or fragment. Used before the endpoint is
// written to diagnostic surfaces (extraction metadata, the doctor report) so a
// URL carrying credentials or signed routing params never becomes durable or
// logged. Returns "" when the URL cannot be parsed.
func SanitizeServeURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + u.Path
}

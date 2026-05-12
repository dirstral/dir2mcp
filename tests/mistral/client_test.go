package tests

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/mistral"
	"github.com/dirstral/dir2mcp/internal/model"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func newJSONResponse(status int, body string) *http.Response {
	r := &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		// mimic real responses by including a minimal request
		Request: &http.Request{
			Method: http.MethodGet,
			URL:    &url.URL{Scheme: "https", Host: "api.example.com", Path: "/"},
		},
	}
	// typical JSON API responses set this header
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestExtract_OCRPagesJoinedByFormFeed(t *testing.T) {
	clientErr := false
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/ocr" {
			clientErr = true
			t.Errorf("unexpected path: %s", r.URL.Path)
			return newJSONResponse(http.StatusBadRequest, ""), nil
		}
		if r.Method != http.MethodPost {
			clientErr = true
			t.Errorf("unexpected method: %s", r.Method)
			return newJSONResponse(http.StatusBadRequest, ""), nil
		}
		if got := r.Header.Get("Authorization"); got != "Bearer key" {
			clientErr = true
			t.Errorf("unexpected auth header: %q", got)
			return newJSONResponse(http.StatusBadRequest, ""), nil
		}

		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			clientErr = true
			t.Errorf("decode request: %v", err)
			return newJSONResponse(http.StatusBadRequest, ""), nil
		}
		if req["model"] != mistral.DefaultOCRModel {
			clientErr = true
			t.Errorf("unexpected model: %#v", req["model"])
			return newJSONResponse(http.StatusBadRequest, ""), nil
		}

		return newJSONResponse(http.StatusOK, `{"pages":[{"markdown":"first page"},{"markdown":"second page"}]}`), nil
	})

	c := mistral.NewClient("https://api.mistral.ai", "key")
	c.HTTPClient = &http.Client{Transport: rt}
	got, err := c.Extract(context.Background(), "docs/file.pdf", []byte("pdf-bytes"))
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}
	if got != "first page\fsecond page" {
		t.Fatalf("unexpected ocr text: %q", got)
	}
	if clientErr {
		t.Fatalf("handler encountered errors, see previous logs")
	}
}

func TestExtract_MapsUnauthorizedToProviderAuthError(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return newJSONResponse(http.StatusUnauthorized, "unauthorized"), nil
	})
	c := mistral.NewClient("https://api.mistral.ai", "key")
	c.HTTPClient = &http.Client{Transport: rt}
	_, err := c.Extract(context.Background(), "scan.png", []byte("img"))
	if err == nil {
		t.Fatalf("expected error")
	}
	var pe *model.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("expected provider error type")
	}
	if pe.Code != "MISTRAL_AUTH" {
		t.Fatalf("expected MISTRAL_AUTH, got %s", pe.Code)
	}
}

func TestExtract_MissingAPIKey(t *testing.T) {
	c := mistral.NewClient("http://example.test", "")
	_, err := c.Extract(context.Background(), "a.pdf", []byte("x"))
	if err == nil {
		t.Fatalf("expected error")
	}
	var pe *model.ProviderError
	if !errors.As(err, &pe) || pe.Code != "MISTRAL_AUTH" {
		t.Fatalf("expected provider auth error, got: %v", err)
	}
}

func TestExtract_UnsupportedExtension(t *testing.T) {
	c := mistral.NewClient("http://example.test", "key")
	_, err := c.Extract(context.Background(), "file.txt", []byte("data"))
	if err == nil {
		t.Fatalf("expected error for unsupported extension")
	}
	var pe *model.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("expected provider error type, got %T", err)
	}
	if pe.Retryable {
		t.Fatalf("error for unsupported extension should not be retryable")
	}
	if !strings.Contains(pe.Message, "unsupported file extension") {
		t.Fatalf("unexpected message: %s", pe.Message)
	}
}

func TestExtract_RetryableErrorsAreRetried(t *testing.T) {
	attempts := 0
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		if attempts <= 2 {
			return newJSONResponse(http.StatusTooManyRequests, "rate limited"), nil
		}
		// succeed on third try
		return newJSONResponse(http.StatusOK, `{"pages":[{"markdown":"ok"}]}`), nil
	})

	c := mistral.NewClient("https://api.mistral.ai", "key")
	c.HTTPClient = &http.Client{Transport: rt}
	c.MaxRetries = 3
	ctx := context.Background()
	out, err := c.Extract(ctx, "file.pdf", []byte("data"))
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if out != "ok" {
		t.Fatalf("unexpected output: %q", out)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
}

func TestExtract_RetryStopsOnNonRetryableOrMax(t *testing.T) {
	attempts := 0
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		if attempts <= 2 {
			return newJSONResponse(http.StatusUnauthorized, "auth"), nil
		}
		return newJSONResponse(http.StatusOK, `{"pages":[{"markdown":"ok"}]}`), nil
	})

	c := mistral.NewClient("https://api.mistral.ai", "key")
	c.HTTPClient = &http.Client{Transport: rt}
	c.MaxRetries = 5
	_, err := c.Extract(context.Background(), "file.pdf", []byte("data"))
	if err == nil {
		t.Fatalf("expected error due to auth failure")
	}
	if attempts != 1 {
		t.Fatalf("expected only 1 attempt for non-retryable error, got %d", attempts)
	}
}

func TestExtract_PayloadTooLargeFailsFast(t *testing.T) {
	attempts := 0
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		return newJSONResponse(http.StatusOK, `{"pages":[{"markdown":"ok"}]}`), nil
	})

	c := mistral.NewClient("https://api.mistral.ai", "key")
	c.HTTPClient = &http.Client{Transport: rt}
	c.MaxOCRPayloadBytes = 64

	_, err := c.Extract(context.Background(), "file.pdf", []byte(strings.Repeat("a", 128)))
	if err == nil {
		t.Fatalf("expected payload too large error")
	}
	var pe *model.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("expected provider error, got %T", err)
	}
	if pe.Retryable {
		t.Fatalf("payload limit error should be non-retryable")
	}
	if !strings.Contains(pe.Message, "payload too large") {
		t.Fatalf("unexpected error message: %s", pe.Message)
	}
	if attempts != 0 {
		t.Fatalf("expected no HTTP attempts for preflight payload failure, got %d", attempts)
	}
}

// Verify that supplying a custom model string to the client actually changes
// the value sent in the OCR payload.  Regression against the earlier hardcode.
func TestExtract_CustomModel(t *testing.T) {
	testModel := "my-custom-ocr"

	clientErr := false
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			clientErr = true
			t.Errorf("decode request: %v", err)
			return newJSONResponse(http.StatusBadRequest, ""), nil
		}
		if req["model"] != testModel {
			clientErr = true
			t.Errorf("expected model %q, got %v", testModel, req["model"])
			return newJSONResponse(http.StatusBadRequest, ""), nil
		}
		return newJSONResponse(http.StatusOK, `{"pages":[{"markdown":"ok"}]}`), nil
	})

	c := mistral.NewClient("https://api.mistral.ai", "key")
	c.HTTPClient = &http.Client{Transport: rt}
	c.DefaultOCRModel = testModel
	if _, err := c.Extract(context.Background(), "x.pdf", []byte("data")); err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}
	if clientErr {
		t.Fatalf("handler encountered errors, see previous logs")
	}
}

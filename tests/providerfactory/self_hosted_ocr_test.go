package tests

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/provider"
	"github.com/dirstral/dir2mcp/internal/providerfactory"
)

// TestSelfHostedOCR_EndToEnd drives the OCR adapter built by the factory from a
// bespoke-OCR profile (kind:mistral) whose base_url points at a fake
// self-hosted endpoint (dir2mcp#240 / SPEC §8.5). It asserts the request lands
// on the bespoke /v1/ocr route with the profile's configured model, that the
// custom base_url is honored (not the hosted SaaS default), and that a generic
// page-markdown response decodes to extracted text. No network, no credential.
func TestSelfHostedOCR_EndToEnd(t *testing.T) {
	var (
		gotPath   string
		gotModel  string
		gotAuth   string
		gotMethod string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if m, ok := body["model"].(string); ok {
			gotModel = m
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"pages":[{"markdown":"hello"},{"markdown":"world"}]}`)
	}))
	defer srv.Close()

	// A self-hosted bespoke-OCR profile: kind:mistral on the fake base_url,
	// with a custom OCR model name. The bespoke OCR client appends the
	// /v1/ocr route itself, so base_url is the host ROOT (not a /v1 path) —
	// unlike a kind:openai base_url which already includes /v1.
	p := provider.Profile{
		Name:     "gpu-ocr",
		Kind:     provider.KindMistral,
		BaseURL:  srv.URL,
		OCRModel: "my-self-hosted-ocr",
		APIKey:   "k", // mistral client requires a key; trusted-network deployments still set one
	}

	ocr, err := providerfactory.OCR(p)
	if err != nil {
		t.Fatalf("build OCR adapter for self-hosted profile: %v", err)
	}

	out, err := ocr.Extract(context.Background(), "scan.pdf", []byte("%PDF-1.4 bytes"))
	if err != nil {
		t.Fatalf("Extract against self-hosted OCR endpoint: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/ocr" {
		t.Errorf("path = %q, want /v1/ocr (the bespoke OCR route on the self-hosted base_url)", gotPath)
	}
	if gotModel != "my-self-hosted-ocr" {
		t.Errorf("model = %q, want the profile's ocr_model my-self-hosted-ocr", gotModel)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("auth header = %q, want Bearer k", gotAuth)
	}
	if !strings.Contains(out, "hello") || !strings.Contains(out, "world") {
		t.Fatalf("extracted text = %q, want both page markdowns", out)
	}
}

// TestSelfHostedOCR_FactoryRejectsOpenAIKind pins SPEC §8.5 at the factory
// boundary: there is no OpenAI-compatible OCR analog, so building an OCR
// adapter from a kind:openai self-hosted profile must error rather than
// silently mis-route to /chat/completions.
func TestSelfHostedOCR_FactoryRejectsOpenAIKind(t *testing.T) {
	p := provider.Profile{
		Name:           "gpu-openai",
		Kind:           provider.KindOpenAI,
		BaseURL:        "http://gpu-vps:8080/v1",
		CredentialLess: true,
	}
	if _, err := providerfactory.OCR(p); err == nil {
		t.Fatal("OCR(kind:openai) must error (SPEC §8.5: OCR has no OpenAI analog)")
	}
}

package tests

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
)

// loadYAMLConfig writes yaml to a temp .dir2mcp.yaml and loads it, so the
// dynamic providers:/model: subtree (which carries model.ocr.provider) is
// parsed — config.Config{} literals never populate it.
func loadYAMLConfig(t *testing.T, yaml string) config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.LoadFile(p)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	return cfg
}

// TestSelfHostedOCR_IngestHonorsModelOCRProvider is the end-to-end ingest gap
// for dir2mcp#240: with ingest.extractor=mistral and model.ocr.provider bound
// to a self-hosted bespoke-OCR profile (kind:mistral on a custom base_url), the
// extractor that ingest builds must call THAT endpoint — not the built-in
// mistral-ocr SaaS default. Uses a fake OpenAI-compatible /v1/ocr server.
func TestSelfHostedOCR_IngestHonorsModelOCRProvider(t *testing.T) {
	var gotPath, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if m, ok := body["model"].(string); ok {
			gotModel = m
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"pages":[{"markdown":"self-hosted page"}]}`)
	}))
	defer srv.Close()

	yaml := "" +
		"ingest:\n" +
		"  extractor: mistral\n" +
		"providers:\n" +
		"  gpu-ocr:\n" +
		"    kind: mistral\n" +
		// bespoke OCR appends /v1/ocr itself, so base_url is the host root
		"    base_url: " + srv.URL + "\n" +
		"    api_key: local-key\n" +
		"    ocr_model: self-hosted-ocr\n" +
		"model:\n" +
		"  ocr:\n" +
		"    provider: gpu-ocr\n"
	cfg := loadYAMLConfig(t, yaml)

	ex := ingest.DocumentExtractorFromConfig(cfg)
	if ex == nil {
		t.Fatal("expected an extractor from the self-hosted OCR binding")
	}
	out, err := ex.Extract(context.Background(), "scan.pdf", []byte("%PDF bytes"))
	if err != nil {
		t.Fatalf("Extract via self-hosted OCR: %v", err)
	}
	if gotPath != "/v1/ocr" {
		t.Errorf("path = %q, want /v1/ocr on the self-hosted endpoint", gotPath)
	}
	if gotModel != "self-hosted-ocr" {
		t.Errorf("model = %q, want the bound ocr_model self-hosted-ocr", gotModel)
	}
	if !strings.Contains(out, "self-hosted page") {
		t.Fatalf("extracted text = %q", out)
	}
}

// TestSelfHostedOCR_IngestFallsBackToMistralOCRWhenUnset confirms the change is
// non-regressive: with no model.ocr.provider binding, ingest.extractor=mistral
// still resolves the built-in mistral-ocr profile (the historical default).
func TestSelfHostedOCR_IngestFallsBackToMistralOCRWhenUnset(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "test-key")
	d := ingest.DescribeDocumentExtractor(config.Config{IngestExtractor: "mistral"})
	if d.Name != "mistral-ocr" || d.Source != "explicit" {
		t.Fatalf("unset binding should select built-in mistral-ocr: name=%q source=%q reason=%q",
			d.Name, d.Source, d.Reason)
	}
}

// TestSelfHostedOCR_IngestDisabledWhenBoundToOpenAIKind pins SPEC §8.5 at the
// ingest layer: binding model.ocr.provider to a kind:openai profile yields no
// extractor (OCR has no OpenAI analog), rather than silently mis-routing.
func TestSelfHostedOCR_IngestDisabledWhenBoundToOpenAIKind(t *testing.T) {
	yaml := "" +
		"ingest:\n" +
		"  extractor: mistral\n" +
		"providers:\n" +
		"  gpu-openai:\n" +
		"    kind: openai\n" +
		"    base_url: http://gpu-vps:8080/v1\n" +
		"model:\n" +
		"  ocr:\n" +
		"    provider: gpu-openai\n"
	cfg := loadYAMLConfig(t, yaml)
	if ex := ingest.DocumentExtractorFromConfig(cfg); ex != nil {
		t.Fatal("kind:openai OCR binding must yield no extractor (SPEC §8.5)")
	}
	d := ingest.DescribeDocumentExtractor(cfg)
	if d.Name != "" || d.Source != "disabled" {
		t.Fatalf("kind:openai OCR binding should be disabled: name=%q source=%q", d.Name, d.Source)
	}
}

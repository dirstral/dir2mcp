package tests

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/mistral"
)

func TestExtract_Integration_MistralOCR(t *testing.T) {
	if os.Getenv("RUN_INTEGRATION_TESTS") != "1" {
		t.Skip("set RUN_INTEGRATION_TESTS=1 to run integration tests")
	}

	apiKey := strings.TrimSpace(os.Getenv("MISTRAL_API_KEY"))
	if apiKey == "" {
		t.Skip("MISTRAL_API_KEY is not set")
	}

	samplePath := strings.TrimSpace(os.Getenv("MISTRAL_OCR_SAMPLE"))
	if samplePath == "" {
		t.Skip("MISTRAL_OCR_SAMPLE is not set (path to local .pdf/.png/.jpg sample)")
	}

	data, err := os.ReadFile(samplePath)
	if err != nil {
		t.Fatalf("read OCR sample %s: %v", samplePath, err)
	}
	if len(data) == 0 {
		t.Fatalf("OCR sample %s is empty", samplePath)
	}

	baseURL := strings.TrimSpace(os.Getenv("MISTRAL_BASE_URL"))
	client := mistral.NewClient(baseURL, apiKey)

	ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout())
	defer cancel()

	out, err := client.Extract(ctx, filepath.Base(samplePath), data)
	if err != nil {
		t.Fatalf("Extract returned error: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatalf("Extract returned empty OCR content")
	}

	// Response shape contract: OCR pages are joined using form-feed separators.
	parts := strings.Split(out, "\f")
	for i, part := range parts {
		if strings.TrimSpace(part) == "" {
			t.Fatalf("OCR output contains empty page segment at index %d", i)
		}
	}
}

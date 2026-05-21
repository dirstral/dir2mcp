package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

func TestRoutingDecisions_NoCredsFallsBackToMistralOCR(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("PATH", t.TempDir()) // no docling on PATH

	cfg := config.Default()
	rows := routingDecisions(cfg)

	if got := findRow(rows, "OCR"); got == nil {
		t.Fatalf("expected OCR row, got rows=%+v", rows)
	} else {
		if got.Provider != "disabled" {
			t.Errorf("OCR provider = %q, want disabled (no MISTRAL_API_KEY, no docling)", got.Provider)
		}
		if !strings.Contains(got.Reason, "no extractor available") {
			t.Errorf("OCR reason = %q, want substring 'no extractor available'", got.Reason)
		}
	}
}

func TestRoutingDecisions_MistralKeyTriggersOCRFallback(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("PATH", t.TempDir()) // no docling on PATH

	cfg := config.Default()
	rows := routingDecisions(cfg)

	got := findRow(rows, "OCR")
	if got == nil {
		t.Fatalf("expected OCR row, got rows=%+v", rows)
	}
	if got.Provider != "mistral-ocr" {
		t.Errorf("OCR provider = %q, want mistral-ocr", got.Provider)
	}
	if !strings.Contains(got.Reason, "docling not found") {
		t.Errorf("OCR reason = %q, want fallback explanation", got.Reason)
	}

	if embed := findRow(rows, "Embed"); embed == nil || embed.Provider != "mistral" {
		t.Errorf("Embed row = %+v, want provider mistral", embed)
	}
}

func TestPrintRoutingSection_OmittedWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	printRoutingSection(&buf, styles{}, []routingRow{
		{Capability: "Embed", Provider: "unavailable"},
		{Capability: "Chat", Provider: "unavailable"},
		{Capability: "OCR", Provider: "unavailable"},
	})
	if buf.Len() != 0 {
		t.Fatalf("expected empty output when no row has content, got %q", buf.String())
	}
}

func TestPrintRoutingSection_RendersReason(t *testing.T) {
	var buf bytes.Buffer
	printRoutingSection(&buf, styles{}, []routingRow{
		{Capability: "OCR", Provider: "mistral-ocr", Reason: "docling not found on PATH"},
	})
	out := buf.String()
	if !strings.Contains(out, "Models") {
		t.Errorf("expected section header, got %q", out)
	}
	if !strings.Contains(out, "mistral-ocr") {
		t.Errorf("expected provider name, got %q", out)
	}
	if !strings.Contains(out, "docling not found on PATH") {
		t.Errorf("expected reason text, got %q", out)
	}
}

func findRow(rows []routingRow, cap string) *routingRow {
	for i := range rows {
		if rows[i].Capability == cap {
			return &rows[i]
		}
	}
	return nil
}

package tests

import (
	"testing"

	"dir2mcp/internal/config"
	"dir2mcp/internal/ingest"
)

func TestResolveProvider_Native(t *testing.T) {
	cfg := config.Default()
	cfg.IngestProvider = "native"

	got := ingest.ResolveProvider(cfg)
	if got.Selected != ingest.IngestProviderNative {
		t.Fatalf("Selected=%q want=%q", got.Selected, ingest.IngestProviderNative)
	}
	if got.FallbackReason != "" {
		t.Fatalf("FallbackReason=%q want empty", got.FallbackReason)
	}
}

func TestResolveProvider_DoclingMissingFallsBackToNative(t *testing.T) {
	cfg := config.Default()
	cfg.IngestProvider = "docling"
	cfg.IngestDoclingCommand = "this-docling-command-does-not-exist"

	got := ingest.ResolveProvider(cfg)
	if got.Selected != ingest.IngestProviderNative {
		t.Fatalf("Selected=%q want native fallback", got.Selected)
	}
	if got.FallbackReason == "" {
		t.Fatal("expected fallback reason when docling is missing")
	}
}

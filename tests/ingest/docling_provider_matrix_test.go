package tests

import (
	"os/exec"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
)

// These cases pin the provider-selection / fallback decision matrix that #145
// asks CI to validate, using only deterministic seams (no docling binary, no
// network, no real Mistral key). Endpoint-reachability and functional-check
// permutations are covered in docling_extractor_test.go and
// docling_functional_check_test.go; this file pins the mode-dispatch and the
// "off" / "no provider at all" edges that those suites do not.

// TestDescribeDocumentExtractor_OffDisables pins that ingest.extractor=off
// disables extraction outright, even when a Mistral credential is present — the
// explicit kill switch must win over any auto-detected provider.
func TestDescribeDocumentExtractor_OffDisables(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "fake-key")
	d := ingest.DescribeDocumentExtractor(config.Config{IngestExtractor: "off"})
	if d.Name != "" || d.Source != "disabled" {
		t.Fatalf("off should disable extraction: name=%q source=%q (%s)", d.Name, d.Source, d.Reason)
	}
}

// TestDocumentExtractorFromConfig_OffYieldsNoExtractor pins that the kill switch
// is honoured by the builder too (lockstep with DescribeDocumentExtractor), so
// no extractor is constructed when extraction is turned off.
func TestDocumentExtractorFromConfig_OffYieldsNoExtractor(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "fake-key")
	if ex := ingest.DocumentExtractorFromConfig(config.Config{IngestExtractor: "off"}); ex != nil {
		t.Fatal("ingest.extractor=off must yield no extractor")
	}
}

// TestDescribeDocumentExtractor_MistralExplicitNoCredentialDisabled pins that
// explicitly requesting Mistral OCR with no credential disables extraction with
// an explanatory reason — it must NOT silently fall through to another engine.
func TestDescribeDocumentExtractor_MistralExplicitNoCredentialDisabled(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "")
	d := ingest.DescribeDocumentExtractor(config.Config{IngestExtractor: "mistral"})
	if d.Name != "" || d.Source != "disabled" {
		t.Fatalf("explicit mistral without credential should be disabled: name=%q source=%q (%s)", d.Name, d.Source, d.Reason)
	}
}

// TestDescribeDocumentExtractor_AutoNoProvidersDisabled pins the bottom of the
// auto cascade: no docling CLI, no serve URL, and no Mistral credential leaves
// no extractor available (rather than failing every document at ingest time).
func TestDescribeDocumentExtractor_AutoNoProvidersDisabled(t *testing.T) {
	if _, err := exec.LookPath("docling"); err == nil {
		t.Skip("docling CLI present on PATH; auto would select it")
	}
	t.Setenv("MISTRAL_API_KEY", "")
	d := ingest.DescribeDocumentExtractor(config.Config{IngestExtractor: "auto"})
	if d.Name != "" || d.Source != "disabled" {
		t.Fatalf("auto with no providers should be disabled: name=%q source=%q (%s)", d.Name, d.Source, d.Reason)
	}
}

// TestDescribeDocumentExtractor_UnknownModeFallsToAuto pins that an unrecognised
// ingest.extractor value is treated as auto (forward-compatible default) rather
// than hard-failing config load.
func TestDescribeDocumentExtractor_UnknownModeFallsToAuto(t *testing.T) {
	if _, err := exec.LookPath("docling"); err == nil {
		t.Skip("docling CLI present on PATH; auto would select it")
	}
	t.Setenv("MISTRAL_API_KEY", "fake-key")
	d := ingest.DescribeDocumentExtractor(config.Config{IngestExtractor: "totally-unknown-mode"})
	// With no docling but a Mistral credential, the auto cascade falls back to
	// Mistral OCR — proving the unknown mode routed through the auto branch.
	if d.Name != "mistral-ocr" || d.Source != "fallback" {
		t.Fatalf("unknown mode should route through auto: name=%q source=%q (%s)", d.Name, d.Source, d.Reason)
	}
}

// TestDescribeAndBuild_Lockstep is a light cross-check that the diagnostic
// describe path and the builder agree on availability across the representative
// modes (#145: the banner must reflect what the runtime actually runs). For
// "disabled" decisions the builder must return nil; for selected ones it must
// return a non-nil extractor.
func TestDescribeAndBuild_Lockstep(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "")
	cases := []config.Config{
		{IngestExtractor: "off"},
		{IngestExtractor: "mistral"},       // no credential -> disabled
		{IngestExtractor: "docling-serve"}, // no URL -> disabled
	}
	for _, cfg := range cases {
		d := ingest.DescribeDocumentExtractor(cfg)
		ex := ingest.DocumentExtractorFromConfig(cfg)
		if d.Source == "disabled" && ex != nil {
			t.Errorf("mode %q: describe=disabled but builder returned an extractor", cfg.IngestExtractor)
		}
		if d.Source != "disabled" && ex == nil {
			t.Errorf("mode %q: describe=%s/%s but builder returned nil", cfg.IngestExtractor, d.Name, d.Source)
		}
	}
}

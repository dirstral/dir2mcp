package tests

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
)

// These tests pin ingest.DescribePandocEngine, the SPEC §7.7 engine-list entry
// for the capability-activated pandoc engine (T2, #393). The startup banner lists
// every engine the ingest.extractor policy makes ELIGIBLE, with its reason when
// unavailable (#395); DescribeDocumentExtractor names only the primary of the
// cascade, so without this describe a missing pandoc was invisible at startup
// even though its absence is what leaves .odt/.rtf/.epub uncovered.

// Under auto, a functional pandoc is the secondary engine: pandoc/auto with the
// redacted resolution reason (never the command or path itself).
func TestDescribePandocEngine_AutoAvailable(t *testing.T) {
	stub := writePandocStub(t, "pandoc", "exit 0")
	d := ingest.DescribePandocEngine(config.Config{IngestExtractor: "auto", IngestPandocCommand: stub})
	if d.Name != "pandoc" || d.Source != "auto" {
		t.Fatalf("describe = %s/%s, want pandoc/auto (reason %q)", d.Name, d.Source, d.Reason)
	}
	if d.Reason != "configured pandoc command" {
		t.Errorf("reason = %q, want the redacted resolution reason", d.Reason)
	}
	if strings.Contains(d.Reason, stub) {
		t.Errorf("reason leaks the configured command path: %q", d.Reason)
	}
}

// Under auto, a pandoc that resolves but fails `--version` is unavailable with a
// reason that says so (SPEC §7.4.B: present-but-broken is never "available").
func TestDescribePandocEngine_AutoBrokenBinary(t *testing.T) {
	stub := writePandocStub(t, "pandoc", "exit 1")
	d := ingest.DescribePandocEngine(config.Config{IngestExtractor: "auto", IngestPandocCommand: stub})
	if d.Name != "" || d.Source != "disabled" {
		t.Fatalf("describe = %q/%s, want \"\"/disabled for a broken pandoc (reason %q)", d.Name, d.Source, d.Reason)
	}
	if !strings.Contains(d.Reason, "failed its functional check") {
		t.Errorf("reason = %q, want it to name the failed functional check", d.Reason)
	}
}

// Under auto with no pandoc anywhere, the engine is unavailable and the reason
// names the fix surface (PATH).
func TestDescribePandocEngine_AutoNotFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := exec.LookPath("pandoc"); err == nil {
		t.Fatal("test precondition: pandoc must not resolve on the empty PATH")
	}
	d := ingest.DescribePandocEngine(config.Config{IngestExtractor: "auto"})
	if d.Name != "" || d.Source != "disabled" {
		t.Fatalf("describe = %q/%s, want \"\"/disabled (reason %q)", d.Name, d.Source, d.Reason)
	}
	if d.Reason != "pandoc not found on PATH" {
		t.Errorf("reason = %q, want %q", d.Reason, "pandoc not found on PATH")
	}
}

// An empty policy is auto (the config default), so the engine is eligible.
func TestDescribePandocEngine_EmptyPolicyIsAuto(t *testing.T) {
	stub := writePandocStub(t, "pandoc", "exit 0")
	d := ingest.DescribePandocEngine(config.Config{IngestPandocCommand: stub})
	if d.Name != "pandoc" || d.Source != "auto" {
		t.Fatalf("describe = %s/%s, want pandoc/auto under the empty (default) policy", d.Name, d.Source)
	}
}

// A pin to another engine (or off) makes pandoc ineligible: the policy never
// activates it, so nothing is reported as unavailable. The banner omits the row.
func TestDescribePandocEngine_IneligibleUnderOtherPins(t *testing.T) {
	stub := writePandocStub(t, "pandoc", "exit 0") // available, yet still ineligible
	for _, policy := range []string{"docling", "docling-serve", "mistral", "off", " Mistral "} {
		d := ingest.DescribePandocEngine(config.Config{IngestExtractor: policy, IngestPandocCommand: stub})
		if d.Source != "ineligible" || d.Name != "" {
			t.Errorf("policy %q: describe = %q/%s, want \"\"/ineligible", policy, d.Name, d.Source)
		}
		if !strings.Contains(d.Reason, "ingest.extractor="+strings.ToLower(strings.TrimSpace(policy))) {
			t.Errorf("policy %q: reason = %q, want it to name the pin", policy, d.Reason)
		}
	}
}

// Under the pandoc pin the describe is the explicit primary decision, so the
// banner's OCR row and this describe can never disagree.
func TestDescribePandocEngine_PinMatchesPrimary(t *testing.T) {
	stub := writePandocStub(t, "pandoc", "exit 0")
	cfg := config.Config{IngestExtractor: "pandoc", IngestPandocCommand: stub}
	got := ingest.DescribePandocEngine(cfg)
	want := ingest.DescribeDocumentExtractor(cfg)
	if got != want {
		t.Fatalf("pandoc pin: DescribePandocEngine = %+v, DescribeDocumentExtractor = %+v; they must agree", got, want)
	}
	if got.Name != "pandoc" || got.Source != "explicit" {
		t.Fatalf("pandoc pin describe = %s/%s, want pandoc/explicit", got.Name, got.Source)
	}
}

package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// writePandocStub writes an executable script named exactly `name` into a fresh
// temp dir and returns its absolute path. body is the shell body (after the
// shebang). Naming it "pandoc" makes looksLikePandoc true so the functional check
// actually probes it; any other name is treated as a wrapper (not probed).
func writePandocStub(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return p
}

func TestResolvePandocBinary_CommandBeatsPath(t *testing.T) {
	stub := writePandocStub(t, "pandoc", "exit 0")
	bin, source, ok := resolvePandocBinary(config.Config{IngestPandocCommand: stub + " --extra"})
	if !ok || bin != stub || source != "command" {
		t.Fatalf("resolvePandocBinary(command) = (%q,%q,%v), want (%q,command,true)", bin, source, ok, stub)
	}
	if got := pandocResolvedReason(source); got != "configured pandoc command" {
		t.Errorf("reason = %q, want configured pandoc command", got)
	}
}

func TestResolvePandocBinary_FallsBackToPath(t *testing.T) {
	// Prepend a temp dir containing a `pandoc` to PATH so resolution is
	// deterministic regardless of whether the host has a real pandoc.
	stubDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubDir, "pandoc"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	bin, source, ok := resolvePandocBinary(config.Config{})
	if !ok || source != "path" || filepath.Base(bin) != "pandoc" {
		t.Fatalf("resolvePandocBinary(path) = (%q,%q,%v), want a PATH pandoc", bin, source, ok)
	}
	if got := pandocResolvedReason(source); got != "auto-detected on PATH" {
		t.Errorf("reason = %q, want auto-detected on PATH", got)
	}
}

func TestPandocFunctionalCheck_MemoizedOnce(t *testing.T) {
	countFile := filepath.Join(t.TempDir(), "count")
	// Each invocation appends a byte to countFile; a memoized probe runs it once.
	stub := writePandocStub(t, "pandoc", "printf x >> "+countFile+"\nexit 0")
	for i := 0; i < 3; i++ {
		if err := pandocFunctionalCheck(context.Background(), stub); err != nil {
			t.Fatalf("functional check err: %v", err)
		}
	}
	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("read count: %v", err)
	}
	if len(data) != 1 {
		t.Errorf("probe ran %d times, want exactly 1 (memoized)", len(data))
	}
}

func TestPandocFunctionalCheck_NonFunctionalFails(t *testing.T) {
	stub := writePandocStub(t, "pandoc", "exit 1")
	if err := pandocFunctionalCheck(context.Background(), stub); err == nil {
		t.Error("a pandoc that exits non-zero on --version must fail the functional check")
	}
	if pandocAvailable(config.Config{IngestPandocCommand: stub}) {
		t.Error("a non-functional pandoc must report unavailable")
	}
}

func TestPandocFunctionalCheck_WrapperNotProbed(t *testing.T) {
	// A command whose basename is not "pandoc" is a wrapper: it is NOT probed, so
	// even a non-zero-exiting binary keeps "resolvable == available".
	wrapper := writePandocStub(t, "my-pandoc-wrapper", "exit 3")
	if err := pandocFunctionalCheck(context.Background(), wrapper); err != nil {
		t.Errorf("wrapper command must not be probed, got err %v", err)
	}
	if !pandocAvailable(config.Config{IngestPandocCommand: wrapper}) {
		t.Error("a resolvable wrapper command must report available (not probed)")
	}
}

func TestPandocEngineActive_PolicyGating(t *testing.T) {
	stub := writePandocStub(t, "pandoc", "exit 0")
	for _, tc := range []struct {
		policy string
		want   bool
	}{
		{"", true}, // empty defaults to auto
		{"auto", true},
		{"pandoc", true},
		{"docling", false},
		{"docling-serve", false},
		{"mistral", false},
		{"off", false},
	} {
		got := pandocEngineActive(config.Config{IngestExtractor: tc.policy, IngestPandocCommand: stub})
		if got != tc.want {
			t.Errorf("pandocEngineActive(policy=%q) = %v, want %v", tc.policy, got, tc.want)
		}
	}
}

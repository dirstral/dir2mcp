package tests

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// TestSupportBundle_AssemblesDiagnosticsArchive drives the
// `support-bundle` subcommand end-to-end through the public Run entry
// point and asserts:
//  1. The tar.gz contains every artifact a maintainer needs.
//  2. No real secret values leak into any entry (MISTRAL_API_KEY is
//     set to a distinctive sentinel that must not appear anywhere).
//  3. routing.json embeds the OCR-fallback diagnostic that would have
//     told the original failing client why docling wasn't selected.
//  4. The bundle file itself is created with 0o600 perms so an
//     unfavourable umask can't expose its contents to other local users.
func TestSupportBundle_AssemblesDiagnosticsArchive(t *testing.T) {
	const sentinelKey = "should-not-leak-1234567890"
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", sentinelKey)
	t.Setenv("PATH", t.TempDir()) // no docling on PATH

	bundlePath := filepath.Join(tmp, "bundle.tar.gz")
	testutil.WithWorkingDir(t, tmp, func() {
		var stdout, stderr bytes.Buffer
		app := cli.NewAppWithIO(&stdout, &stderr)
		code := app.Run([]string{"support-bundle", "--output", bundlePath})
		if code != 0 {
			t.Fatalf("support-bundle exit=%d stderr=%q", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "Wrote support bundle to") {
			t.Errorf("stdout should confirm bundle write, got %q", stdout.String())
		}
	})

	info, err := os.Stat(bundlePath)
	if err != nil {
		t.Fatalf("stat bundle: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("bundle perms = %o, want 0o600 (diagnostics include server.log)", perm)
	}

	entries := extractTarGz(t, bundlePath)
	for _, name := range []string{
		"version.txt", "os.txt", "config.snapshot.yaml",
		"server.log", "status.json", "list-files.json", "routing.json",
	} {
		if _, ok := entries[name]; !ok {
			t.Errorf("bundle missing entry %q", name)
		}
	}
	for name, body := range entries {
		if bytes.Contains(body, []byte(sentinelKey)) {
			t.Fatalf("entry %q leaked the MISTRAL_API_KEY value", name)
		}
	}
	assertOCRDiagnostic(t, entries["routing.json"])
}

// TestSupportBundle_JSONFlagParseError ensures the support-bundle
// command emits a single structured error via writeCLIError when its
// flag parser rejects input, instead of letting the flag package
// scribble untyped text alongside our JSON output.
func TestSupportBundle_JSONFlagParseError(t *testing.T) {
	tmp := t.TempDir()
	testutil.WithWorkingDir(t, tmp, func() {
		var stdout, stderr bytes.Buffer
		app := cli.NewAppWithIO(&stdout, &stderr)
		code := app.Run([]string{"--json", "support-bundle", "--no-such-flag"})
		if code == 0 {
			t.Fatalf("expected non-zero exit, got 0 (stdout=%q stderr=%q)", stdout.String(), stderr.String())
		}
		// JSON callers see structured output; the flag package's
		// freeform stderr line must not appear in stdout.
		if strings.Contains(stdout.String(), "flag provided but not defined") {
			t.Errorf("stdout contained raw flag-package text: %q", stdout.String())
		}
	})
}

func assertOCRDiagnostic(t *testing.T, raw []byte) {
	t.Helper()
	var routing struct {
		Decisions []struct {
			Capability string `json:"capability"`
			Provider   string `json:"provider"`
			Reason     string `json:"reason"`
		} `json:"decisions"`
	}
	if err := json.Unmarshal(raw, &routing); err != nil {
		t.Fatalf("routing.json invalid: %v", err)
	}
	for _, r := range routing.Decisions {
		if r.Capability != "OCR" {
			continue
		}
		if r.Provider != "mistral-ocr" {
			t.Errorf("OCR provider = %q, want mistral-ocr (fallback)", r.Provider)
		}
		if !strings.Contains(r.Reason, "docling not found") {
			t.Errorf("OCR reason = %q, want substring 'docling not found'", r.Reason)
		}
		return
	}
	t.Errorf("routing.json missing OCR decision: %+v", routing.Decisions)
}

func extractTarGz(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = body
	}
	return out
}

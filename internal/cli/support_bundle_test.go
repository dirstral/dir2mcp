package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

func TestSupportBundle_WritesExpectedEntries(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "should-not-leak-1234567890")
	t.Setenv("PATH", t.TempDir())

	cfg := config.Default()
	cfg.RootDir = dir
	cfg.StateDir = filepath.Join(dir, ".dir2mcp")
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(serverLogPath(cfg.StateDir), []byte("test log line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, warnings := collectSupportArtifacts(context.Background(), newTestApp(), cfg)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}

	out := filepath.Join(dir, "bundle.tar.gz")
	if err := writeSupportBundle(out, files); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	entries := extractBundleEntries(t, out)

	assertSupportBundleEntries(t, entries)
	assertSupportBundleNoSecretLeak(t, entries, "should-not-leak-1234567890")
	assertSupportBundleOCRDiagnostic(t, entries["routing.json"])
}

func assertSupportBundleEntries(t *testing.T, entries map[string][]byte) {
	t.Helper()
	required := []string{
		"version.txt", "os.txt", "config.snapshot.yaml",
		"server.log", "status.json", "list-files.json", "routing.json",
	}
	for _, name := range required {
		if _, ok := entries[name]; !ok {
			t.Errorf("bundle missing entry %q (got %v)", name, sortedKeys(entries))
		}
	}
	if got := string(entries["server.log"]); !strings.Contains(got, "test log line") {
		t.Errorf("server.log = %q, want substring 'test log line'", got)
	}
}

func assertSupportBundleNoSecretLeak(t *testing.T, entries map[string][]byte, secret string) {
	t.Helper()
	for name, body := range entries {
		if bytes.Contains(body, []byte(secret)) {
			t.Fatalf("entry %q leaked the MISTRAL_API_KEY value", name)
		}
	}
}

func assertSupportBundleOCRDiagnostic(t *testing.T, raw []byte) {
	t.Helper()
	var routing struct {
		Decisions []routingRow `json:"decisions"`
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

func TestReadLogTail_CapsAtMaxBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	// Write 100 bytes; ask for the last 30.
	body := bytes.Repeat([]byte("A"), 70)
	body = append(body, bytes.Repeat([]byte("B"), 30)...)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := readLogTail(path, 30)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(out), 30; got != want {
		t.Fatalf("len = %d, want %d", got, want)
	}
	if !bytes.Equal(out, bytes.Repeat([]byte("B"), 30)) {
		t.Errorf("tail = %q, want trailing B's", out)
	}
}

func TestReadLogTail_MissingFileSilent(t *testing.T) {
	out, err := readLogTail(filepath.Join(t.TempDir(), "missing"), 1024)
	if err != nil {
		t.Fatalf("expected nil err for missing file, got %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty bytes, got %q", out)
	}
}

func newTestApp() *App {
	return NewAppWithIO(io.Discard, io.Discard)
}

func extractBundleEntries(t *testing.T, path string) map[string][]byte {
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

func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

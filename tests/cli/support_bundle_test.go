package tests

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

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
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
		"daemon.json",
	} {
		if _, ok := entries[name]; !ok {
			t.Errorf("bundle missing entry %q", name)
		}
	}
	// daemon.json records liveness even with no daemon running (clean state dir).
	if body, ok := entries["daemon.json"]; ok && !bytes.Contains(body, []byte("connection_present")) {
		t.Errorf("daemon.json missing connection_present field: %s", body)
	}
	for name, body := range entries {
		if bytes.Contains(body, []byte(sentinelKey)) {
			t.Fatalf("entry %q leaked the MISTRAL_API_KEY value", name)
		}
	}
	assertOCRDiagnostic(t, entries["routing.json"])
}

// TestSupportBundle_ListFilesIncludesErrorMessage pins the follow-up
// fix to the original failing-install diagnostic story: before this
// change the bundle's list-files.json told maintainers *that* a doc
// failed (status="error") but never *why* — the upstream extraction
// error was logged to server.log only. Now the bundle carries
// documents.error_message so maintainers can triage a failed corpus
// from the bundle alone. Because rel_path/error_message can echo corpus
// content, they are only emitted when the operator opts in with
// --include-content (see TestSupportBundle_ListFilesRedactsContentByDefault
// for the privacy-preserving default).
func TestSupportBundle_ListFilesIncludesErrorMessage(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("PATH", t.TempDir())

	// Seed a failed document so the bundle's list-files.json has
	// something to carry the new error_message field for.
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	const wantMessage = "docling extraction failed: unsupported PDF v1.7"
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath:      "doomed.pdf",
		DocType:      "pdf",
		SizeBytes:    1234,
		Status:       "error",
		ErrorMessage: wantMessage,
	}); err != nil {
		t.Fatalf("upsert error doc: %v", err)
	}
	_ = st.Close()

	bundlePath := filepath.Join(tmp, "bundle.tar.gz")
	testutil.WithWorkingDir(t, tmp, func() {
		var stdout, stderr bytes.Buffer
		app := cli.NewAppWithIO(&stdout, &stderr)
		code := app.Run([]string{"support-bundle", "--include-content", "--output", bundlePath})
		if code != 0 {
			t.Fatalf("support-bundle exit=%d stderr=%q", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "review the bundle before sharing") {
			t.Errorf("--include-content should warn about corpus content, stderr=%q", stderr.String())
		}
	})

	entries := extractTarGz(t, bundlePath)
	listBody, ok := entries["list-files.json"]
	if !ok {
		t.Fatalf("bundle missing list-files.json")
	}
	var listed struct {
		Files []struct {
			RelPath      string `json:"rel_path"`
			Status       string `json:"status"`
			ErrorMessage string `json:"error_message"`
		} `json:"files"`
	}
	if err := json.Unmarshal(listBody, &listed); err != nil {
		t.Fatalf("decode list-files.json: %v body=%s", err, listBody)
	}
	var found *struct {
		RelPath      string `json:"rel_path"`
		Status       string `json:"status"`
		ErrorMessage string `json:"error_message"`
	}
	for i := range listed.Files {
		if listed.Files[i].RelPath == "doomed.pdf" {
			found = &listed.Files[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("list-files.json missing doomed.pdf row: %s", listBody)
	}
	if found.Status != "error" {
		t.Errorf("status = %q, want error", found.Status)
	}
	if found.ErrorMessage != wantMessage {
		t.Errorf("error_message = %q, want %q", found.ErrorMessage, wantMessage)
	}
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

// TestSupportBundle_ListFilesRedactsContentByDefault pins the #436 S1 fix:
// without --include-content the bundle must NOT disclose the corpus inventory
// (rel_path/title) or free-text extraction errors (which can echo file
// content), while still keeping the diagnostic skeleton (doc_type, status, and
// a has_error flag) so a maintainer can triage failures.
func TestSupportBundle_ListFilesRedactsContentByDefault(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("PATH", t.TempDir())

	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	const secretPath = "clients/acme-merger-2026/board-minutes.pdf"
	const secretTitle = "Project Bluebird acquisition terms"
	const secretError = "extraction failed near text 'confidential: layoff list'"
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath:      secretPath,
		DocType:      "pdf",
		Title:        secretTitle,
		SizeBytes:    4096,
		Status:       "error",
		ErrorMessage: secretError,
	}); err != nil {
		t.Fatalf("upsert doc: %v", err)
	}
	_ = st.Close()

	bundlePath := filepath.Join(tmp, "bundle.tar.gz")
	testutil.WithWorkingDir(t, tmp, func() {
		var stdout, stderr bytes.Buffer
		app := cli.NewAppWithIO(&stdout, &stderr)
		code := app.Run([]string{"support-bundle", "--output", bundlePath})
		if code != 0 {
			t.Fatalf("support-bundle exit=%d stderr=%q", code, stderr.String())
		}
		if strings.Contains(stderr.String(), "review the bundle before sharing") {
			t.Errorf("default bundle should not emit the include-content warning, stderr=%q", stderr.String())
		}
	})

	entries := extractTarGz(t, bundlePath)
	// No corpus content anywhere in the bundle by default.
	assertNoLeak(t, entries, []string{
		secretPath, secretTitle, secretError, "board-minutes", "Bluebird", "layoff",
	})

	listBody := entries["list-files.json"]
	var listed struct {
		ContentIncluded bool `json:"content_included"`
		Files           []struct {
			RelPath      string `json:"rel_path"`
			DocType      string `json:"doc_type"`
			Title        string `json:"title"`
			Status       string `json:"status"`
			ErrorMessage string `json:"error_message"`
			HasError     bool   `json:"has_error"`
		} `json:"files"`
	}
	if err := json.Unmarshal(listBody, &listed); err != nil {
		t.Fatalf("decode list-files.json: %v body=%s", err, listBody)
	}
	if listed.ContentIncluded {
		t.Errorf("content_included = true, want false by default")
	}
	if len(listed.Files) != 1 {
		t.Fatalf("want 1 file row, got %d: %s", len(listed.Files), listBody)
	}
	row := listed.Files[0]
	// Skeleton preserved for diagnostics...
	if row.DocType != "pdf" || row.Status != "error" || !row.HasError {
		t.Errorf("diagnostic skeleton lost: %+v", row)
	}
	// ...but extension-only placeholder path, no title, no error text.
	if row.RelPath != "[redacted].pdf" {
		t.Errorf("rel_path = %q, want extension-only placeholder", row.RelPath)
	}
	if row.Title != "" || row.ErrorMessage != "" {
		t.Errorf("title/error_message should be empty by default: %+v", row)
	}
}

// TestSupportBundle_ServerLogRedacted pins that server.log — redirected daemon
// stdout/stderr that can carry bearer tokens and query text — is run through the
// secret redactor before it enters the shareable bundle (previously it was
// bundled raw while client logs and daemon.json were already redacted).
func TestSupportBundle_ServerLogRedacted(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("PATH", t.TempDir())

	const token = "sk-supersecret-abc123XYZ"
	logLine := "2026-07-01 handling request Authorization: Bearer " + token + "\n"
	if err := os.WriteFile(filepath.Join(stateDir, "server.log"), []byte(logLine), 0o600); err != nil {
		t.Fatalf("write server.log: %v", err)
	}

	bundlePath := filepath.Join(tmp, "bundle.tar.gz")
	testutil.WithWorkingDir(t, tmp, func() {
		var stdout, stderr bytes.Buffer
		app := cli.NewAppWithIO(&stdout, &stderr)
		code := app.Run([]string{"support-bundle", "--output", bundlePath})
		if code != 0 {
			t.Fatalf("support-bundle exit=%d stderr=%q", code, stderr.String())
		}
	})

	entries := extractTarGz(t, bundlePath)
	serverLog, ok := entries["server.log"]
	if !ok {
		t.Fatalf("bundle missing server.log")
	}
	if bytes.Contains(serverLog, []byte(token)) {
		t.Fatalf("server.log leaked bearer token: %s", serverLog)
	}
	if !bytes.Contains(serverLog, []byte("[REDACTED]")) {
		t.Errorf("server.log should carry redaction marker, got: %s", serverLog)
	}
}

// TestSupportBundle_RedactedPathKeepsCompoundExtension pins that the
// extension-only placeholder retains multi-part suffixes like .tar.gz (the
// routing signal a maintainer needs) instead of the misleading .gz that
// filepath.Ext alone would yield — while still disclosing no basename.
func TestSupportBundle_RedactedPathKeepsCompoundExtension(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("PATH", t.TempDir())

	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath:   "backups/nightly.tar.gz",
		DocType:   "archive",
		SizeBytes: 1024,
		Status:    "indexed",
	}); err != nil {
		t.Fatalf("upsert doc: %v", err)
	}
	_ = st.Close()

	bundlePath := filepath.Join(tmp, "bundle.tar.gz")
	testutil.WithWorkingDir(t, tmp, func() {
		var stdout, stderr bytes.Buffer
		app := cli.NewAppWithIO(&stdout, &stderr)
		code := app.Run([]string{"support-bundle", "--output", bundlePath})
		if code != 0 {
			t.Fatalf("support-bundle exit=%d stderr=%q", code, stderr.String())
		}
	})

	entries := extractTarGz(t, bundlePath)
	// Basename must not leak even though the compound suffix is retained.
	assertNoLeak(t, entries, []string{"nightly", "backups"})

	var listed struct {
		Files []struct {
			RelPath string `json:"rel_path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(entries["list-files.json"], &listed); err != nil {
		t.Fatalf("decode list-files.json: %v", err)
	}
	if len(listed.Files) != 1 {
		t.Fatalf("want 1 file row, got %d: %s", len(listed.Files), entries["list-files.json"])
	}
	if listed.Files[0].RelPath != "[redacted].tar.gz" {
		t.Errorf("rel_path = %q, want [redacted].tar.gz", listed.Files[0].RelPath)
	}
}

// TestSupportBundle_StatusFailureSamplesRedactedByDefault pins that status.json
// — which embeds the corpus snapshot including FailureSummary.Samples — never
// discloses the raw {rel_path, message} pairs of failed chunks without
// --include-content. The category aggregate (a fixed enum) is still kept so a
// maintainer can triage embedding failures.
func TestSupportBundle_StatusFailureSamplesRedactedByDefault(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("PATH", t.TempDir())

	const secretPath = "clients/acme-merger-2026/board-minutes.pdf"
	const secretMessage = "embed failed near 'confidential: layoff list'"
	// Seed corpus.json directly with a failure sample so the snapshot read path
	// (source=corpus_json) carries the sensitive fields into status.json.
	corpusJSON := `{
  "ts": "2026-07-01T00:00:00Z",
  "indexing": {
    "mode": "idle",
    "errors": 1,
    "failure_summary": {
      "categories": {"payload_too_large": 1},
      "samples": [
        {"rel_path": "` + secretPath + `", "category": "payload_too_large", "message": "` + secretMessage + `"}
      ]
    }
  },
  "doc_counts": {},
  "total_docs": 1
}`
	if err := os.WriteFile(filepath.Join(stateDir, "corpus.json"), []byte(corpusJSON), 0o600); err != nil {
		t.Fatalf("write corpus.json: %v", err)
	}

	bundlePath := filepath.Join(tmp, "bundle.tar.gz")
	testutil.WithWorkingDir(t, tmp, func() {
		var stdout, stderr bytes.Buffer
		app := cli.NewAppWithIO(&stdout, &stderr)
		code := app.Run([]string{"support-bundle", "--output", bundlePath})
		if code != 0 {
			t.Fatalf("support-bundle exit=%d stderr=%q", code, stderr.String())
		}
	})

	entries := extractTarGz(t, bundlePath)
	// No raw failure-sample content anywhere in the bundle by default.
	assertNoLeak(t, entries, []string{
		secretPath, secretMessage, "board-minutes", "layoff",
	})

	var status struct {
		Snapshot struct {
			Indexing struct {
				FailureSummary struct {
					Categories map[string]int64 `json:"categories"`
					Samples    []struct {
						RelPath  string `json:"rel_path"`
						Category string `json:"category"`
						Message  string `json:"message"`
					} `json:"samples"`
				} `json:"failure_summary"`
			} `json:"indexing"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(entries["status.json"], &status); err != nil {
		t.Fatalf("decode status.json: %v body=%s", err, entries["status.json"])
	}
	samples := status.Snapshot.Indexing.FailureSummary.Samples
	if len(samples) != 1 {
		t.Fatalf("want 1 failure sample, got %d: %s", len(samples), entries["status.json"])
	}
	s := samples[0]
	// Category (a fixed enum) is preserved for triage...
	if s.Category != "payload_too_large" {
		t.Errorf("category = %q, want payload_too_large", s.Category)
	}
	if status.Snapshot.Indexing.FailureSummary.Categories["payload_too_large"] != 1 {
		t.Errorf("categories aggregate lost: %+v", status.Snapshot.Indexing.FailureSummary.Categories)
	}
	// ...but rel_path is an extension-only placeholder and message is dropped.
	if s.RelPath != "[redacted].pdf" {
		t.Errorf("rel_path = %q, want extension-only placeholder", s.RelPath)
	}
	if s.Message != "" {
		t.Errorf("message = %q, want empty by default", s.Message)
	}
}

// TestSupportBundle_QuietPreservesIncludeContentWarning pins that --quiet does
// not silently swallow the --include-content privacy-consent warning: quiet
// suppresses the "Wrote support bundle" progress line on stdout but the consent
// notice must still reach stderr.
func TestSupportBundle_QuietPreservesIncludeContentWarning(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("PATH", t.TempDir())

	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath:      "doomed.pdf",
		DocType:      "pdf",
		SizeBytes:    1234,
		Status:       "error",
		ErrorMessage: "docling extraction failed",
	}); err != nil {
		t.Fatalf("upsert error doc: %v", err)
	}
	_ = st.Close()

	bundlePath := filepath.Join(tmp, "bundle.tar.gz")
	testutil.WithWorkingDir(t, tmp, func() {
		var stdout, stderr bytes.Buffer
		app := cli.NewAppWithIO(&stdout, &stderr)
		code := app.Run([]string{"--quiet", "support-bundle", "--include-content", "--output", bundlePath})
		if code != 0 {
			t.Fatalf("support-bundle exit=%d stderr=%q", code, stderr.String())
		}
		if strings.Contains(stdout.String(), "Wrote support bundle to") {
			t.Errorf("--quiet should suppress the progress line, stdout=%q", stdout.String())
		}
		if !strings.Contains(stderr.String(), "review the bundle before sharing") {
			t.Errorf("--quiet must not drop the include-content consent warning, stderr=%q", stderr.String())
		}
	})
}

// assertNoLeak fails if any bundle entry contains any of the sensitive strings.
func assertNoLeak(t *testing.T, entries map[string][]byte, secrets []string) {
	t.Helper()
	for name, body := range entries {
		for _, secret := range secrets {
			if bytes.Contains(body, []byte(secret)) {
				t.Fatalf("entry %q leaked corpus content %q", name, secret)
			}
		}
	}
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

package tests

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// #720: the default `support-bundle` copied .dir2mcp.yaml.snapshot into the
// shareable archive verbatim, applying neither the --include-content consent
// gate nor the secret redactor. That disclosed (1) absolute corpus and state
// paths with no consent, and (2) plaintext credentials whenever a persisted
// endpoint carried URL userinfo — the S3-compatible case in the issue,
// `https://user:pass@host`, which none of the existing redactors matched.
//
// Every test below builds a real bundle through the public CLI entry point from
// a config that genuinely contains a URL credential, reproducing the issue's
// end-to-end steps rather than exercising the redactor in isolation.

const (
	bundleURLPassword = "repro-only-secret"
	bundleURLUser     = "audit-user"
	bundleS3Bucket    = "audit-bucket"
	bundleEndpoint    = "https://" + bundleURLUser + ":" + bundleURLPassword + "@example.invalid"
)

// buildCredentialBundle runs `support-bundle` against a config whose persisted
// S3 endpoint embeds a username and password, and returns the archive entries
// keyed by name plus the corpus root and state directory it used.
func buildCredentialBundle(t *testing.T, extraArgs ...string) (entries map[string][]byte, rootDir, stateDir string) {
	t.Helper()
	tmp := t.TempDir()
	rootDir = filepath.Join(tmp, "corpus-secret", "client-alpha")
	if err := os.MkdirAll(rootDir, 0o700); err != nil {
		t.Fatalf("mkdir corpus: %v", err)
	}
	stateDir = filepath.Join(tmp, "dir2mcp-state")
	seedIndexedDocument(t, stateDir)

	// The issue's reproduction, verbatim: credentials in the environment and a
	// non-secret endpoint that happens to carry userinfo. The endpoint IS
	// persisted to the snapshot; the AWS keys are not.
	t.Setenv("AWS_ACCESS_KEY_ID", "audit-only")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "audit-only-secret")
	t.Setenv("DIR2MCP_SOURCE_KIND", "s3")
	t.Setenv("DIR2MCP_SOURCE_S3_BUCKET", bundleS3Bucket)
	t.Setenv("DIR2MCP_SOURCE_S3_ENDPOINT", bundleEndpoint)

	dest := filepath.Join(tmp, "bundle.tar.gz")
	args := append([]string{"--dir", rootDir, "--state-dir", stateDir, "support-bundle", "--output", dest}, extraArgs...)

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	if code := app.Run(args); code != 0 {
		t.Fatalf("support-bundle exit=%d stderr=%q", code, stderr.String())
	}

	// The persisted snapshot must really contain the credential, otherwise the
	// test would pass for the wrong reason.
	onDisk, err := os.ReadFile(filepath.Join(stateDir, ".dir2mcp.yaml.snapshot"))
	if err != nil {
		t.Fatalf("read persisted snapshot: %v", err)
	}
	if !bytes.Contains(onDisk, []byte(bundleURLPassword)) {
		t.Fatalf("precondition failed: the persisted snapshot does not contain the URL credential, so this test proves nothing:\n%s", onDisk)
	}

	return extractBundleEntries(t, dest), rootDir, stateDir
}

// seedIndexedDocument puts a meta.sqlite under stateDir so status.json takes
// its POPULATED branch. Without it status.json short-circuits to
// {"available": false} and never emits state_dir, which would leave the
// second copy of the same path disclosure untested.
func seedIndexedDocument(t *testing.T, stateDir string) {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath:   "brief.pdf",
		DocType:   "pdf",
		SizeBytes: 1234,
		Status:    "indexed",
	}); err != nil {
		t.Fatalf("upsert doc: %v", err)
	}
}

func extractBundleEntries(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer func() { _ = gz.Close() }()

	entries := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", hdr.Name, err)
		}
		entries[hdr.Name] = body
	}
	return entries
}

// assertNoEntryContains fails naming the offending entry, since "which artifact
// leaked it" is the whole diagnostic.
func assertNoEntryContains(t *testing.T, entries map[string][]byte, needle, what string) {
	t.Helper()
	for name, body := range entries {
		if bytes.Contains(body, []byte(needle)) {
			t.Errorf("bundle entry %q leaked %s (%q)", name, what, needle)
		}
	}
}

// TestSupportBundleDefaultRedactsURLCredential is the credential half of the
// issue. It fails on the pre-fix code: config.snapshot.yaml carried
// `source_s3_endpoint: https://audit-user:repro-only-secret@example.invalid`.
func TestSupportBundleDefaultRedactsURLCredential(t *testing.T) {
	entries, _, _ := buildCredentialBundle(t)

	snapshot, ok := entries["config.snapshot.yaml"]
	if !ok {
		t.Fatal("bundle has no config.snapshot.yaml")
	}
	if !bytes.Contains(snapshot, []byte("[redacted]")) {
		t.Errorf("config.snapshot.yaml shows no redaction marker at all:\n%s", snapshot)
	}
	assertNoEntryContains(t, entries, bundleURLPassword, "a URL password")
	assertNoEntryContains(t, entries, bundleURLUser, "a URL username (an S3 access key ID)")
}

// TestSupportBundleDefaultRedactsCorpusAndStatePaths is the path half. The
// issue reports it against the snapshot; status.json emitted state_dir just as
// plainly, so both are asserted — redacting only one would have left the reader
// able to read the path out of the other.
func TestSupportBundleDefaultRedactsCorpusAndStatePaths(t *testing.T) {
	entries, rootDir, stateDir := buildCredentialBundle(t)

	// Guard the guard: if status.json ever short-circuits to
	// {"available": false} again it stops carrying state_dir, and the
	// assertion below would pass without proving anything.
	if !bytes.Contains(entries["status.json"], []byte(`"state_dir"`)) {
		t.Fatalf("status.json is not the populated branch, so it cannot prove state_dir is redacted:\n%s", entries["status.json"])
	}

	assertNoEntryContains(t, entries, rootDir, "the absolute corpus path")
	assertNoEntryContains(t, entries, stateDir, "the absolute state path")
	assertNoEntryContains(t, entries, bundleS3Bucket, "the operator's bucket name")
}

// TestSupportBundleDefaultKeepsDiagnosticSettings is the counterweight: an
// allow-list is only defensible if the bundle stays useful. These are the
// closed-domain settings a maintainer triages an extraction or retrieval bug
// against, and they must survive redaction.
func TestSupportBundleDefaultKeepsDiagnosticSettings(t *testing.T) {
	entries, _, _ := buildCredentialBundle(t)
	snapshot := string(entries["config.snapshot.yaml"])

	for _, want := range []string{
		"ingest_extractor: auto",
		"ingest_pdf_mode: ocr",
		"index_backend: memory",
		"retrieval_hybrid_enabled: true",
		"chunking_max_tokens: 0",
		"stt_provider: mistral",
		"source_kind: s3",
		"public: false",
	} {
		if !strings.Contains(snapshot, want) {
			t.Errorf("redaction dropped a diagnostic setting %q:\n%s", want, snapshot)
		}
	}
}

// TestSupportBundleMarksRemovedValuesDistinctlyFromUnsetOnes pins that a reader
// can tell "the operator never set this" from "this was removed". Collapsing
// the two would make the bundle actively misleading: a redacted custom system
// prompt would read as a default one.
func TestSupportBundleMarksRemovedValuesDistinctlyFromUnsetOnes(t *testing.T) {
	entries, _, _ := buildCredentialBundle(t)
	snapshot := string(entries["config.snapshot.yaml"])

	// root_dir was set and removed.
	if !strings.Contains(snapshot, `root_dir: "[redacted]"`) {
		t.Errorf("a removed value is not visibly marked:\n%s", snapshot)
	}
	// source_s3_prefix was never configured; nothing was removed, so it must
	// not claim a redaction.
	for _, line := range strings.Split(snapshot, "\n") {
		if strings.HasPrefix(line, "source_s3_prefix:") && strings.Contains(line, "redacted") {
			t.Errorf("an unset value is reported as redacted, which is a lie: %q", line)
		}
	}
}

// TestSupportBundleIncludeContentRestoresPathsButNeverCredentials pins the tier
// boundary the issue's last bullet asks for: consenting to disclose your own
// corpus layout is not consenting to disclose a password.
func TestSupportBundleIncludeContentRestoresPathsButNeverCredentials(t *testing.T) {
	entries, rootDir, _ := buildCredentialBundle(t, "--include-content")

	snapshot := string(entries["config.snapshot.yaml"])
	if !strings.Contains(snapshot, rootDir) {
		t.Errorf("--include-content did not restore root_dir:\n%s", snapshot)
	}
	if !strings.Contains(snapshot, bundleS3Bucket) {
		t.Errorf("--include-content did not restore the bucket name (not a credential):\n%s", snapshot)
	}

	assertNoEntryContains(t, entries, bundleURLPassword, "a URL password despite --include-content")
	assertNoEntryContains(t, entries, bundleURLUser, "a URL username despite --include-content")
	if !strings.Contains(snapshot, "[REDACTED]@example.invalid") {
		t.Errorf("the endpoint host should survive with only its userinfo removed:\n%s", snapshot)
	}
}

// TestSupportBundleRedactedSnapshotIsStillValidYAML pins that redaction does
// not turn a .yaml artifact into something no parser will read. The marker is
// quoted for exactly this reason: a bare [redacted] decodes as a one-element
// flow sequence, and "[redacted] (3 items)" unquoted is a syntax error.
func TestSupportBundleRedactedSnapshotIsStillValidYAML(t *testing.T) {
	entries, _, _ := buildCredentialBundle(t)

	var doc map[string]interface{}
	if err := yaml.Unmarshal(entries["config.snapshot.yaml"], &doc); err != nil {
		t.Fatalf("redacted snapshot is not parseable YAML: %v\n%s", err, entries["config.snapshot.yaml"])
	}
	if got, ok := doc["root_dir"].(string); !ok || got != "[redacted]" {
		t.Errorf("root_dir did not decode as the redaction marker string, got %#v", doc["root_dir"])
	}
	if got, ok := doc["path_excludes"].(string); !ok || !strings.HasPrefix(got, "[redacted] (") {
		t.Errorf("a redacted list did not decode as a scalar marker, got %#v", doc["path_excludes"])
	}
	// A closed-domain value must survive with its real type intact.
	if got, ok := doc["public"].(bool); !ok || got {
		t.Errorf("public did not decode as bool false, got %#v", doc["public"])
	}
}

// TestSupportBundleSnapshotOnDiskIsUntouched pins that redaction applies to the
// bundle's COPY only. Rewriting the persisted snapshot would break config
// reload and embed-identity verification.
func TestSupportBundleSnapshotOnDiskIsUntouched(t *testing.T) {
	_, rootDir, stateDir := buildCredentialBundle(t)

	onDisk, err := os.ReadFile(filepath.Join(stateDir, ".dir2mcp.yaml.snapshot"))
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	for _, want := range []string{rootDir, bundleEndpoint} {
		if !strings.Contains(string(onDisk), want) {
			t.Errorf("bundling mutated the persisted snapshot; %q is gone:\n%s", want, onDisk)
		}
	}
}

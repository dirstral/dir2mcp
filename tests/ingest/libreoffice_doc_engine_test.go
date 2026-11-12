package tests

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Tests for the libreoffice extraction engine (spec 0.93.0): legacy .doc is
// converted to .docx by headless LibreOffice and read by the pandoc docx path.

const loStubPath = "PATH=/usr/bin:/bin\nexport PATH\n"

// loFakeSoffice writes <outdir>/<input>.docx the way soffice --convert-to does.
const loFakeSoffice = loStubPath + `case "$1" in --version) echo 'LibreOffice 7.6'; exit 0;; esac
out=""; prev=""; last=""
for a in "$@"; do
  if [ "$prev" = "--outdir" ]; then out="$a"; fi
  prev="$a"; last="$a"
done
printf 'FAKEDOCX' > "$out/$(basename "$last" .doc).docx"
exit 0`

// loFakePandoc prints Cyrillic Markdown for a .docx input.
const loFakePandoc = loStubPath + `case "$1" in --version) echo 'pandoc 3.1'; exit 0;; esac
printf '# Описание\n\nАрхивная запись из legacy doc.\n'
exit 0`

// loStubDir writes the named stubs into a fresh directory.
func loStubDir(t *testing.T, scripts map[string]string) string {
	t.Helper()
	skipOnWindows(t, "shell-script stubs need a POSIX sh; Windows cannot run them")
	dir := testutil.TempDir(t)
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	return dir
}

// wiredService builds an ingest service the way `dir2mcp up` does: the primary
// extractor from config, then the capability-activated T2 engines.
func wiredService(t *testing.T, cfg config.Config, st *store.SQLiteStore) *ingest.Service {
	t.Helper()
	svc := mustNewIngestService(t, cfg, st)
	if ex := ingest.DocumentExtractorFromConfig(cfg); ex != nil {
		svc.SetDocumentExtractor(ex)
	}
	svc.ActivatePandocEngine(cfg)
	svc.ActivateLibreOfficeEngine(cfg)
	return svc
}

// extractedMarkdown returns the provider and chunk text of the document's
// active extracted_markdown representation.
func extractedMarkdown(t *testing.T, st *store.SQLiteStore, relPath string) (providerName, text string) {
	t.Helper()
	ctx := context.Background()
	reps, err := st.ActiveRepresentations(ctx, relPath)
	if err != nil {
		t.Fatalf("ActiveRepresentations(%s): %v", relPath, err)
	}
	for _, r := range reps {
		if r.RepType != ingest.RepTypeExtractedMarkdown {
			continue
		}
		var meta map[string]string
		if err := json.Unmarshal([]byte(r.MetaJSON), &meta); err != nil {
			t.Fatalf("meta_json %q: %v", r.MetaJSON, err)
		}
		chunks, err := st.GetChunksByRepID(ctx, r.RepID)
		if err != nil {
			t.Fatalf("GetChunksByRepID(%d): %v", r.RepID, err)
		}
		var b strings.Builder
		for _, c := range chunks {
			b.WriteString(c.Text)
			b.WriteString("\n")
		}
		return meta["provider"], b.String()
	}
	t.Fatalf("%s has no active extracted_markdown representation (reps=%+v)", relPath, reps)
	return "", ""
}

// TestLibreOfficeEngine_UpgradePicksUpSkippedDoc is the upgrade path: a corpus
// indexed while the engine was inactive stores each .doc as a durable
// unsupported_format skip. Once soffice and pandoc are present, the next
// incremental run (no --force) must index it.
func TestLibreOfficeEngine_UpgradePicksUpSkippedDoc(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "")
	const rel = "descriptions/film-0042.doc"
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, rel), "legacy word bytes")
	f := ingest.DiscoveredFile{RelPath: rel, SizeBytes: 17, MTimeUnix: time.Now().Unix()}
	cfg := config.Config{RootDir: root, StateDir: testutil.TempDir(t), STTProvider: "off", IngestExtractor: "auto", IngestLibreOfficeTimeoutSec: 30}
	st := newRealStore(t)

	// Run 1: only pandoc is installed, so the engine is inactive.
	t.Setenv("PATH", loStubDir(t, map[string]string{"pandoc": loFakePandoc}))
	if err := wiredService(t, cfg, st).ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	doc := documentByPath(t, st, rel)
	if doc.Status != string(model.DocumentStatusSkipped) || doc.SkipReason != model.SkipReasonUnsupportedFormat {
		t.Fatalf("run 1: status=%q skip_reason=%q, want skipped/%s", doc.Status, doc.SkipReason, model.SkipReasonUnsupportedFormat)
	}

	// Run 2: LibreOffice is installed too. Same bytes, incremental run.
	t.Setenv("PATH", loStubDir(t, map[string]string{"pandoc": loFakePandoc, "soffice": loFakeSoffice}))
	if err := wiredService(t, cfg, st).ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	doc = documentByPath(t, st, rel)
	if doc.Status != string(model.DocumentStatusOK) || doc.SkipReason != "" {
		t.Fatalf("run 2: status=%q skip_reason=%q, want ok with no skip reason", doc.Status, doc.SkipReason)
	}
	prov, text := extractedMarkdown(t, st, rel)
	if prov != "libreoffice" {
		t.Fatalf("provider = %q, want libreoffice", prov)
	}
	if !strings.Contains(text, "Архивная запись") {
		t.Fatalf("chunk text %q lost the Cyrillic content", text)
	}
}

// TestLibreOfficeEngine_UpgradeWithHashedSkipRow covers a skip row that DOES
// carry a content_hash (a row an older binary may have written). The content
// gate alone would treat it as done; the engine-activation trigger must still
// reprocess it.
func TestLibreOfficeEngine_UpgradeWithHashedSkipRow(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "")
	const rel = "old/notes.doc"
	const body = "legacy word bytes"
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, rel), body)
	cfg := config.Config{RootDir: root, StateDir: testutil.TempDir(t), STTProvider: "off", IngestExtractor: "auto", IngestLibreOfficeTimeoutSec: 30}
	st := newRealStore(t)
	if err := st.UpsertDocument(context.Background(), model.Document{
		RelPath:     rel,
		DocType:     ingest.ClassifyDocType(rel),
		SizeBytes:   int64(len(body)),
		ContentHash: ingest.ComputeContentHash([]byte(body)),
		Status:      string(model.DocumentStatusSkipped),
		SkipReason:  model.SkipReasonUnsupportedFormat,
	}); err != nil {
		t.Fatalf("seed skip row: %v", err)
	}

	t.Setenv("PATH", loStubDir(t, map[string]string{"pandoc": loFakePandoc, "soffice": loFakeSoffice}))
	f := ingest.DiscoveredFile{RelPath: rel, SizeBytes: int64(len(body)), MTimeUnix: time.Now().Unix()}
	if err := wiredService(t, cfg, st).ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	if doc := documentByPath(t, st, rel); doc.Status != string(model.DocumentStatusOK) {
		t.Fatalf("status = %q, want ok after the engine became active", doc.Status)
	}
	if prov, _ := extractedMarkdown(t, st, rel); prov != "libreoffice" {
		t.Fatalf("provider = %q, want libreoffice", prov)
	}
}

// TestLibreOfficeEngine_ConversionFailureIsPerDocumentError checks that a
// conversion failure is a per-document error under the existing rules, not a
// skip and not a run failure for other documents.
func TestLibreOfficeEngine_ConversionFailureIsPerDocumentError(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "")
	const rel = "broken.doc"
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, rel), "not really a doc")
	cfg := config.Config{RootDir: root, StateDir: testutil.TempDir(t), STTProvider: "off", IngestExtractor: "auto", IngestLibreOfficeTimeoutSec: 30}
	failing := loStubPath + "case \"$1\" in --version) exit 0;; esac\necho 'Error: source file could not be loaded' >&2\nexit 1"
	t.Setenv("PATH", loStubDir(t, map[string]string{"pandoc": loFakePandoc, "soffice": failing}))
	st := newRealStore(t)
	f := ingest.DiscoveredFile{RelPath: rel, SizeBytes: 16, MTimeUnix: time.Now().Unix()}
	if err := wiredService(t, cfg, st).ProcessDocument(context.Background(), f, nil, false); err == nil {
		t.Fatal("ProcessDocument must return the conversion error")
	}
	doc := documentByPath(t, st, rel)
	if doc.Status != string(model.DocumentStatusError) || !strings.Contains(doc.ErrorMessage, "libreoffice") {
		t.Fatalf("status=%q error=%q, want error naming libreoffice", doc.Status, doc.ErrorMessage)
	}
}

// TestLibreOfficeEngine_RealBinaries runs the engine against the real soffice
// and pandoc. It needs both on PATH and skips otherwise. The .doc fixture is
// made at test time: pandoc writes a .docx with Cyrillic text, and soffice
// converts it to the legacy .doc format, so no binary fixture is committed.
func TestLibreOfficeEngine_RealBinaries(t *testing.T) {
	skipOnWindows(t, "the real-binary check runs on unix hosts")
	soffice, pandoc := realLibreOfficeBinaries(t)
	t.Setenv("MISTRAL_API_KEY", "")
	docBytes := makeRealDocFixture(t, soffice, pandoc)
	assertRealDocIndexed(t, docBytes)
}

// realLibreOfficeBinaries returns the real soffice and pandoc paths, or skips
// the test with a message that names the missing binary.
func realLibreOfficeBinaries(t *testing.T) (soffice, pandoc string) {
	t.Helper()
	soffice, err := exec.LookPath("soffice")
	if err != nil {
		if soffice, err = exec.LookPath("libreoffice"); err != nil {
			t.Skip("skipping: neither soffice nor libreoffice is on PATH (install LibreOffice to run this test)")
		}
	}
	pandoc, err = exec.LookPath("pandoc")
	if err != nil {
		t.Skip("skipping: pandoc is not on PATH (install pandoc to run this test)")
	}
	return soffice, pandoc
}

// makeRealDocFixture makes a legacy .doc with Cyrillic and English text:
// pandoc writes a .docx, and soffice converts it to .doc.
func makeRealDocFixture(t *testing.T, soffice, pandoc string) []byte {
	t.Helper()
	work := testutil.TempDir(t)
	src := filepath.Join(work, "fixture.md")
	if err := os.WriteFile(src, []byte("# Архив\n\nОписание фильма: Москва, 1991 год.\n\nEnglish line for the mixed corpus.\n"), 0o600); err != nil {
		t.Fatalf("write fixture source: %v", err)
	}
	docx := filepath.Join(work, "fixture.docx")
	if out, err := exec.Command(pandoc, src, "-o", docx).CombinedOutput(); err != nil {
		t.Fatalf("pandoc make fixture: %v: %s", err, out)
	}
	profile := filepath.Join(work, "profile")
	conv := exec.Command(soffice, "--headless", "--norestore", "-env:UserInstallation=file://"+filepath.ToSlash(profile),
		"--convert-to", "doc", "--outdir", work, docx)
	if out, err := conv.CombinedOutput(); err != nil {
		t.Fatalf("soffice make .doc fixture: %v: %s", err, out)
	}
	docBytes, err := os.ReadFile(filepath.Join(work, "fixture.doc"))
	if err != nil {
		t.Fatalf("read .doc fixture: %v", err)
	}
	return docBytes
}

// assertRealDocIndexed indexes docBytes under the libreoffice pin and checks
// the text, the provider, and that nothing was written next to the source.
func assertRealDocIndexed(t *testing.T, docBytes []byte) {
	t.Helper()
	const rel = "archive/fixture.doc"
	root := testutil.TempDir(t)
	if err := os.MkdirAll(filepath.Join(root, "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	srcDoc := filepath.Join(root, rel)
	if err := os.WriteFile(srcDoc, docBytes, 0o400); err != nil {
		t.Fatalf("write corpus .doc: %v", err)
	}
	cfg := config.Config{RootDir: root, StateDir: testutil.TempDir(t), STTProvider: "off", IngestExtractor: "libreoffice", IngestLibreOfficeTimeoutSec: 120}
	if !ingest.LibreOfficeActive(cfg) {
		t.Fatalf("real soffice and pandoc are on PATH but the engine is inactive: %s", ingest.LibreOfficeInstallHint(cfg))
	}
	st := newRealStore(t)
	f := ingest.DiscoveredFile{RelPath: rel, SizeBytes: int64(len(docBytes)), MTimeUnix: time.Now().Unix()}
	if err := wiredService(t, cfg, st).ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	if doc := documentByPath(t, st, rel); doc.Status != string(model.DocumentStatusOK) {
		t.Fatalf("status = %q (%s), want ok", doc.Status, doc.ErrorMessage)
	}
	prov, text := extractedMarkdown(t, st, rel)
	if prov != "libreoffice" {
		t.Fatalf("provider = %q, want libreoffice", prov)
	}
	for _, want := range []string{"Архив", "Москва, 1991", "English line"} {
		if !strings.Contains(text, want) {
			t.Fatalf("extracted text %q misses %q", text, want)
		}
	}
	// Nothing was written next to the source.
	entries, err := os.ReadDir(filepath.Join(root, "archive"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("source dir holds %d entries, want only the source .doc", len(entries))
	}
}

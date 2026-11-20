//go:build unix

package tests

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// fixtureCommandTimeout bounds each fixture command, so a stalled LibreOffice
// or pandoc cannot hang the test run.
const fixtureCommandTimeout = 2 * time.Minute

// runBoundedFixture runs a fixture command with a deadline, in its own process
// group. A timeout kills the whole group (soffice starts soffice.bin), and the
// group is also killed after a normal exit, so no helper outlives the test.
func runBoundedFixture(t *testing.T, bin string, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return out, err
}

// TestLibreOfficeEngine_RealBinaries runs the engine against the real soffice
// and pandoc. It needs both on PATH and skips otherwise. The .doc fixture is
// made at test time: pandoc writes a .docx with Cyrillic text, and soffice
// converts it to the legacy .doc format, so no binary fixture is committed.
func TestLibreOfficeEngine_RealBinaries(t *testing.T) {
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
	if out, err := runBoundedFixture(t, pandoc, src, "-o", docx); err != nil {
		t.Fatalf("pandoc make fixture: %v: %s", err, out)
	}
	profile := filepath.Join(work, "profile")
	if out, err := runBoundedFixture(t, soffice, "--headless", "--norestore", "-env:UserInstallation=file://"+filepath.ToSlash(profile),
		"--convert-to", "doc", "--outdir", work, docx); err != nil {
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

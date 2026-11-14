//go:build unix

package tests

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Unit-level tests for the libreoffice engine (spec 0.93.0), through the
// exported surface of internal/ingest. Fake binaries stand in for soffice and
// pandoc. The scripts set their own PATH because the tests point PATH at the
// stub directory only, so a real soffice or pandoc on the host can never leak
// into a verdict.

// fakeSofficeOK answers --version, records its arguments, and writes a fake
// .docx named after the input into --outdir, the way soffice --convert-to does.
const fakeSofficeOK = loStubPath + `case "$1" in --version) echo 'LibreOffice 7.6'; exit 0;; esac
out=""; prev=""; last=""
for a in "$@"; do
  if [ "$prev" = "--outdir" ]; then out="$a"; fi
  prev="$a"; last="$a"
done
if [ -n "$ARGS_LOG" ]; then printf '%s\n' "$@" > "$ARGS_LOG"; fi
base=$(basename "$last" .doc)
printf 'FAKEDOCX' > "$out/$base.docx"
exit 0`

// fakePandocOK answers --version and prints Markdown with Cyrillic text for a
// .docx input. Any other input fails, so a .doc that reached pandoc without the
// LibreOffice conversion is visible.
const fakePandocOK = loStubPath + `case "$1" in --version) echo 'pandoc 3.1'; exit 0;; esac
case "$1" in *.docx) ;; *) echo "unexpected input $1" >&2; exit 9;; esac
printf '# Отчёт\n\nПривет, мир. converted from docx\n'
exit 0`

// usePathOnly points PATH at the stub directory alone for the test.
func usePathOnly(t *testing.T, scripts map[string]string) {
	t.Helper()
	t.Setenv("PATH", loStubDir(t, scripts))
}

// isolateTempDir points os.TempDir at a fresh directory, so a test can prove
// the engine leaves nothing behind.
func isolateTempDir(t *testing.T) string {
	t.Helper()
	dir := testutil.TempDir(t)
	t.Setenv("TMPDIR", dir)
	return dir
}

func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("temp dir %s is not empty after the conversion: %v", dir, names)
	}
}

func TestLibreOfficeAvailability_NeedsBothBinaries(t *testing.T) {
	const sofficeName, pandocName = "soffice (LibreOffice)", "pandoc"
	cases := []struct {
		name        string
		scripts     map[string]string
		wantActive  bool
		wantMissing []string
	}{
		{"both present", map[string]string{"soffice": fakeSofficeOK, "pandoc": fakePandocOK}, true, nil},
		{"libreoffice launcher name", map[string]string{"libreoffice": fakeSofficeOK, "pandoc": fakePandocOK}, true, nil},
		{"soffice only", map[string]string{"soffice": fakeSofficeOK}, false, []string{pandocName}},
		{"pandoc only", map[string]string{"pandoc": fakePandocOK}, false, []string{sofficeName}},
		{"neither", map[string]string{}, false, []string{sofficeName, pandocName}},
		{"soffice fails its probe", map[string]string{"soffice": "exit 3", "pandoc": fakePandocOK}, false, []string{sofficeName}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usePathOnly(t, tc.scripts)
			cfg := config.Config{IngestExtractor: "auto"}
			if got := ingest.LibreOfficeActive(cfg); got != tc.wantActive {
				t.Fatalf("LibreOfficeActive = %v, want %v", got, tc.wantActive)
			}
			hint := ingest.LibreOfficeInstallHint(cfg)
			if tc.wantActive {
				if hint != "" {
					t.Fatalf("hint = %q, want empty when both binaries are present", hint)
				}
				return
			}
			for _, name := range []string{sofficeName, pandocName} {
				want := containsString(tc.wantMissing, name)
				if got := strings.Contains(hint, name); got != want {
					t.Fatalf("hint %q names %q = %v, want %v", hint, name, got, want)
				}
			}
		})
	}
}

func TestLibreOfficeAvailability_PolicyGates(t *testing.T) {
	usePathOnly(t, map[string]string{"soffice": fakeSofficeOK, "pandoc": fakePandocOK})
	for policy, want := range map[string]bool{
		"":              true,
		"auto":          true,
		"libreoffice":   true,
		"pandoc":        false,
		"docling":       false,
		"docling-serve": false,
		"mistral":       false,
		"off":           false,
	} {
		if got := ingest.LibreOfficeActive(config.Config{IngestExtractor: policy}); got != want {
			t.Errorf("LibreOfficeActive(policy=%q) = %v, want %v", policy, got, want)
		}
	}
}

// TestLibreOfficeInstallHint_PinNamesPolicy: under a pin of another engine,
// installing the binaries alone does not turn the engine on, so the remedy
// names the policy change too.
func TestLibreOfficeInstallHint_PinNamesPolicy(t *testing.T) {
	usePathOnly(t, map[string]string{"pandoc": fakePandocOK})
	hint := ingest.LibreOfficeInstallHint(config.Config{IngestExtractor: "docling"})
	for _, want := range []string{"install soffice (LibreOffice)", "set ingest.extractor to auto or libreoffice"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("hint %q misses %q", hint, want)
		}
	}
	usePathOnly(t, map[string]string{"soffice": fakeSofficeOK, "pandoc": fakePandocOK})
	if hint := ingest.LibreOfficeInstallHint(config.Config{IngestExtractor: "docling"}); hint != "set ingest.extractor to auto or libreoffice for legacy .doc" {
		t.Fatalf("hint with both binaries under a docling pin = %q, want only the policy change", hint)
	}
}

func TestDescribeLibreOffice_PinAndAuto(t *testing.T) {
	usePathOnly(t, map[string]string{"soffice": fakeSofficeOK, "pandoc": fakePandocOK})
	isolateTempDir(t)
	pin := config.Config{IngestExtractor: "libreoffice"}
	if d := ingest.DescribeDocumentExtractor(pin); d.Name != "libreoffice" || d.Source != "explicit" {
		t.Fatalf("pin decision = %+v, want libreoffice/explicit", d)
	}
	ex := ingest.DocumentExtractorFromConfig(pin)
	if ex == nil {
		t.Fatal("pin: DocumentExtractorFromConfig returned nil")
	}
	// fakePandocOK rejects any input but a .docx, so Cyrillic Markdown from a
	// .doc proves the extractor ran the LibreOffice conversion first.
	if md, err := ex.Extract(context.Background(), "a.doc", []byte("x")); err != nil || !strings.Contains(md, "Привет") {
		t.Fatalf("pin extractor on a .doc = (%q, %v), want the converted text", md, err)
	}
	if d := ingest.DescribeLibreOfficeEngine(config.Config{IngestExtractor: "auto"}); d.Name != "libreoffice" || d.Source != "auto" {
		t.Fatalf("auto engine decision = %+v, want libreoffice/auto", d)
	}
	if d := ingest.DescribeLibreOfficeEngine(config.Config{IngestExtractor: "pandoc"}); d.Source != "ineligible" {
		t.Fatalf("pandoc pin engine decision = %+v, want ineligible", d)
	}
}

func TestDescribeLibreOffice_PinWithoutPandocIsDisabled(t *testing.T) {
	usePathOnly(t, map[string]string{"soffice": fakeSofficeOK})
	d := ingest.DescribeDocumentExtractor(config.Config{IngestExtractor: "libreoffice"})
	if d.Name != "" || d.Source != "disabled" || !strings.Contains(d.Reason, "pandoc") {
		t.Fatalf("pin without pandoc = %+v, want disabled naming pandoc", d)
	}
	if ingest.DocumentExtractorFromConfig(config.Config{IngestExtractor: "libreoffice"}) != nil {
		t.Fatal("pin without pandoc must build no extractor (no fallback)")
	}
	auto := ingest.DescribeLibreOfficeEngine(config.Config{IngestExtractor: "auto"})
	if auto.Source != "disabled" || !strings.Contains(auto.Reason, "pandoc") || strings.Contains(auto.Reason, "soffice") {
		t.Fatalf("auto without pandoc = %+v, want disabled naming only pandoc", auto)
	}
}

// TestActivateLibreOfficeEngine_ServiceRouting checks the service wiring: the
// engine reads .doc beside a docling primary under auto, the pin makes it the
// primary engine, and without soffice no engine reads .doc.
func TestActivateLibreOfficeEngine_ServiceRouting(t *testing.T) {
	usePathOnly(t, map[string]string{"soffice": fakeSofficeOK, "pandoc": fakePandocOK})
	cfg := config.Config{RootDir: testutil.TempDir(t), StateDir: testutil.TempDir(t), STTProvider: "off", IngestExtractor: "auto", IngestLibreOfficeTimeoutSec: 7}
	st := newRealStore(t)
	doc := model.Document{RelPath: "archive/old.doc", DocType: ingest.ClassifyDocType("archive/old.doc")}

	s := mustNewIngestService(t, cfg, st)
	s.SetDocumentExtractor(ingest.NewDoclingExtractor("docling"))
	s.ActivatePandocEngine(cfg)
	s.ActivateLibreOfficeEngine(cfg)
	if !s.CanExtractSourceText(doc) {
		t.Fatal("CanExtractSourceText(.doc) = false with the libreoffice engine active")
	}
	md, err := s.ExtractSourceText(context.Background(), doc, []byte("legacy"))
	if err != nil || !strings.Contains(md, "Привет") {
		t.Fatalf("ExtractSourceText(.doc) = (%q, %v), want the libreoffice text", md, err)
	}

	pinCfg := cfg
	pinCfg.IngestExtractor = "libreoffice"
	pinned := wiredService(t, pinCfg, st)
	if engine, _ := pinned.ActiveExtractionEngine(); engine != "libreoffice" {
		t.Fatalf("pin: ActiveExtractionEngine = %q, want libreoffice", engine)
	}
	if pinned.CanExtractSourceText(model.Document{RelPath: "a.docx", DocType: ingest.ClassifyDocType("a.docx")}) {
		t.Fatal("pin: .docx must have no fallback engine")
	}

	usePathOnly(t, map[string]string{"pandoc": fakePandocOK})
	s2 := mustNewIngestService(t, cfg, st)
	s2.SetDocumentExtractor(ingest.NewDoclingExtractor("docling"))
	s2.ActivateLibreOfficeEngine(cfg)
	if s2.CanExtractSourceText(doc) {
		t.Fatal("libreoffice engine must stay inactive without soffice")
	}
}

// TestLibreOfficeEngine_TimeoutFromConfig checks that the service builds the
// engine with ingest.libreoffice.timeout_sec: a hung soffice ends at 1s, and
// the per-document error names the setting.
func TestLibreOfficeEngine_TimeoutFromConfig(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "")
	const rel = "slow.doc"
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, rel), "legacy")
	cfg := config.Config{RootDir: root, StateDir: testutil.TempDir(t), STTProvider: "off", IngestExtractor: "auto", IngestLibreOfficeTimeoutSec: 1}
	usePathOnly(t, map[string]string{"soffice": loStubPath + "case \"$1\" in --version) exit 0;; esac\nexec sleep 60", "pandoc": fakePandocOK})
	st := newRealStore(t)
	f := ingest.DiscoveredFile{RelPath: rel, SizeBytes: 6, MTimeUnix: time.Now().Unix()}
	start := time.Now()
	err := wiredService(t, cfg, st).ProcessDocument(context.Background(), f, nil, false)
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("ProcessDocument took %s, want it bounded near the 1s limit", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "ingest.libreoffice.timeout_sec=1") {
		t.Fatalf("ProcessDocument error = %v, want a timeout naming ingest.libreoffice.timeout_sec=1", err)
	}
}

func TestLibreOfficeExtract_ConvertsInPrivateTempDir(t *testing.T) {
	usePathOnly(t, map[string]string{"soffice": fakeSofficeOK, "pandoc": fakePandocOK})
	argsLog := filepath.Join(testutil.TempDir(t), "args.txt")
	t.Setenv("ARGS_LOG", argsLog)
	// A space in the temp path proves the profile URL is percent-encoded.
	tmp := filepath.Join(testutil.TempDir(t), "a b")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)

	ex := ingest.NewLibreOfficeExtractor("", "", "", 30*time.Second)
	md, err := ex.Extract(context.Background(), "letters/old.doc", []byte("legacy word bytes"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(md, "Привет, мир") || !strings.Contains(md, "Отчёт") {
		t.Fatalf("markdown %q lost the Cyrillic text", md)
	}
	raw, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("read args log: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, want := range []string{"--headless", "--norestore", "--convert-to", "docx", "--outdir"} {
		if !containsString(args, want) {
			t.Errorf("soffice args %q miss %q", args, want)
		}
	}
	var profile string
	for _, a := range args {
		if strings.HasPrefix(a, "-env:UserInstallation=file:///") {
			profile = a
		}
	}
	if profile == "" || !strings.Contains(profile, "a%20b") {
		t.Errorf("soffice args %q carry no absolute, percent-encoded per-conversion profile URL", args)
	}
	if last := args[len(args)-1]; filepath.Base(last) != "input.doc" || strings.Contains(last, "letters") {
		t.Errorf("input arg %q must be the private copy, never the source path", last)
	}
	assertDirEmpty(t, tmp)
}

func TestLibreOfficeExtract_ParallelProfilesDiffer(t *testing.T) {
	usePathOnly(t, map[string]string{"soffice": fakeSofficeOK, "pandoc": fakePandocOK})
	logs := testutil.TempDir(t)
	profiles := map[string]bool{}
	for i := 0; i < 2; i++ {
		log := filepath.Join(logs, strconv.Itoa(i))
		t.Setenv("ARGS_LOG", log)
		if _, err := ingest.NewLibreOfficeExtractor("", "", "", 30*time.Second).Extract(context.Background(), "a.doc", []byte("x")); err != nil {
			t.Fatalf("Extract %d: %v", i, err)
		}
		raw, _ := os.ReadFile(log)
		for _, a := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(a, "-env:UserInstallation=") {
				profiles[a] = true
			}
		}
	}
	if len(profiles) != 2 {
		t.Fatalf("profiles = %v, want a distinct profile per conversion", profiles)
	}
}

func TestLibreOfficeExtract_FailureIsAnError(t *testing.T) {
	cases := []struct {
		name    string
		soffice string
		want    string
	}{
		{"non-zero exit", loStubPath + "case \"$1\" in --version) exit 0;; esac\necho 'Error: source file could not be loaded' >&2\nexit 1", "source file could not be loaded"},
		{"exit 0 without output", loStubPath + "exit 0", "produced no .docx output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usePathOnly(t, map[string]string{"soffice": tc.soffice, "pandoc": fakePandocOK})
			tmp := isolateTempDir(t)
			_, err := ingest.NewLibreOfficeExtractor("", "", "", 30*time.Second).Extract(context.Background(), "bad.doc", []byte("x"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Extract error = %v, want it to contain %q", err, tc.want)
			}
			assertDirEmpty(t, tmp)
		})
	}
}

func TestLibreOfficeExtract_TimeoutKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(testutil.TempDir(t), "child.pid")
	// The fake soffice starts a long-lived helper (like soffice.bin), records its
	// PID, then hangs. A timeout must kill the whole group, helper included.
	hang := loStubPath + "sleep 60 &\necho $! > \"$CHILD_PID\"\nwait\n"
	usePathOnly(t, map[string]string{"soffice": hang, "pandoc": fakePandocOK})
	t.Setenv("CHILD_PID", pidFile)
	tmp := isolateTempDir(t)

	start := time.Now()
	_, err := ingest.NewLibreOfficeExtractor("", "", "", time.Second).Extract(context.Background(), "slow.doc", []byte("x"))
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("timeout took %s, want it bounded near the 1s limit", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "libreoffice extraction timed out") || !strings.Contains(err.Error(), "ingest.libreoffice.timeout_sec=1") {
		t.Fatalf("Extract error = %v, want the libreoffice timeout naming the setting", err)
	}
	assertDirEmpty(t, tmp)

	raw, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("the fake soffice did not record its helper PID: %v", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if convErr != nil {
		t.Fatalf("bad helper PID %q: %v", raw, convErr)
	}
	deadline := time.Now().Add(5 * time.Second)
	// The loop ends when the helper is gone, or is a zombie that waits to be
	// reaped (it has exited, so it does not count as a survivor).
	for processRunning(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("helper process %d survived the timeout; the process group was not killed", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestLibreOfficeExtract_TimeoutCoversPandocRead checks that one time limit
// covers the conversion AND the pandoc read (spec 0.93.0): a fast soffice
// followed by a hung pandoc still ends at the libreoffice limit, with the
// libreoffice timeout error, and the work dir is removed.
func TestLibreOfficeExtract_TimeoutCoversPandocRead(t *testing.T) {
	hungPandoc := loStubPath + "case \"$1\" in --version) exit 0;; esac\nexec sleep 60"
	usePathOnly(t, map[string]string{"soffice": fakeSofficeOK, "pandoc": hungPandoc})
	tmp := isolateTempDir(t)
	start := time.Now()
	_, err := ingest.NewLibreOfficeExtractor("", "", "", time.Second).Extract(context.Background(), "a.doc", []byte("x"))
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("pandoc step took %s, want it bounded by the 1s libreoffice limit", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "libreoffice extraction timed out") {
		t.Fatalf("Extract error = %v, want the libreoffice timeout", err)
	}
	assertDirEmpty(t, tmp)
}

// TestLibreOfficeCommandOverride checks ingest.libreoffice.command: it names
// the binary, it wins over PATH, and the probe runs on it.
func TestLibreOfficeCommandOverride(t *testing.T) {
	custom := loStubDir(t, map[string]string{"my-office": fakeSofficeOK})
	usePathOnly(t, map[string]string{"pandoc": fakePandocOK})
	cfg := config.Config{IngestExtractor: "auto", IngestLibreOfficeCommand: filepath.Join(custom, "my-office")}
	if !ingest.LibreOfficeActive(cfg) {
		t.Fatalf("engine inactive with a functional configured binary: %s", ingest.LibreOfficeInstallHint(cfg))
	}
	md, err := ingest.NewLibreOfficeExtractor("", cfg.IngestLibreOfficeCommand, "", 30*time.Second).Extract(context.Background(), "a.doc", []byte("x"))
	if err != nil || !strings.Contains(md, "Привет") {
		t.Fatalf("Extract via the configured binary = (%q, %v)", md, err)
	}
	broken := loStubDir(t, map[string]string{"bad-office": "exit 2"})
	if ingest.LibreOfficeActive(config.Config{IngestLibreOfficeCommand: filepath.Join(broken, "bad-office")}) {
		t.Fatal("a configured binary that fails its probe must leave the engine inactive")
	}
}

// TestLibreOfficeCommand_PathWithSpaces checks that a configured binary whose
// path holds a space (for example under "Program Files") is one path, not a
// command split on spaces.
func TestLibreOfficeCommand_PathWithSpaces(t *testing.T) {
	root := testutil.TempDir(t)
	dir := filepath.Join(root, "Program Files", "LibreOffice")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "soffice")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+fakeSofficeOK+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	usePathOnly(t, map[string]string{"pandoc": fakePandocOK})
	cfg := config.Config{IngestExtractor: "auto", IngestLibreOfficeCommand: bin}
	if !ingest.LibreOfficeActive(cfg) {
		t.Fatalf("a configured binary under a path with a space must be active: %s", ingest.LibreOfficeInstallHint(cfg))
	}
	if _, err := ingest.NewLibreOfficeExtractor("", bin, "", 30*time.Second).Extract(context.Background(), "a.doc", []byte("x")); err != nil {
		t.Fatalf("Extract via a path with a space: %v", err)
	}
}

// TestLibreOfficeCommand_RelativePath checks that a relative configured binary
// (./tools/soffice) runs in the conversion as it did in the probe: the
// conversion must not resolve it against a different working directory.
func TestLibreOfficeCommand_RelativePath(t *testing.T) {
	root := testutil.TempDir(t)
	if err := os.MkdirAll(filepath.Join(root, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tools", "soffice"), []byte("#!/bin/sh\n"+fakeSofficeOK+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	usePathOnly(t, map[string]string{"pandoc": fakePandocOK})
	t.Chdir(root)
	rel := "." + string(filepath.Separator) + filepath.Join("tools", "soffice")
	if !ingest.LibreOfficeActive(config.Config{IngestLibreOfficeCommand: rel}) {
		t.Fatal("the relative binary must pass its probe")
	}
	if _, err := ingest.NewLibreOfficeExtractor("", rel, "", 30*time.Second).Extract(context.Background(), "a.doc", []byte("x")); err != nil {
		t.Fatalf("Extract via a relative binary: %v", err)
	}
}

// TestLibreOfficeExtract_BinaryGoneAfterProbe checks that a configured binary
// that does not exist (for example removed after its probe) gives an error,
// not a panic: the command never starts, so it has no Process to reap.
func TestLibreOfficeExtract_BinaryGoneAfterProbe(t *testing.T) {
	usePathOnly(t, map[string]string{"pandoc": fakePandocOK})
	tmp := isolateTempDir(t)
	missing := filepath.Join(testutil.TempDir(t), "gone", "soffice")
	_, err := ingest.NewLibreOfficeExtractor("", missing, "", 30*time.Second).Extract(context.Background(), "a.doc", []byte("x"))
	if err == nil {
		t.Fatal("Extract with a missing binary must return an error")
	}
	assertDirEmpty(t, tmp)
}

// TestLibreOfficeConvert_RealSoffice runs the conversion step against a real
// soffice. It needs only LibreOffice (not pandoc): a fake pandoc copies the
// converted .docx out, so a host without pandoc still proves the .doc to .docx
// half with real UTF-8 text. It skips when no soffice or libreoffice is on
// PATH. The .doc fixture is made at test time from HTML, so no binary fixture
// is committed. The extractor's own limit (120s) bounds the conversion, and
// runBoundedFixture bounds the fixture command.
func TestLibreOfficeConvert_RealSoffice(t *testing.T) {
	bin, err := exec.LookPath("soffice")
	if err != nil {
		if bin, err = exec.LookPath("libreoffice"); err != nil {
			t.Skip("skipping: neither soffice nor libreoffice is on PATH (install LibreOffice to run this test)")
		}
	}
	work := testutil.TempDir(t)
	html := filepath.Join(work, "fixture.html")
	page := "<html><head><meta charset=\"utf-8\"></head><body><h1>Архив</h1><p>Описание фильма: Москва, 1991 год.</p></body></html>"
	if err := os.WriteFile(html, []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runBoundedFixture(t, bin, "--headless", "--norestore", "-env:UserInstallation=file://"+filepath.ToSlash(filepath.Join(work, "profile")),
		"--convert-to", "doc:MS Word 97", "--outdir", work, html)
	if err != nil {
		t.Fatalf("make .doc fixture: %v: %s", err, out)
	}
	doc, err := os.ReadFile(filepath.Join(work, "fixture.doc"))
	if err != nil {
		t.Fatalf("read .doc fixture: %v", err)
	}

	docxCopy := filepath.Join(testutil.TempDir(t), "converted.docx")
	t.Setenv("DOCX_COPY", docxCopy)
	copyPandoc := loStubPath + "case \"$1\" in --version) exit 0;; esac\ncp \"$1\" \"$DOCX_COPY\"\nprintf 'copied\\n'\nexit 0"
	pandocDir := loStubDir(t, map[string]string{"pandoc": copyPandoc})
	if _, err := ingest.NewLibreOfficeExtractor("", bin, filepath.Join(pandocDir, "pandoc"), 120*time.Second).Extract(context.Background(), "fixture.doc", doc); err != nil {
		t.Fatalf("Extract with real soffice: %v", err)
	}
	body := docxDocumentXML(t, docxCopy)
	for _, want := range []string{"Архив", "Москва"} {
		if !strings.Contains(body, want) {
			t.Fatalf("word/document.xml misses %q", want)
		}
	}
}

// docxDocumentXML returns word/document.xml of the .docx at path.
func docxDocumentXML(t *testing.T, path string) string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("converted output is not a .docx zip: %v", err)
	}
	defer func() { _ = zr.Close() }()
	for _, f := range zr.File {
		if f.Name != "word/document.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		return string(b)
	}
	t.Fatal("the .docx has no word/document.xml")
	return ""
}

// processRunning reports whether pid is a live process. A zombie (state Z)
// has exited and only waits for its parent to reap it, so it does not count.
func processRunning(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	out, err := exec.Command("/bin/ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false // ps finds no such process
	}
	state := strings.TrimSpace(string(out))
	return state != "" && !strings.HasPrefix(state, "Z")
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

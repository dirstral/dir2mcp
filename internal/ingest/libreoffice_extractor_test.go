//go:build unix

package ingest

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
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
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Fake binaries for the libreoffice engine (spec 0.93.0). The scripts set their
// own PATH because the tests point PATH at the stub directory only, so a real
// soffice or pandoc on the host can never leak into a verdict.

const fakeScriptPath = "PATH=/usr/bin:/bin\nexport PATH\n"

// fakeSofficeOK answers --version, records its arguments, and writes a fake
// .docx named after the input into --outdir, the way soffice --convert-to does.
const fakeSofficeOK = fakeScriptPath + `case "$1" in --version) echo 'LibreOffice 7.6'; exit 0;; esac
out=""; prev=""; last=""
for a in "$@"; do
  if [ "$prev" = "--outdir" ]; then out="$a"; fi
  prev="$a"; last="$a"
done
if [ -n "$ARGS_LOG" ]; then printf '%s\n' "$@" > "$ARGS_LOG"; fi
base=$(basename "$last" .doc)
printf 'FAKEDOCX' > "$out/$base.docx"
exit 0`

// fakePandocOK answers --version and prints Markdown with Cyrillic text for any
// input, proving the .docx step runs and UTF-8 survives.
const fakePandocOK = fakeScriptPath + `case "$1" in --version) echo 'pandoc 3.1'; exit 0;; esac
case "$1" in *.docx) ;; *) echo "unexpected input $1" >&2; exit 9;; esac
printf '# Отчёт\n\nПривет, мир. converted from docx\n'
exit 0`

// writeStubDir writes the named executable scripts into a fresh directory and
// returns it. The file is unix-only (build constraint), so a POSIX sh exists.
func writeStubDir(t *testing.T, scripts map[string]string) string {
	t.Helper()
	dir := testutil.TempDir(t)
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	return dir
}

// usePathOnly points PATH at dir alone for the test.
func usePathOnly(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PATH", dir)
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
	cases := []struct {
		name        string
		scripts     map[string]string
		wantActive  bool
		wantMissing []string
	}{
		{"both present", map[string]string{"soffice": fakeSofficeOK, "pandoc": fakePandocOK}, true, nil},
		{"libreoffice launcher name", map[string]string{"libreoffice": fakeSofficeOK, "pandoc": fakePandocOK}, true, nil},
		{"soffice only", map[string]string{"soffice": fakeSofficeOK}, false, []string{"pandoc"}},
		{"pandoc only", map[string]string{"pandoc": fakePandocOK}, false, []string{"soffice (LibreOffice)"}},
		{"neither", map[string]string{}, false, []string{"soffice (LibreOffice)", "pandoc"}},
		{"soffice fails its probe", map[string]string{"soffice": "exit 3", "pandoc": fakePandocOK}, false, []string{"soffice (LibreOffice)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usePathOnly(t, writeStubDir(t, tc.scripts))
			cfg := config.Config{IngestExtractor: "auto"}
			if got := LibreOfficeActive(cfg); got != tc.wantActive {
				t.Fatalf("LibreOfficeActive = %v, want %v", got, tc.wantActive)
			}
			missing := libreofficeMissing(context.Background(), cfg)
			if strings.Join(missing, ",") != strings.Join(tc.wantMissing, ",") {
				t.Fatalf("missing = %q, want %q", missing, tc.wantMissing)
			}
			hint := LibreOfficeInstallHint(cfg)
			if tc.wantActive {
				if hint != "" {
					t.Fatalf("hint = %q, want empty when both binaries are present", hint)
				}
				return
			}
			for _, m := range tc.wantMissing {
				if !strings.Contains(hint, m) {
					t.Fatalf("hint %q must name the missing binary %q", hint, m)
				}
			}
		})
	}
}

func TestLibreOfficeAvailability_PolicyGates(t *testing.T) {
	usePathOnly(t, writeStubDir(t, map[string]string{"soffice": fakeSofficeOK, "pandoc": fakePandocOK}))
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
		if got := LibreOfficeActive(config.Config{IngestExtractor: policy}); got != want {
			t.Errorf("LibreOfficeActive(policy=%q) = %v, want %v", policy, got, want)
		}
	}
}

func TestDescribeLibreOffice_PinAndAuto(t *testing.T) {
	usePathOnly(t, writeStubDir(t, map[string]string{"soffice": fakeSofficeOK, "pandoc": fakePandocOK}))
	pin := config.Config{IngestExtractor: "libreoffice"}
	if d := DescribeDocumentExtractor(pin); d.Name != "libreoffice" || d.Source != "explicit" {
		t.Fatalf("pin decision = %+v, want libreoffice/explicit", d)
	}
	if ex := DocumentExtractorFromConfig(pin); ex == nil {
		t.Fatal("pin: DocumentExtractorFromConfig returned nil")
	} else if _, ok := ex.(*libreofficeExtractor); !ok {
		t.Fatalf("pin: extractor type = %T, want *libreofficeExtractor", ex)
	}
	if d := DescribeLibreOfficeEngine(config.Config{IngestExtractor: "auto"}); d.Name != "libreoffice" || d.Source != "auto" {
		t.Fatalf("auto engine decision = %+v, want libreoffice/auto", d)
	}
	if d := DescribeLibreOfficeEngine(config.Config{IngestExtractor: "pandoc"}); d.Source != "ineligible" {
		t.Fatalf("pandoc pin engine decision = %+v, want ineligible", d)
	}
}

func TestDescribeLibreOffice_PinWithoutPandocIsDisabled(t *testing.T) {
	usePathOnly(t, writeStubDir(t, map[string]string{"soffice": fakeSofficeOK}))
	d := DescribeDocumentExtractor(config.Config{IngestExtractor: "libreoffice"})
	if d.Name != "" || d.Source != "disabled" || !strings.Contains(d.Reason, "pandoc") {
		t.Fatalf("pin without pandoc = %+v, want disabled naming pandoc", d)
	}
	if DocumentExtractorFromConfig(config.Config{IngestExtractor: "libreoffice"}) != nil {
		t.Fatal("pin without pandoc must build no extractor (no fallback)")
	}
	auto := DescribeLibreOfficeEngine(config.Config{IngestExtractor: "auto"})
	if auto.Source != "disabled" || !strings.Contains(auto.Reason, "pandoc") || strings.Contains(auto.Reason, "soffice") {
		t.Fatalf("auto without pandoc = %+v, want disabled naming only pandoc", auto)
	}
}

func TestActivateLibreOfficeEngine_ServiceRouting(t *testing.T) {
	usePathOnly(t, writeStubDir(t, map[string]string{"soffice": fakeSofficeOK, "pandoc": fakePandocOK}))
	cfg := config.Config{IngestExtractor: "auto", IngestLibreOfficeTimeoutSec: 7}
	s := &Service{cfg: cfg, extractor: NewDoclingExtractor("docling")}
	s.ActivatePandocEngine(cfg)
	s.ActivateLibreOfficeEngine(cfg)
	if s.libreofficeExtractor == nil {
		t.Fatal("libreoffice engine not activated with both binaries present")
	}
	if s.libreofficeExtractor.timeout != 7*time.Second {
		t.Fatalf("timeout = %s, want 7s from ingest.libreoffice.timeout_sec", s.libreofficeExtractor.timeout)
	}
	eng, ok := s.flatEngineForExt(".doc")
	if !ok || eng.name != "libreoffice" {
		t.Fatalf("flatEngineForExt(.doc) = (%+v, %v), want libreoffice", eng, ok)
	}
	if !s.extractorCanReadExt("archive/old.doc") {
		t.Fatal("extractorCanReadExt(.doc) = false with the libreoffice engine active")
	}
	if _, ok := s.flatEngineForExt(".docx"); ok {
		t.Fatal(".docx must stay with docling (T1), not a T2 engine")
	}
	want := derivationIdentity("ocr", "libreoffice", "", "", "")
	if got := s.activeOCRIdentityForPath("old.doc"); got != want {
		t.Fatalf("active OCR identity for .doc = %q, want %q", got, want)
	}

	// Under the libreoffice pin the primary instance is reused, not duplicated.
	pinCfg := config.Config{IngestExtractor: "libreoffice", IngestLibreOfficeTimeoutSec: 7}
	primary := DocumentExtractorFromConfig(pinCfg)
	pinned := &Service{cfg: pinCfg, extractor: primary}
	pinned.ActivateLibreOfficeEngine(pinCfg)
	if pinned.libreofficeExtractor != primary {
		t.Fatal("pin: ActivateLibreOfficeEngine must reuse the primary instance")
	}
	if got := pinned.extractorLabel(); got != "libreoffice" {
		t.Fatalf("pin: extractorLabel = %q, want libreoffice", got)
	}
	if engine, _ := pinned.ActiveExtractionEngine(); engine != "libreoffice" {
		t.Fatalf("pin: ActiveExtractionEngine = %q, want libreoffice", engine)
	}

	// Without soffice the engine is not wired and .doc is not readable.
	usePathOnly(t, writeStubDir(t, map[string]string{"pandoc": fakePandocOK}))
	s2 := &Service{cfg: cfg, extractor: NewDoclingExtractor("docling")}
	s2.ActivateLibreOfficeEngine(cfg)
	if s2.libreofficeExtractor != nil || s2.extractorCanReadExt("old.doc") {
		t.Fatal("libreoffice engine must stay inactive without soffice")
	}
}

func TestLibreOfficeExtract_ConvertsInPrivateTempDir(t *testing.T) {
	stubs := writeStubDir(t, map[string]string{"soffice": fakeSofficeOK, "pandoc": fakePandocOK})
	usePathOnly(t, stubs)
	argsLog := filepath.Join(testutil.TempDir(t), "args.txt")
	t.Setenv("ARGS_LOG", argsLog)
	tmp := isolateTempDir(t)

	ex := NewLibreOfficeExtractor("", "", "", 30*time.Second)
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
		if strings.HasPrefix(a, "-env:UserInstallation=file://") {
			profile = a
		}
	}
	if profile == "" {
		t.Errorf("soffice args %q carry no per-conversion -env:UserInstallation profile", args)
	}
	if last := args[len(args)-1]; filepath.Base(last) != libreofficeInputName || strings.Contains(last, "letters") {
		t.Errorf("input arg %q must be the private copy, never the source path", last)
	}
	assertDirEmpty(t, tmp)
}

func TestLibreOfficeExtract_ParallelProfilesDiffer(t *testing.T) {
	usePathOnly(t, writeStubDir(t, map[string]string{"soffice": fakeSofficeOK, "pandoc": fakePandocOK}))
	logs := testutil.TempDir(t)
	profiles := map[string]bool{}
	for i := 0; i < 2; i++ {
		log := filepath.Join(logs, strconv.Itoa(i))
		t.Setenv("ARGS_LOG", log)
		if _, err := NewLibreOfficeExtractor("", "", "", 30*time.Second).Extract(context.Background(), "a.doc", []byte("x")); err != nil {
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
		{"non-zero exit", fakeScriptPath + "case \"$1\" in --version) exit 0;; esac\necho 'Error: source file could not be loaded' >&2\nexit 1", "source file could not be loaded"},
		{"exit 0 without output", fakeScriptPath + "exit 0", "produced no .docx output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usePathOnly(t, writeStubDir(t, map[string]string{"soffice": tc.soffice, "pandoc": fakePandocOK}))
			tmp := isolateTempDir(t)
			_, err := NewLibreOfficeExtractor("", "", "", 30*time.Second).Extract(context.Background(), "bad.doc", []byte("x"))
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
	hang := fakeScriptPath + "sleep 60 &\necho $! > \"$CHILD_PID\"\nwait\n"
	usePathOnly(t, writeStubDir(t, map[string]string{"soffice": hang, "pandoc": fakePandocOK}))
	t.Setenv("CHILD_PID", pidFile)
	tmp := isolateTempDir(t)

	start := time.Now()
	_, err := NewLibreOfficeExtractor("", "", "", time.Second).Extract(context.Background(), "slow.doc", []byte("x"))
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("timeout took %s, want it bounded near the 1s limit", elapsed)
	}
	var te *libreofficeTimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("Extract error = %v, want a libreofficeTimeoutError", err)
	}
	if !strings.Contains(err.Error(), "ingest.libreoffice.timeout_sec=1") {
		t.Fatalf("timeout error %q must name the setting", err)
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
	hungPandoc := fakeScriptPath + "case \"$1\" in --version) exit 0;; esac\nexec sleep 60"
	usePathOnly(t, writeStubDir(t, map[string]string{"soffice": fakeSofficeOK, "pandoc": hungPandoc}))
	tmp := isolateTempDir(t)
	start := time.Now()
	_, err := NewLibreOfficeExtractor("", "", "", time.Second).Extract(context.Background(), "a.doc", []byte("x"))
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("pandoc step took %s, want it bounded by the 1s libreoffice limit", elapsed)
	}
	var te *libreofficeTimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("Extract error = %v, want a libreofficeTimeoutError", err)
	}
	assertDirEmpty(t, tmp)
}

// TestLibreOfficeCommandOverride checks ingest.libreoffice.command: it names
// the binary, it wins over PATH, and the probe runs on it.
func TestLibreOfficeCommandOverride(t *testing.T) {
	custom := writeStubDir(t, map[string]string{"my-office": fakeSofficeOK})
	usePathOnly(t, writeStubDir(t, map[string]string{"pandoc": fakePandocOK}))
	cfg := config.Config{IngestExtractor: "auto", IngestLibreOfficeCommand: filepath.Join(custom, "my-office")}
	bin, ok := resolveSofficeBinary(cfg)
	if !ok || bin != filepath.Join(custom, "my-office") {
		t.Fatalf("resolveSofficeBinary = (%q, %v), want the configured binary", bin, ok)
	}
	if !LibreOfficeActive(cfg) {
		t.Fatalf("engine inactive with a functional configured binary: %s", LibreOfficeInstallHint(cfg))
	}
	md, err := NewLibreOfficeExtractor("", cfg.IngestLibreOfficeCommand, "", 30*time.Second).Extract(context.Background(), "a.doc", []byte("x"))
	if err != nil || !strings.Contains(md, "Привет") {
		t.Fatalf("Extract via the configured binary = (%q, %v)", md, err)
	}
	broken := writeStubDir(t, map[string]string{"bad-office": "exit 2"})
	if LibreOfficeActive(config.Config{IngestLibreOfficeCommand: filepath.Join(broken, "bad-office")}) {
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
	usePathOnly(t, writeStubDir(t, map[string]string{"pandoc": fakePandocOK}))
	cfg := config.Config{IngestExtractor: "auto", IngestLibreOfficeCommand: bin}
	if !LibreOfficeActive(cfg) {
		t.Fatalf("a configured binary under a path with a space must be active: %s", LibreOfficeInstallHint(cfg))
	}
	if _, err := NewLibreOfficeExtractor("", bin, "", 30*time.Second).Extract(context.Background(), "a.doc", []byte("x")); err != nil {
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
	usePathOnly(t, writeStubDir(t, map[string]string{"pandoc": fakePandocOK}))
	t.Chdir(root)
	rel := "." + string(filepath.Separator) + filepath.Join("tools", "soffice")
	if !LibreOfficeActive(config.Config{IngestLibreOfficeCommand: rel}) {
		t.Fatal("the relative binary must pass its probe")
	}
	if _, err := NewLibreOfficeExtractor("", rel, "", 30*time.Second).Extract(context.Background(), "a.doc", []byte("x")); err != nil {
		t.Fatalf("Extract via a relative binary: %v", err)
	}
}

// TestLibreOfficeExtract_BinaryGoneAfterProbe checks that a configured binary
// that does not exist (for example removed after its probe) gives an error,
// not a panic: the command never starts, so it has no Process to reap.
func TestLibreOfficeExtract_BinaryGoneAfterProbe(t *testing.T) {
	usePathOnly(t, writeStubDir(t, map[string]string{"pandoc": fakePandocOK}))
	tmp := isolateTempDir(t)
	missing := filepath.Join(testutil.TempDir(t), "gone", "soffice")
	_, err := NewLibreOfficeExtractor("", missing, "", 30*time.Second).Extract(context.Background(), "a.doc", []byte("x"))
	if err == nil {
		t.Fatal("Extract with a missing binary must return an error")
	}
	assertDirEmpty(t, tmp)
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

// TestLibreOfficeConvert_RealSoffice runs the conversion step against a real
// soffice. It needs only LibreOffice (not pandoc), so a host without pandoc
// still proves the .doc to .docx half with real UTF-8 text. It skips when no
// soffice or libreoffice is on PATH. The .doc fixture is made at test time from
// HTML, so no binary fixture is committed.
func TestLibreOfficeConvert_RealSoffice(t *testing.T) {
	bin, ok := resolveSofficeBinary(config.Config{})
	if !ok {
		t.Skip("skipping: neither soffice nor libreoffice is on PATH (install LibreOffice to run this test)")
	}
	work := testutil.TempDir(t)
	html := filepath.Join(work, "fixture.html")
	page := "<html><head><meta charset=\"utf-8\"></head><body><h1>Архив</h1><p>Описание фильма: Москва, 1991 год.</p></body></html>"
	if err := os.WriteFile(html, []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	// The fixture command is bounded and stoppable as a tree, like the
	// production conversion, so a stalled LibreOffice cannot hang the test.
	fctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	mk := exec.CommandContext(fctx, bin, "--headless", "--norestore", "-env:UserInstallation="+fileURL(filepath.Join(work, "profile")),
		"--convert-to", "doc:MS Word 97", "--outdir", work, html)
	prepareLibreOfficeCmd(mk)
	mk.WaitDelay = pandocWaitDelay
	out, err := mk.CombinedOutput()
	reapLibreOfficeGroup(mk)
	if err != nil {
		t.Fatalf("make .doc fixture: %v: %s", err, out)
	}
	doc, err := os.ReadFile(filepath.Join(work, "fixture.doc"))
	if err != nil {
		t.Fatalf("read .doc fixture: %v", err)
	}

	convWork := testutil.TempDir(t)
	l := NewLibreOfficeExtractor("", "", "", 120*time.Second)
	docx, err := l.convertToDocx(context.Background(), bin, convWork, doc)
	if err != nil {
		t.Fatalf("convertToDocx: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(docx), int64(len(docx)))
	if err != nil {
		t.Fatalf("converted output is not a .docx zip: %v", err)
	}
	var body string
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
		body = string(b)
	}
	for _, want := range []string{"Архив", "Москва"} {
		if !strings.Contains(body, want) {
			t.Fatalf("word/document.xml misses %q", want)
		}
	}
}

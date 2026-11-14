package tests

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Spec 0.93.0 §7.4.B: the LibreOffice profile and output directories MUST be
// outside the corpus root. When the system temporary directory is inside the
// root, the engine MUST use another location or be unavailable. Both
// directories MUST be owner-only (0700 on POSIX).

// loRecordingSoffice logs the work, profile and output directories with their
// `ls -ld` lines to $LO_TEST_LOG, then writes the .docx like soffice does. The
// directories are gone after the extraction, so the stub records them while
// they exist.
const loRecordingSoffice = loStubPath + `case "$1" in --version) echo 'LibreOffice 7.6'; exit 0;; esac
out=""; prev=""; last=""; profile=""
for a in "$@"; do
  case "$a" in -env:UserInstallation=file://*) profile="${a#-env:UserInstallation=file://}";; esac
  if [ "$prev" = "--outdir" ]; then out="$a"; fi
  prev="$a"; last="$a"
done
work=$(dirname "$out")
{
  echo "work $work"
  echo "profile $profile"
  echo "out $out"
  echo "mode $(ls -ld "$work" | cut -c1-10)"
  echo "mode $(ls -ld "$profile" | cut -c1-10)"
  echo "mode $(ls -ld "$out" | cut -c1-10)"
} >> "$LO_TEST_LOG"
printf 'FAKEDOCX' > "$out/$(basename "$last" .doc).docx"
exit 0`

// loRecordingPandoc logs the input path it reads to $LO_TEST_LOG.
const loRecordingPandoc = loStubPath + `case "$1" in --version) echo 'pandoc 3.1'; exit 0;; esac
echo "pandoc $1" >> "$LO_TEST_LOG"
printf '# Notes\n\nLegacy doc text.\n'
exit 0`

// loLog parses the stub log into its "key value" lines.
func loLog(t *testing.T, path string) map[string][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string][]string{}
		}
		t.Fatalf("open stub log: %v", err)
	}
	defer func() { _ = f.Close() }()
	out := map[string][]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, _ := strings.Cut(sc.Text(), " ")
		out[key] = append(out[key], val)
	}
	return out
}

// loTempEnv makes a corpus root with a tmp directory inside it, points TMPDIR
// there, and points the user cache directory (HOME and XDG_CACHE_HOME) at
// cacheHome. It returns the root, the in-root tmp directory and the stub log
// path (outside the root).
func loTempEnv(t *testing.T, root, cacheHome string) (tmpInRoot, logPath string) {
	t.Helper()
	tmpInRoot = filepath.Join(root, "tmp")
	for _, dir := range []string{
		tmpInRoot,
		filepath.Join(cacheHome, "Library", "Caches"), // os.UserCacheDir on darwin
		filepath.Join(cacheHome, ".cache"),            // os.UserCacheDir on linux
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	logPath = filepath.Join(testutil.TempDir(t), "stub.log")
	t.Setenv("LO_TEST_LOG", logPath)
	t.Setenv("HOME", cacheHome)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(cacheHome, ".cache"))
	t.Setenv("TMPDIR", tmpInRoot)
	return tmpInRoot, logPath
}

// TestLibreOfficeEngine_TempDirInsideRootUsesCacheDir: TMPDIR is inside the
// corpus root, so the engine makes its work directory in the user cache
// directory instead. No LibreOffice or pandoc file is written below the root,
// and the work, profile and output directories are mode 0700.
func TestLibreOfficeEngine_TempDirInsideRootUsesCacheDir(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "")
	const rel = "old/notes.doc"
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, rel), "legacy word bytes")
	cfg := config.Config{RootDir: root, StateDir: testutil.TempDir(t), STTProvider: "off", IngestExtractor: "auto", IngestLibreOfficeTimeoutSec: 30}
	st := newRealStore(t)
	stubs := loStubDir(t, map[string]string{"pandoc": loRecordingPandoc, "soffice": loRecordingSoffice})
	cacheHome := testutil.TempDir(t)
	tmpInRoot, logPath := loTempEnv(t, root, cacheHome)
	t.Setenv("PATH", stubs)
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("UserCacheDir: %v", err)
	}

	if !ingest.LibreOfficeActive(cfg) {
		t.Fatalf("engine inactive although the user cache directory is outside the root: %+v", ingest.DescribeLibreOfficeEngine(cfg))
	}
	f := ingest.DiscoveredFile{RelPath: rel, SizeBytes: 17, MTimeUnix: time.Now().Unix()}
	if err := wiredService(t, cfg, st).ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	if doc := documentByPath(t, st, rel); doc.Status != string(model.DocumentStatusOK) {
		t.Fatalf("status = %q (%s), want ok", doc.Status, doc.ErrorMessage)
	}

	log := loLog(t, logPath)
	assertWorkDirsOutsideRoot(t, log, root, cacheDir)
	assertTreeHolds(t, tmpInRoot, nil)
	assertTreeHolds(t, root, []string{"old", "old/notes.doc", "tmp"})
	// The work directory is removed after the read.
	if _, err := os.Stat(log["work"][0]); !os.IsNotExist(err) {
		t.Fatalf("work directory %s still exists after the extraction (err=%v)", log["work"][0], err)
	}
}

// assertWorkDirsOutsideRoot checks the stub log of one conversion: the work,
// profile, output and pandoc input paths are in cacheDir and not inside root,
// and the three directories are mode 0700.
func assertWorkDirsOutsideRoot(t *testing.T, log map[string][]string, root, cacheDir string) {
	t.Helper()
	if len(log["work"]) != 1 || len(log["profile"]) != 1 || len(log["out"]) != 1 || len(log["pandoc"]) != 1 {
		t.Fatalf("stub log does not show one conversion and one pandoc read: %v", log)
	}
	for _, key := range []string{"work", "profile", "out", "pandoc"} {
		p := log[key][0]
		if config.PathInsideRoot(p, root) {
			t.Fatalf("%s path %s is inside the corpus root %s", key, p, root)
		}
		if !config.PathInsideRoot(p, cacheDir) {
			t.Fatalf("%s path %s is not in the user cache directory %s", key, p, cacheDir)
		}
	}
	if got := log["mode"]; len(got) != 3 {
		t.Fatalf("modes = %v, want 3", got)
	}
	for i, mode := range log["mode"] {
		if mode != "drwx------" {
			t.Fatalf("directory %d mode = %q, want drwx------ (0700)", i, mode)
		}
	}
}

// TestLibreOfficeEngine_NoTempDirOutsideRootIsUnavailable: TMPDIR and the
// user cache directory are both inside the corpus root, so the engine is
// unavailable. The .doc is a coverage gap (unsupported_format skip), soffice
// never converts anything, the remedy names TMPDIR, and nothing is written
// below the root.
func TestLibreOfficeEngine_NoTempDirOutsideRootIsUnavailable(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "")
	const rel = "notes.doc"
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, rel), "legacy word bytes")
	cfg := config.Config{RootDir: root, StateDir: testutil.TempDir(t), STTProvider: "off", IngestExtractor: "auto", IngestLibreOfficeTimeoutSec: 30}
	st := newRealStore(t)
	stubs := loStubDir(t, map[string]string{"pandoc": loRecordingPandoc, "soffice": loRecordingSoffice})
	home := filepath.Join(root, "home")
	_, logPath := loTempEnv(t, root, home)
	t.Setenv("PATH", stubs)

	if ingest.LibreOfficeActive(cfg) {
		t.Fatal("engine active although no temporary directory outside the root is usable")
	}
	d := ingest.DescribeLibreOfficeEngine(cfg)
	if d.Source != "disabled" || !strings.Contains(d.Reason, "TMPDIR") {
		t.Fatalf("decision = %+v, want disabled with a TMPDIR remedy", d)
	}
	if hint := ingest.LibreOfficeInstallHint(cfg); !strings.Contains(hint, "TMPDIR") {
		t.Fatalf("install hint = %q, want the TMPDIR remedy", hint)
	}
	pinned := cfg
	pinned.IngestExtractor = "libreoffice"
	if d := ingest.DescribeDocumentExtractor(pinned); d.Name != "" || !strings.Contains(d.Reason, "TMPDIR") {
		t.Fatalf("pinned decision = %+v, want disabled with a TMPDIR remedy", d)
	}

	f := ingest.DiscoveredFile{RelPath: rel, SizeBytes: 17, MTimeUnix: time.Now().Unix()}
	if err := wiredService(t, cfg, st).ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	doc := documentByPath(t, st, rel)
	if doc.Status != string(model.DocumentStatusSkipped) || doc.SkipReason != model.SkipReasonUnsupportedFormat {
		t.Fatalf("status=%q skip_reason=%q, want skipped/%s", doc.Status, doc.SkipReason, model.SkipReasonUnsupportedFormat)
	}
	if log := loLog(t, logPath); len(log) != 0 {
		t.Fatalf("soffice or pandoc ran although the engine is unavailable: %v", log)
	}
	assertTreeHolds(t, root, []string{
		"home", "home/.cache", "home/Library", "home/Library/Caches",
		"notes.doc", "tmp",
	})
}

// assertTreeHolds fails when the tree below dir holds any entry other than
// want (slash-separated paths relative to dir).
func assertTreeHolds(t *testing.T, dir string, want []string) {
	t.Helper()
	allowed := map[string]bool{}
	for _, w := range want {
		allowed[w] = true
	}
	var extra []string
	err := filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		if rel = filepath.ToSlash(rel); !allowed[rel] {
			extra = append(extra, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	if len(extra) > 0 {
		t.Fatalf("unexpected entries below %s: %v", dir, extra)
	}
}

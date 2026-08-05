package tests

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/store"
)

// buildZipOrdered writes members in the given order, allowing duplicate or
// hostile names that a map literal cannot express.
func buildZipOrdered(t *testing.T, names []string, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, name := range names {
		fw, err := w.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// buildTarGzOrdered is buildTarGz with an explicit member order.
func buildTarGzOrdered(t *testing.T, names []string, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, name := range names {
		data := []byte(content)
		hdr := &tar.Header{Name: name, Size: int64(len(data)), Typeflag: tar.TypeReg, Mode: 0o644}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %q: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("tar write %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// TestArchiveMember_BenignDoubleDotNamesIndexed is the #718 regression guard for
// the false-positive direction: `..` INSIDE a path segment is an ordinary
// filename, not traversal. The old check rejected any `..` substring, so these
// members were dropped from the corpus with no diagnostic at all.
func TestArchiveMember_BenignDoubleDotNamesIndexed(t *testing.T) {
	benign := []string{
		"v1..v2.txt",
		"draft..final/report.md",
		"...leading.txt",
		"notes...md",
	}

	t.Run("zip", func(t *testing.T) {
		st := runArchiveIngest(t, "docs.zip", buildZipOrdered(t, benign, "content"))
		paths := docPaths(t, st)
		for _, name := range benign {
			if !paths["docs.zip/"+name] {
				t.Errorf("member %q must be indexed (a `..` inside a segment is not traversal); got %v", name, paths)
			}
		}
	})

	t.Run("tar.gz", func(t *testing.T) {
		st := runArchiveIngest(t, "bundle.tar.gz", buildTarGzOrdered(t, benign, "content"))
		paths := docPaths(t, st)
		for _, name := range benign {
			if !paths["bundle.tar.gz/"+name] {
				t.Errorf("member %q must be indexed (a `..` inside a segment is not traversal); got %v", name, paths)
			}
		}
	})
}

// TestArchiveMember_SingleCompressedNamePreserved covers the third code path
// named in #718: a bare `.gz`/`.bz2` payload derives its member name from the
// archive's own filename, and the old `..` substring check replaced a perfectly
// safe stem with the synthetic "member", losing the name in retrieval.
func TestArchiveMember_SingleCompressedNamePreserved(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write([]byte("gzipped body")); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	st := runArchiveIngest(t, "report..final.txt.gz", buf.Bytes())
	paths := docPaths(t, st)
	want := "report..final.txt.gz/report..final.txt"
	if !paths[want] {
		t.Errorf("want member %q (the safe stem must be preserved); got %v", want, paths)
	}
	if paths["report..final.txt.gz/member"] {
		t.Errorf("member name was replaced with the synthetic fallback; got %v", paths)
	}
}

// TestArchiveMember_DotSlashPrefixedTarMembersIndexed pins the shape `tar -czf
// archive.tgz .` produces for every member (`./a.txt`). The member is safe, so
// it must be indexed, and under a clean rel_path, since `archive.tgz/./a.txt`
// is not a path any other part of the system can produce or resolve.
//
// This one passes before the fix too, and that is the point: it is the guard
// that stops the traversal rule from being tightened into relpath.Normalize's
// reject-don't-clean form, which would refuse every member of an ordinary
// `tar -czf x.tgz .` tarball. The rel_path was already being cleaned one layer
// down by the store; the extractor now makes that explicit at admission time.
func TestArchiveMember_DotSlashPrefixedTarMembersIndexed(t *testing.T) {
	st := runArchiveIngest(t, "tree.tar.gz", buildTarGzOrdered(t, []string{"./a.txt", "./sub/b.txt"}, "content"))
	paths := docPaths(t, st)
	for _, want := range []string{"tree.tar.gz/a.txt", "tree.tar.gz/sub/b.txt"} {
		if !paths[want] {
			t.Errorf("want member %q indexed under a clean rel_path; got %v", want, paths)
		}
	}
}

// hostileMemberNames are the traversal shapes archive extraction must keep
// refusing after #718 relaxes the `..`-substring test. Relaxing the check must
// not open a zip-slip hole.
//
// Note the deliberately strict entries: `a/../b.txt` does not escape the archive
// root, and `C:/Windows/x.txt` is only a traversal on a platform dir2mcp does not
// ship binaries for. Both are refused anyway: a member name is refused, never
// guessed at, when it does not mean exactly one thing.
var hostileMemberNames = []string{
	"../escape.txt",
	"../../etc/passwd",
	"a/../../escape.txt",
	"a/b/../../../escape.txt",
	"a/../b.txt",
	"..",
	"/absolute.txt",
	"//absolute.txt",
	`..\escape.txt`,
	`a\..\..\escape.txt`,
	`\\server\share\escape.txt`,
	`C:\Windows\escape.txt`,
	"sub/../../..",
	"   ",
}

// assertOnlyContainedDocs fails when any document other than the archive itself
// and the known-safe member exists: a refused member must produce no document at
// all, under any name.
func assertOnlyContainedDocs(t *testing.T, st *store.SQLiteStore, archive, hostile string) {
	t.Helper()
	paths := docPaths(t, st)
	if !paths[archive+"/safe.txt"] {
		t.Errorf("refusing %q must not drop the safe member in the same archive; got %v", hostile, paths)
	}
	for p := range paths {
		if p == archive || p == archive+"/safe.txt" {
			continue // the archive container itself and the safe member
		}
		t.Errorf("hostile member %q produced document %q; it must be refused entirely", hostile, p)
	}
}

// TestArchiveMember_TraversalShapesRefused_Zip is the other direction of #718.
func TestArchiveMember_TraversalShapesRefused_Zip(t *testing.T) {
	for _, name := range hostileMemberNames {
		t.Run(name, func(t *testing.T) {
			st := runArchiveIngest(t, "test.zip", buildZipOrdered(t, []string{name, "safe.txt"}, "content"))
			assertOnlyContainedDocs(t, st, "test.zip", name)
		})
	}
}

// TestArchiveMember_TraversalShapesRefused_Tar covers the same shapes on the tar
// path. (`../` itself is not in the table: neither writer will encode a regular
// member whose name ends in a slash, so it is not a shape a real archive can
// present; `..` and `../escape.txt` cover the intent.)
func TestArchiveMember_TraversalShapesRefused_Tar(t *testing.T) {
	for _, name := range hostileMemberNames {
		t.Run(name, func(t *testing.T) {
			st := runArchiveIngest(t, "test.tar.gz", buildTarGzOrdered(t, []string{name, "safe.txt"}, "content"))
			assertOnlyContainedDocs(t, st, "test.tar.gz", name)
		})
	}
}

// TestCorpusFile_LeadingDotsInSubdirIndexed pins the second half of the #718
// defect, found while fixing the first: the STORE's rel_path check used the same
// substring test, so an ordinary corpus file in a subdirectory whose name starts
// with two dots (`sub/...notes.md`, `sub/..draft.txt`) was refused on upsert.
// No archive is involved (this is a plain local corpus), and the failure was
// invisible: no document row, no path in the log, just an anonymous +1 on the
// error counter.
func TestCorpusFile_LeadingDotsInSubdirIndexed(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	files := []string{
		"ok.txt",
		"sub/...notes.md",
		"sub/..draft.txt",
		"sub/deeper/...more.txt",
		"v1..v2.txt",
	}
	for _, name := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(full, []byte("hello"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	cfg := config.Default()
	cfg.RootDir = root
	cfg.STTProvider = "off"
	svc := mustNewIngestService(t, cfg, st)
	state := appstate.NewIndexingState(appstate.ModeIncremental)
	svc.SetIndexingState(state)
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	paths := docPaths(t, st)
	for _, name := range files {
		if !paths[name] {
			t.Errorf("corpus file %q must be indexed (a `..` inside a name is not traversal); got %v", name, paths)
		}
	}
	if got := state.Snapshot().Errors; got != 0 {
		t.Errorf("indexing errors = %d, want 0 (a legal filename must not fail its upsert)", got)
	}
}

// TestArchiveMember_RefusalIsObservable pins the #718 observability requirement:
// dropping a member is an admission decision, and an admission decision that
// leaves no trace is indistinguishable from an archive that never held the file.
// Precedent: Options.OnUnsafeKey (#735) and OnOversize (#497).
func TestArchiveMember_RefusalIsObservable(t *testing.T) {
	var logBuf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	ctx := context.Background()
	root := t.TempDir()
	data := buildZipOrdered(t, []string{"../../etc/passwd", "safe.txt"}, "content")
	if err := os.WriteFile(filepath.Join(root, "test.zip"), data, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	cfg := config.Default()
	cfg.RootDir = root
	cfg.STTProvider = "off"
	svc := mustNewIngestService(t, cfg, st)
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "test.zip") {
		t.Errorf("refusal log did not name the archive; got: %q", logged)
	}
	if !strings.Contains(logged, "../../etc/passwd") {
		t.Errorf("refusal log did not name the refused member; got: %q", logged)
	}
}

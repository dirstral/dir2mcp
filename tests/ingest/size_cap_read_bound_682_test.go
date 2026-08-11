package tests

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Bounded source reads (dir2mcp #682).
//
// `ingest.max_file_mb` was enforced at discovery and nowhere else. Discovery
// measured a file with a stat (local) or a ListObjectsV2 entry (S3), admitted it
// under the cap, and the pipeline then read the bytes with an unbounded
// io.ReadAll. Nothing tied the bytes read to the size that had been checked.
//
// So a file that grew after the check, or an object that never matched the size it
// was listed with, was read in full. What an operator saw was resident memory
// climbing during a scan and, on a large enough file, the daemon killed by the OOM
// killer mid-scan: indexing stopped, `status` never reached pending=0, and the log
// named no file, because the process died inside the read.
//
// Every test here asserts on TWO things, and both matter. The row the scan
// persisted, which is the honest-coverage answer; and the number of bytes the
// source was actually asked for, which is the bound. A re-check of the size could
// produce the first without the second, and the second is the defect.

// readCap682 is the cap the tests configure, in MiB and in bytes. It is the
// smallest cap `ingest.max_file_mb` can express, which keeps the over-cap fixtures
// small.
const (
	readCapMB682    = 1
	readCapBytes682 = int64(readCapMB682) * 1024 * 1024
)

// overCapBytes682 is the size of the fixtures that must trip the bound.
const overCapBytes682 = 4 * readCapBytes682

// countingReader682 wraps a reader and records how many bytes were pulled through
// it. It is the whole proof: with the bound in place the scan must stop asking for
// bytes at cap+1, however many the source is willing to serve.
type countingReader682 struct {
	inner   io.ReadSeekCloser
	counter *atomic.Int64
}

func (c *countingReader682) Read(p []byte) (int, error) {
	n, err := c.inner.Read(p)
	c.counter.Add(int64(n))
	return n, err
}

func (c *countingReader682) Seek(offset int64, whence int) (int64, error) {
	return c.inner.Seek(offset, whence)
}

func (c *countingReader682) Close() error { return c.inner.Close() }

// generatedFile682 serves n bytes of ordinary text without holding them in
// memory. It also satisfies io.Seeker so it can stand in for a CorpusFS reader;
// only the position query (Seek(0, io.SeekCurrent)) is supported, which is all a
// sequential whole-file read needs.
type generatedFile682 struct {
	remaining int64
	offset    int64
}

func (g *generatedFile682) Read(p []byte) (int, error) {
	if g.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > g.remaining {
		p = p[:g.remaining]
	}
	for i := range p {
		p[i] = 'a'
	}
	g.remaining -= int64(len(p))
	g.offset += int64(len(p))
	return len(p), nil
}

func (g *generatedFile682) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekCurrent && offset == 0 {
		return g.offset, nil
	}
	return 0, os.ErrInvalid
}

func (g *generatedFile682) Close() error { return nil }

// underReportingFS682 is a stub corpus backend that LIES about a file's size: Walk
// reports a handful of bytes, Open serves megabytes. It stands in for the S3
// backend without credentials or a network, and it is the case a size re-check
// cannot catch, because the only size a re-check can read is the one the backend
// reports.
type underReportingFS682 struct {
	relPath     string
	reportedLen int64
	bodyLen     int64
	bytesServed atomic.Int64
}

func (f *underReportingFS682) Walk(_ context.Context, _ string, _ corpusfs.Options) ([]corpusfs.DiscoveredFile, error) {
	return []corpusfs.DiscoveredFile{{
		RelPath:   f.relPath,
		SizeBytes: f.reportedLen,
		ETag:      "etag-682",
	}}, nil
}

func (f *underReportingFS682) Open(_ context.Context, relPath string) (io.ReadSeekCloser, error) {
	if relPath != f.relPath {
		return nil, os.ErrNotExist
	}
	return &countingReader682{inner: &generatedFile682{remaining: f.bodyLen}, counter: &f.bytesServed}, nil
}

func (f *underReportingFS682) Localize(_ context.Context, _ string) (string, func(), error) {
	return "", func() {}, os.ErrNotExist
}

// growOnOpenFS682 is a real local corpus with a race built into it: it walks the
// file at its original size, then appends bytes to it before the pipeline opens it.
// That is the local growth race, reproduced deterministically at the exact point
// between the check and the use.
type growOnOpenFS682 struct {
	inner       corpusfs.CorpusFS
	root        string
	target      string
	extra       int64
	grown       atomic.Bool
	bytesServed atomic.Int64
}

func (f *growOnOpenFS682) Walk(ctx context.Context, root string, opts corpusfs.Options) ([]corpusfs.DiscoveredFile, error) {
	return f.inner.Walk(ctx, root, opts)
}

func (f *growOnOpenFS682) Open(ctx context.Context, relPath string) (io.ReadSeekCloser, error) {
	if relPath == f.target && f.grown.CompareAndSwap(false, true) {
		file, err := os.OpenFile(filepath.Join(f.root, filepath.FromSlash(relPath)), os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
		if _, err := file.Write([]byte(strings.Repeat("b", int(f.extra)))); err != nil {
			_ = file.Close()
			return nil, err
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
	}
	rc, err := f.inner.Open(ctx, relPath)
	if err != nil {
		return nil, err
	}
	return &countingReader682{inner: rc, counter: &f.bytesServed}, nil
}

func (f *growOnOpenFS682) Localize(ctx context.Context, relPath string) (string, func(), error) {
	return f.inner.Localize(ctx, relPath)
}

// capConfig682 returns a config with the 1 MiB read cap and no providers wired.
func capConfig682(root, stateDir string) config.Config {
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = stateDir
	cfg.STTProvider = "off"
	cfg.IngestMaxFileMB = readCapMB682
	return cfg
}

// assertSizeCapSkip682 fails unless the document is recorded as the SPEC §15.2
// `size_cap` skip and has nothing left that retrieval can return.
func assertSizeCapSkip682(t *testing.T, st *store.SQLiteStore, relPath string) {
	t.Helper()
	doc, err := st.GetDocumentByPath(context.Background(), relPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath(%s): %v", relPath, err)
	}
	if doc.Status != "skipped" {
		t.Errorf("%s status = %q, want skipped", relPath, doc.Status)
	}
	if doc.SkipReason != model.SkipReasonSizeCap {
		t.Errorf("%s skip_reason = %q, want %q", relPath, doc.SkipReason, model.SkipReasonSizeCap)
	}
	reps, err := st.ActiveRepresentations(context.Background(), relPath)
	if err != nil {
		t.Fatalf("ActiveRepresentations(%s): %v", relPath, err)
	}
	if len(reps) != 0 {
		t.Errorf("%s still has %d live representation(s) %+v; a size_cap row must not serve chunks", relPath, len(reps), reps)
	}
}

// liveRepTypes682 lists the rep types still searchable for a path.
func liveRepTypes682(t *testing.T, st *store.SQLiteStore, relPath string) []string {
	t.Helper()
	reps, err := st.ActiveRepresentations(context.Background(), relPath)
	if err != nil {
		t.Fatalf("ActiveRepresentations(%s): %v", relPath, err)
	}
	types := make([]string, 0, len(reps))
	for _, rep := range reps {
		types = append(types, rep.RepType)
	}
	sort.Strings(types)
	return types
}

// TestSizeCapRead_UnderReportingSourceIsBoundedAndSkipped is the case a re-check
// cannot catch. The backend reports 512 bytes and serves 4 MiB under a 1 MiB cap,
// so every size the pipeline could check says the file fits.
//
// On `main` the scan reads all 4 MiB and records the document as `ok`.
func TestSizeCapRead_UnderReportingSourceIsBoundedAndSkipped(t *testing.T) {
	root := t.TempDir()
	fs := &underReportingFS682{relPath: "liar.txt", reportedLen: 512, bodyLen: overCapBytes682}
	st := newRealStore(t)

	svc := mustNewIngestService(t, capConfig682(root, t.TempDir()), st)
	svc.SetCorpusFS(fs)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if served := fs.bytesServed.Load(); served > readCapBytes682+1 {
		t.Errorf("the scan pulled %d bytes from a source that reported %d; the bound is %d (cap+1)", served, fs.reportedLen, readCapBytes682+1)
	}
	assertSizeCapSkip682(t, st, "liar.txt")
}

// TestSizeCapRead_LocalGrowthAfterDiscoveryIsBoundedAndSkipped is the local half:
// the file is 12 bytes when discovery stats it and 4 MiB when the pipeline opens
// it. This is the time-of-check/time-of-use window itself, closed at the read.
//
// On `main` the scan reads the whole grown file and records it as `ok`.
func TestSizeCapRead_LocalGrowthAfterDiscoveryIsBoundedAndSkipped(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "notes.txt"), "small so far")
	fs := &growOnOpenFS682{
		inner:  corpusfs.NewLocalFS(root),
		root:   root,
		target: "notes.txt",
		extra:  overCapBytes682,
	}
	st := newRealStore(t)

	svc := mustNewIngestService(t, capConfig682(root, t.TempDir()), st)
	svc.SetCorpusFS(fs)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if served := fs.bytesServed.Load(); served > readCapBytes682+1 {
		t.Errorf("the scan pulled %d bytes from a file discovery measured at 12; the bound is %d (cap+1)", served, readCapBytes682+1)
	}
	assertSizeCapSkip682(t, st, "notes.txt")
}

// TestSizeCapRead_CountsAsSkipNotError pins the accounting. An over-cap file is
// refused by policy, not by failure, so it is one skip and never an error. Mixing
// the two would break `scanned = indexed + skipped + errors` and would put the
// file in `recent_failures`, where an operator would look for a bug that is not
// there.
//
// On `main` the file is counted as indexed.
func TestSizeCapRead_CountsAsSkipNotError(t *testing.T) {
	root := t.TempDir()
	fs := &underReportingFS682{relPath: "liar.txt", reportedLen: 512, bodyLen: overCapBytes682}
	st := newRealStore(t)

	svc := mustNewIngestService(t, capConfig682(root, t.TempDir()), st)
	svc.SetCorpusFS(fs)
	state := appstate.NewIndexingState(appstate.ModeIncremental)
	svc.SetIndexingState(state)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	snap := state.Snapshot()
	if snap.Scanned != 1 || snap.Indexed != 0 || snap.Skipped != 1 || snap.Errors != 0 {
		t.Errorf("counters scanned=%d indexed=%d skipped=%d errors=%d, want 1/0/1/0",
			snap.Scanned, snap.Indexed, snap.Skipped, snap.Errors)
	}
}

// TestSizeCapRead_RetiresWhatTheSmallerFileIndexed is the honesty half. The file
// was indexed while it fitted, then grew past the cap. The row now says
// "skipped for size_cap", so its old chunks must not still be answering `search`:
// a document cannot be both not-indexed in the coverage report and searchable.
//
// On `main` the second scan reads the grown file and reindexes it, so the
// representation survives and the row never says size_cap at all.
func TestSizeCapRead_RetiresWhatTheSmallerFileIndexed(t *testing.T) {
	root := t.TempDir()
	stateDir := t.TempDir()
	writeFile(t, filepath.Join(root, "notes.txt"), "small so far")
	st := newRealStore(t)
	cfg := capConfig682(root, stateDir)

	// Scan 1: an ordinary in-cap file, indexed with a raw_text representation.
	first := mustNewIngestService(t, cfg, st)
	first.SetCorpusFS(corpusfs.NewLocalFS(root))
	if err := first.Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if got := liveRepTypes682(t, st, "notes.txt"); len(got) == 0 {
		t.Fatalf("first scan indexed no representation for the in-cap file; the fixture is wrong")
	}

	// Scan 2: the file grows past the cap between the stat and the read.
	second := mustNewIngestService(t, cfg, st)
	second.SetCorpusFS(&growOnOpenFS682{
		inner:  corpusfs.NewLocalFS(root),
		root:   root,
		target: "notes.txt",
		extra:  overCapBytes682,
	})
	if err := second.Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	assertSizeCapSkip682(t, st, "notes.txt")
}

// TestSizeCapDiscovery_RetiresWhatTheSmallerFileIndexed is the same honesty rule
// for the file that grew between two scans, so the discovery stat catches it. Both
// routes to a size_cap row must mean the same thing, or the reason tells an
// operator nothing about what is still searchable.
//
// On `main` the second scan writes the size_cap row and leaves the first scan's
// representation live.
func TestSizeCapDiscovery_RetiresWhatTheSmallerFileIndexed(t *testing.T) {
	root := t.TempDir()
	stateDir := t.TempDir()
	path := filepath.Join(root, "notes.txt")
	writeFile(t, path, "small so far")
	st := newRealStore(t)
	cfg := capConfig682(root, stateDir)

	first := mustNewIngestService(t, cfg, st)
	if err := first.Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if got := liveRepTypes682(t, st, "notes.txt"); len(got) == 0 {
		t.Fatalf("first scan indexed no representation for the in-cap file; the fixture is wrong")
	}

	// Grow the file on disk, between scans, so discovery itself refuses it.
	if err := os.WriteFile(path, []byte(strings.Repeat("b", int(overCapBytes682))), 0o600); err != nil {
		t.Fatalf("grow file: %v", err)
	}

	second := mustNewIngestService(t, cfg, st)
	if err := second.Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	assertSizeCapSkip682(t, st, "notes.txt")
}

// TestSizeCapRead_OverCapArchiveIsNotExpanded pins that an over-cap ARCHIVE is
// refused as a whole. The archive branch runs for a `skipped` container by design,
// so without an explicit stop the pipeline would localize a container whose own
// bytes had just been declared over the cap and ingest its members. A file dropped
// by the discovery stat (#497) never reaches that branch, and this one must not
// either.
//
// On `main` the container reads in full and is recorded with skip_reason="archive".
func TestSizeCapRead_OverCapArchiveIsNotExpanded(t *testing.T) {
	root := t.TempDir()
	fs := &underReportingFS682{relPath: "bundle.zip", reportedLen: 512, bodyLen: overCapBytes682}
	st := newRealStore(t)

	svc := mustNewIngestService(t, capConfig682(root, t.TempDir()), st)
	svc.SetCorpusFS(fs)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if served := fs.bytesServed.Load(); served > readCapBytes682+1 {
		t.Errorf("the scan pulled %d bytes from the container; the bound is %d (cap+1)", served, readCapBytes682+1)
	}
	doc, err := st.GetDocumentByPath(context.Background(), "bundle.zip")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.SkipReason != model.SkipReasonSizeCap {
		t.Errorf("container skip_reason = %q, want %q: an over-cap container was refused by the cap, not because it is an archive", doc.SkipReason, model.SkipReasonSizeCap)
	}
	docs, _, err := st.ListFiles(context.Background(), "", "", 100, 0)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	for _, d := range docs {
		if d.RelPath != "bundle.zip" {
			t.Errorf("member %q was ingested out of an over-cap container", d.RelPath)
		}
	}
}

// TestSizeCapRead_RaisesOneFileSkipEvent pins SPEC §3.2: the number of file_skip
// events a run emits equals its terminal `indexing.skipped`. An over-cap read
// counts one skip, so it must raise exactly one event, carrying the §15.2 reason.
//
// The container case is included because it takes a different route: an archive's
// event is normally deferred until extraction finishes, and an over-cap container
// never extracts.
func TestSizeCapRead_RaisesOneFileSkipEvent(t *testing.T) {
	for _, relPath := range []string{"liar.txt", "bundle.zip"} {
		t.Run(relPath, func(t *testing.T) {
			root := t.TempDir()
			fs := &underReportingFS682{relPath: relPath, reportedLen: 512, bodyLen: overCapBytes682}
			st := newRealStore(t)

			svc := mustNewIngestService(t, capConfig682(root, t.TempDir()), st)
			svc.SetCorpusFS(fs)
			var reasons []string
			svc.SetOnDocumentSkip(func(_, _, reason string) {
				reasons = append(reasons, reason)
			})
			state := appstate.NewIndexingState(appstate.ModeIncremental)
			svc.SetIndexingState(state)
			if err := svc.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}

			if len(reasons) != 1 || reasons[0] != model.SkipReasonSizeCap {
				t.Errorf("file_skip reasons = %v, want exactly one %q", reasons, model.SkipReasonSizeCap)
			}
			if got := state.Snapshot().Skipped; got != int64(len(reasons)) {
				t.Errorf("skipped counter = %d but %d file_skip event(s) were raised; SPEC §3.2 requires equality", got, len(reasons))
			}
		})
	}
}

// TestSizeCapRead_ManifestCarriesFileTooLarge pins the §14.4 code on the batch
// manifest record (§8.6.11). A file the discovery stat drops already records
// FILE_TOO_LARGE, so a file the READ refuses must record it too: a codeless skip
// leaves the manifest unable to tell this asset from an unchanged cache hit.
//
// On `main` the asset is recorded as "completed".
func TestSizeCapRead_ManifestCarriesFileTooLarge(t *testing.T) {
	root := t.TempDir()
	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "run.jsonl")
	fs := &underReportingFS682{relPath: "liar.txt", reportedLen: 512, bodyLen: overCapBytes682}
	st := newRealStore(t)

	cfg := capConfig682(root, stateDir)
	cfg.MediaBatchManifest = manifestPath
	svc := mustNewIngestService(t, cfg, st)
	svc.SetCorpusFS(fs)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	recs := readManifest(t, manifestPath)
	if len(recs) != 1 {
		t.Fatalf("want 1 manifest record, got %d: %+v", len(recs), recs)
	}
	if got := recs[0]["status"]; got != "skipped" {
		t.Errorf("manifest status = %v, want skipped", got)
	}
	if got := recs[0]["error_code"]; got != "FILE_TOO_LARGE" {
		t.Errorf("manifest error_code = %v, want FILE_TOO_LARGE", got)
	}
}

// TestSizeCapRead_FileAtTheCapIsStillIndexed is the false-positive guard on the
// limit+1 read. A file of exactly `ingest.max_file_mb` bytes is inside the cap and
// must still be indexed: the bound must refuse only what passes it, and an
// off-by-one here would silently drop every file at the boundary.
func TestSizeCapRead_FileAtTheCapIsStillIndexed(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "exact.txt"), strings.Repeat("a", int(readCapBytes682)))
	st := newRealStore(t)

	svc := mustNewIngestService(t, capConfig682(root, t.TempDir()), st)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	doc, err := st.GetDocumentByPath(context.Background(), "exact.txt")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.Status != "ok" {
		t.Errorf("a file of exactly the cap has status = %q (skip_reason %q), want ok", doc.Status, doc.SkipReason)
	}
	if doc.SizeBytes != readCapBytes682 {
		t.Errorf("size_bytes = %d, want %d", doc.SizeBytes, readCapBytes682)
	}
	if got := liveRepTypes682(t, st, "exact.txt"); len(got) == 0 {
		t.Errorf("a file of exactly the cap has no live representation; it must stay searchable")
	}
}

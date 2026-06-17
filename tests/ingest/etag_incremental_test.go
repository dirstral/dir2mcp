package tests

import (
	"bytes"
	"context"
	"io"
	"os"
	"sort"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// fakeRemoteFS is a stub object-store CorpusFS that serves a fixed set of
// in-memory objects (each carrying an ETag, mimicking S3 discovery) and counts
// Open calls so a test can assert whether an object's body was actually read
// during a scan. Localize is unused by the change-detection path under test.
type fakeRemoteFS struct {
	files   []corpusfs.DiscoveredFile
	bodies  map[string][]byte
	mu      sync.Mutex
	opens   map[string]int
	openErr map[string]error
}

func newFakeRemoteFS() *fakeRemoteFS {
	return &fakeRemoteFS{
		bodies:  map[string][]byte{},
		opens:   map[string]int{},
		openErr: map[string]error{},
	}
}

func (f *fakeRemoteFS) add(relPath, etag, body string) {
	f.files = append(f.files, corpusfs.DiscoveredFile{
		RelPath:   relPath,
		SizeBytes: int64(len(body)),
		ETag:      etag,
	})
	f.bodies[relPath] = []byte(body)
}

// addWithMTime is add() with an explicit MTimeUnix so a test can pin the value
// the sidecar fingerprint folds in ("rel_path@mtime").
func (f *fakeRemoteFS) addWithMTime(relPath, etag, body string, mtime int64) {
	f.files = append(f.files, corpusfs.DiscoveredFile{
		RelPath:   relPath,
		SizeBytes: int64(len(body)),
		MTimeUnix: mtime,
		ETag:      etag,
	})
	f.bodies[relPath] = []byte(body)
}

func (f *fakeRemoteFS) Walk(_ context.Context, _ string, _ corpusfs.Options) ([]corpusfs.DiscoveredFile, error) {
	out := append([]corpusfs.DiscoveredFile(nil), f.files...)
	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out, nil
}

func (f *fakeRemoteFS) Open(_ context.Context, relPath string) (io.ReadSeekCloser, error) {
	f.mu.Lock()
	f.opens[relPath]++
	err := f.openErr[relPath]
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return nopReadSeekCloser{bytes.NewReader(f.bodies[relPath])}, nil
}

func (f *fakeRemoteFS) Localize(_ context.Context, relPath string) (string, func(), error) {
	return "", func() {}, os.ErrNotExist
}

func (f *fakeRemoteFS) openCount(relPath string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens[relPath]
}

type nopReadSeekCloser struct{ *bytes.Reader }

func (nopReadSeekCloser) Close() error { return nil }

// remoteScanStore is a Store implementation backed by an in-memory map, with the
// optional documentDeleteMarker capability so a missing source object can be
// tombstoned. It is the persistence half of the S3 change-detection scenarios.
type remoteScanStore struct {
	mu   sync.Mutex
	docs map[string]model.Document

	upsertCalls map[string]int
	deleted     map[string]bool
}

func newRemoteScanStore() *remoteScanStore {
	return &remoteScanStore{
		docs:        map[string]model.Document{},
		upsertCalls: map[string]int{},
		deleted:     map[string]bool{},
	}
}

func (s *remoteScanStore) seed(doc model.Document) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs[doc.RelPath] = doc
}

func (s *remoteScanStore) Init(context.Context) error { return nil }
func (s *remoteScanStore) Close() error               { return nil }

func (s *remoteScanStore) UpsertDocument(_ context.Context, doc model.Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsertCalls[doc.RelPath]++
	// Preserve the assigned DocID across upserts so refreshDocID is stable.
	if existing, ok := s.docs[doc.RelPath]; ok && doc.DocID == 0 {
		doc.DocID = existing.DocID
	}
	if doc.DocID == 0 {
		doc.DocID = int64(len(s.docs) + 1)
	}
	s.docs[doc.RelPath] = doc
	return nil
}

func (s *remoteScanStore) GetDocumentByPath(_ context.Context, relPath string) (model.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, ok := s.docs[relPath]
	if !ok {
		return model.Document{}, os.ErrNotExist
	}
	return doc, nil
}

func (s *remoteScanStore) ListFiles(_ context.Context, prefix, _ string, limit, offset int) ([]model.Document, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := make([]model.Document, 0, len(s.docs))
	for _, d := range s.docs {
		if d.Deleted {
			continue
		}
		all = append(all, d)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].RelPath < all[j].RelPath })
	total := int64(len(all))
	if offset >= len(all) {
		return nil, total, nil
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end], total, nil
}

func (s *remoteScanStore) MarkDocumentDeleted(_ context.Context, relPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc := s.docs[relPath]
	doc.RelPath = relPath
	doc.Deleted = true
	s.docs[relPath] = doc
	s.deleted[relPath] = true
	return nil
}

func (s *remoteScanStore) get(relPath string) (model.Document, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.docs[relPath]
	return d, ok
}

// TestRunScan_S3ETagChangeDetection drives a full incremental scan against a
// stub object-store backend and asserts the SPEC §7.8.3 change-detection
// behavior for S3 corpora (#245): an unchanged ETag skips the GET + re-hash and
// leaves the document untouched; a changed ETag re-reads and reindexes; a new
// object is ingested; and a removed object is tombstoned.
func TestRunScan_S3ETagChangeDetection(t *testing.T) {
	fs := newFakeRemoteFS()
	fs.add("unchanged.txt", "etag-unchanged", "stable body")
	fs.add("changed.txt", "etag-new", "new body")
	fs.add("new.txt", "etag-fresh", "brand new")
	// "removed.txt" is intentionally NOT added to the source: it exists only in
	// the store, so the scan must tombstone it.

	st := newRemoteScanStore()
	st.seed(model.Document{
		DocID:       1,
		RelPath:     "unchanged.txt",
		DocType:     "text",
		SizeBytes:   int64(len("stable body")),
		ContentHash: ingest.ComputeContentHash([]byte("stable body")),
		ETag:        "etag-unchanged",
		Status:      "ok",
	})
	st.seed(model.Document{
		DocID:       2,
		RelPath:     "changed.txt",
		DocType:     "text",
		SizeBytes:   int64(len("old body")),
		ContentHash: ingest.ComputeContentHash([]byte("old body")),
		ETag:        "etag-old",
		Status:      "ok",
	})
	st.seed(model.Document{
		DocID:       3,
		RelPath:     "removed.txt",
		DocType:     "text",
		SizeBytes:   5,
		ContentHash: ingest.ComputeContentHash([]byte("gone!")),
		ETag:        "etag-removed",
		Status:      "ok",
	})

	svc := mustNewIngestService(t, config.Config{RootDir: "/corpus"}, st)
	svc.SetCorpusFS(fs)

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// Unchanged ETag: object body must NOT have been read, and the stored row
	// (content_hash, ETag) must be untouched (no upsert).
	if got := fs.openCount("unchanged.txt"); got != 0 {
		t.Errorf("unchanged object was re-read %d time(s); expected 0 (ETag skip)", got)
	}
	if got := st.upsertCalls["unchanged.txt"]; got != 0 {
		t.Errorf("unchanged object was upserted %d time(s); expected 0", got)
	}
	if doc, _ := st.get("unchanged.txt"); doc.ContentHash != ingest.ComputeContentHash([]byte("stable body")) {
		t.Errorf("unchanged content_hash mutated: %q", doc.ContentHash)
	}

	// Changed ETag: object must be re-read and reindexed (new hash + new ETag
	// persisted).
	if got := fs.openCount("changed.txt"); got == 0 {
		t.Errorf("changed object was not re-read; expected a GET")
	}
	if doc, ok := st.get("changed.txt"); !ok {
		t.Errorf("changed object missing from store")
	} else {
		if doc.ContentHash != ingest.ComputeContentHash([]byte("new body")) {
			t.Errorf("changed content_hash not recomputed: %q", doc.ContentHash)
		}
		if doc.ETag != "etag-new" {
			t.Errorf("changed ETag not updated: %q", doc.ETag)
		}
	}

	// New object: read and ingested with its ETag recorded.
	if got := fs.openCount("new.txt"); got == 0 {
		t.Errorf("new object was not read")
	}
	if doc, ok := st.get("new.txt"); !ok {
		t.Errorf("new object not ingested")
	} else if doc.ETag != "etag-fresh" {
		t.Errorf("new object ETag not recorded: %q", doc.ETag)
	}

	// Removed object: tombstoned.
	if !st.deleted["removed.txt"] {
		t.Errorf("removed object was not tombstoned")
	}
}

// TestRunScan_S3ETagSizeMismatchForcesReRead confirms the size guard in
// etagUnchanged: when the stored ETag matches the discovered object but the size
// differs (e.g. a truncated/legacy ETag collision), the object MUST be re-read
// and re-hashed rather than silently skipped (SPEC §7.8.3 — content_hash stays
// canonical). This is the highest-risk silent-staleness branch.
func TestRunScan_S3ETagSizeMismatchForcesReRead(t *testing.T) {
	fs := newFakeRemoteFS()
	fs.add("doc.txt", "etag-collide", "fresh longer body")

	st := newRemoteScanStore()
	st.seed(model.Document{
		DocID:       1,
		RelPath:     "doc.txt",
		DocType:     "text",
		SizeBytes:   int64(len("old body")), // deliberately different size
		ContentHash: ingest.ComputeContentHash([]byte("old body")),
		ETag:        "etag-collide", // same token as discovered object
		Status:      "ok",
	})

	svc := mustNewIngestService(t, config.Config{RootDir: "/corpus"}, st)
	svc.SetCorpusFS(fs)

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if got := fs.openCount("doc.txt"); got == 0 {
		t.Errorf("ETag matched but size differed; object must be re-read (no skip)")
	}
	if doc, _ := st.get("doc.txt"); doc.ContentHash != ingest.ComputeContentHash([]byte("fresh longer body")) {
		t.Errorf("content_hash not recomputed on size-mismatch re-read: %q", doc.ContentHash)
	}
}

// TestRunScan_S3ETagErrorStatusForcesReprocess confirms that a document
// previously recorded as status="error" is re-processed even when its ETag and
// size still match the discovered object, so a transient ingest failure recovers
// on the next incremental scan without a full reindex (SPEC §7.8.3).
func TestRunScan_S3ETagErrorStatusForcesReprocess(t *testing.T) {
	fs := newFakeRemoteFS()
	fs.add("doc.txt", "etag-stable", "body")

	st := newRemoteScanStore()
	st.seed(model.Document{
		DocID:       1,
		RelPath:     "doc.txt",
		DocType:     "text",
		SizeBytes:   int64(len("body")),
		ContentHash: ingest.ComputeContentHash([]byte("body")),
		ETag:        "etag-stable",
		Status:      "error", // prior ingest failed
	})

	svc := mustNewIngestService(t, config.Config{RootDir: "/corpus"}, st)
	svc.SetCorpusFS(fs)

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if got := fs.openCount("doc.txt"); got == 0 {
		t.Errorf("error-status document with matching ETag was skipped; it must re-process")
	}
	if doc, _ := st.get("doc.txt"); doc.Status != "ok" {
		t.Errorf("error-status document not recovered after re-process: status=%q", doc.Status)
	}
}

// TestRunScan_S3ETagForceReindexBypassesSkip confirms that a forced reindex
// re-reads even an object whose ETag is unchanged (the skip is incremental-only,
// SPEC §7.8.3).
func TestRunScan_S3ETagForceReindexBypassesSkip(t *testing.T) {
	fs := newFakeRemoteFS()
	fs.add("doc.txt", "etag-stable", "body")

	st := newRemoteScanStore()
	st.seed(model.Document{
		DocID:       1,
		RelPath:     "doc.txt",
		DocType:     "text",
		SizeBytes:   int64(len("body")),
		ContentHash: ingest.ComputeContentHash([]byte("body")),
		ETag:        "etag-stable",
		Status:      "ok",
	})

	svc := mustNewIngestService(t, config.Config{RootDir: "/corpus"}, st)
	svc.SetCorpusFS(fs)
	// Reindex forces only when an IndexingState in full mode is attached (the
	// CLI wires this); attach one so forceReindex is true.
	svc.SetIndexingState(appstate.NewIndexingState(appstate.ModeFull))

	if err := svc.Reindex(context.Background()); err != nil {
		t.Fatalf("Reindex failed: %v", err)
	}
	if got := fs.openCount("doc.txt"); got == 0 {
		t.Errorf("force reindex did not re-read object despite matching ETag")
	}
}

// TestRunScan_LocalEmptyETagUsesContentHash confirms the local/NFS path is
// unaffected: with an empty ETag the (size,mtime)→content_hash gate decides, so
// an unchanged-hash document still reads its body (no ETag skip) but is not
// re-represented, while a changed body reindexes.
func TestRunScan_LocalEmptyETagUsesContentHash(t *testing.T) {
	fs := newFakeRemoteFS()
	// Empty ETag mimics LocalFS discovery.
	fs.add("local.txt", "", "same body")

	st := newRemoteScanStore()
	st.seed(model.Document{
		DocID:       1,
		RelPath:     "local.txt",
		DocType:     "text",
		SizeBytes:   int64(len("same body")),
		ContentHash: ingest.ComputeContentHash([]byte("same body")),
		ETag:        "",
		Status:      "ok",
	})

	svc := mustNewIngestService(t, config.Config{RootDir: "/corpus"}, st)
	svc.SetCorpusFS(fs)

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	// Local path has no cheap ETag token, so the body IS read to confirm the
	// content_hash (the historical behavior must be intact).
	if got := fs.openCount("local.txt"); got == 0 {
		t.Errorf("local (empty ETag) object was not read; ETag skip must not apply locally")
	}
	if doc, _ := st.get("local.txt"); doc.ContentHash != ingest.ComputeContentHash([]byte("same body")) {
		t.Errorf("local content_hash changed unexpectedly: %q", doc.ContentHash)
	}
}

// TestRunScan_S3ETagSidecarMediaNoSidecarSkips confirms that sidecar-capable
// media (audio/video) WITHOUT any adjacent sidecar now keeps the cheap ETag fast
// path: the persisted sidecar_fingerprint is empty and the cheaply-recomputed
// current fingerprint is also empty, so an unchanged ETag skips the GET + re-hash
// just like a non-media object (#298 replaces the conservative #295 carve-out).
func TestRunScan_S3ETagSidecarMediaNoSidecarSkips(t *testing.T) {
	fs := newFakeRemoteFS()
	fs.add("clip.mp4", "etag-stable", "media bytes") // sidecar-capable (video), no sidecar
	fs.add("notes.txt", "etag-stable", "text body")  // non-media control

	st := newRemoteScanStore()
	st.seed(model.Document{
		DocID:       1,
		RelPath:     "clip.mp4",
		DocType:     "video",
		SizeBytes:   int64(len("media bytes")),
		ContentHash: ingest.ComputeContentHash([]byte("media bytes")),
		ETag:        "etag-stable",
		// SidecarFingerprint intentionally empty: no sidecar at last scan.
		Status: "ok",
	})
	st.seed(model.Document{
		DocID:       2,
		RelPath:     "notes.txt",
		DocType:     "text",
		SizeBytes:   int64(len("text body")),
		ContentHash: ingest.ComputeContentHash([]byte("text body")),
		ETag:        "etag-stable",
		Status:      "ok",
	})

	svc := mustNewIngestService(t, config.Config{RootDir: "/corpus"}, st)
	svc.SetCorpusFS(fs)

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	// Sidecar-capable media with no sidecar and a matching empty fingerprint must
	// ETag-skip (the whole point of #298: stop re-reading every media object).
	if got := fs.openCount("clip.mp4"); got != 0 {
		t.Errorf("sidecar-capable media without sidecar was re-read %d time(s); expected ETag skip (0)", got)
	}
	if got := st.upsertCalls["clip.mp4"]; got != 0 {
		t.Errorf("sidecar-capable media without sidecar was upserted %d time(s); expected 0 (untouched on skip)", got)
	}
	// Non-media object keeps the cheap ETag skip (regression guard).
	if got := fs.openCount("notes.txt"); got != 0 {
		t.Errorf("non-media object with matching ETag was re-read; it should ETag-skip (got %d opens)", got)
	}
}

// TestRunScan_S3ETagSidecarAddedForcesReRead confirms that adding a subtitle
// sidecar next to a media object with an unchanged ETag breaks the ETag skip:
// the cheaply-recomputed current fingerprint (now non-empty) no longer matches
// the persisted (empty) fingerprint, so the media is re-read and re-hashed so the
// sidecar transcript is ingested (#298).
func TestRunScan_S3ETagSidecarAddedForcesReRead(t *testing.T) {
	fs := newFakeRemoteFS()
	fs.add("clip.mp4", "etag-stable", "media bytes")
	// A subtitle sidecar appeared since the last scan (media ETag unchanged).
	fs.add("clip.en.vtt", "etag-sub", "WEBVTT\n\n00:00.000 --> 00:01.000\nhi\n")

	st := newRemoteScanStore()
	st.seed(model.Document{
		DocID:       1,
		RelPath:     "clip.mp4",
		DocType:     "video",
		SizeBytes:   int64(len("media bytes")),
		ContentHash: ingest.ComputeContentHash([]byte("media bytes")),
		ETag:        "etag-stable",
		// SidecarFingerprint empty: no sidecar existed at last scan.
		Status: "ok",
	})

	svc := mustNewIngestService(t, config.Config{RootDir: "/corpus"}, st)
	svc.SetCorpusFS(fs)

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if got := fs.openCount("clip.mp4"); got == 0 {
		t.Errorf("media with newly-added sidecar (ETag unchanged) was ETag-skipped; it must be re-read")
	}
	// The recomputed row must now carry a non-empty persisted fingerprint so the
	// next scan can skip when nothing changes again.
	if doc, _ := st.get("clip.mp4"); doc.SidecarFingerprint == "" {
		t.Errorf("expected non-empty persisted sidecar_fingerprint after re-read; got empty")
	}
}

// TestRunScan_S3ETagSidecarUnchangedSkips confirms that media whose sidecar is
// unchanged since the last scan (persisted fingerprint == current fingerprint)
// keeps the cheap ETag skip — no media read despite being sidecar-capable (#298).
func TestRunScan_S3ETagSidecarUnchangedSkips(t *testing.T) {
	fs := newFakeRemoteFS()
	fs.add("clip.mp4", "etag-stable", "media bytes")
	fs.addWithMTime("clip.en.vtt", "etag-sub", "WEBVTT\n", 4242)

	// Seed the row with the SAME fingerprint the scan will recompute for the
	// unchanged sidecar (sorted "rel_path@mtime"). This mirrors what a prior
	// successful ingest persisted.
	st := newRemoteScanStore()
	st.seed(model.Document{
		DocID:              1,
		RelPath:            "clip.mp4",
		DocType:            "video",
		SizeBytes:          int64(len("media bytes")),
		ContentHash:        ingest.ComputeContentHash([]byte("media bytes")),
		ETag:               "etag-stable",
		SidecarFingerprint: "clip.en.vtt@4242",
		Status:             "ok",
	})

	svc := mustNewIngestService(t, config.Config{RootDir: "/corpus"}, st)
	svc.SetCorpusFS(fs)

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if got := fs.openCount("clip.mp4"); got != 0 {
		t.Errorf("media with unchanged sidecar was re-read %d time(s); expected ETag skip (0)", got)
	}
	if got := st.upsertCalls["clip.mp4"]; got != 0 {
		t.Errorf("media with unchanged sidecar was upserted %d time(s); expected 0 (untouched on skip)", got)
	}
}

// TestRunScan_S3ETagSidecarChangedForcesReRead confirms that a sidecar whose
// mtime changed since the last scan (persisted fingerprint differs from the
// recomputed one) breaks the ETag skip even though the media object's ETag is
// unchanged (#298).
func TestRunScan_S3ETagSidecarChangedForcesReRead(t *testing.T) {
	fs := newFakeRemoteFS()
	fs.add("clip.mp4", "etag-stable", "media bytes")
	fs.addWithMTime("clip.en.vtt", "etag-sub", "WEBVTT\n\n00:00.000 --> 00:01.000\nhi\n", 9999)

	st := newRemoteScanStore()
	st.seed(model.Document{
		DocID:              1,
		RelPath:            "clip.mp4",
		DocType:            "video",
		SizeBytes:          int64(len("media bytes")),
		ContentHash:        ingest.ComputeContentHash([]byte("media bytes")),
		ETag:               "etag-stable",
		SidecarFingerprint: "clip.en.vtt@1111", // stale mtime
		Status:             "ok",
	})

	svc := mustNewIngestService(t, config.Config{RootDir: "/corpus"}, st)
	svc.SetCorpusFS(fs)

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if got := fs.openCount("clip.mp4"); got == 0 {
		t.Errorf("media with changed sidecar (ETag unchanged) was ETag-skipped; it must be re-read")
	}
	if doc, _ := st.get("clip.mp4"); doc.SidecarFingerprint != "clip.en.vtt@9999" {
		t.Errorf("persisted sidecar_fingerprint not refreshed: got %q", doc.SidecarFingerprint)
	}
}

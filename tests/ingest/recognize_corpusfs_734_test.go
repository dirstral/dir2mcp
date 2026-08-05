package tests

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// localizingRemoteFS is a stub object-store CorpusFS shaped like S3FS for the
// properties that matter to issue #734: discovery reports no local path
// (DiscoveredFile.AbsPath == ""), Walk ignores the RootDir argument, and
// Localize materializes a temp copy under its own cache dir and returns a
// cleanup that removes it. It records Localize/cleanup calls so a test can
// assert the recognizer is fed the localized copy and that the copy is released
// on both the success and the failure path.
type localizingRemoteFS struct {
	bodies   map[string][]byte
	cacheDir string

	mu            sync.Mutex
	localizeCalls int
	cleanupCalls  int
	localizeErr   error
	lastLocalPath string
}

func newLocalizingRemoteFS(t *testing.T) *localizingRemoteFS {
	t.Helper()
	return &localizingRemoteFS{bodies: map[string][]byte{}, cacheDir: t.TempDir()}
}

func (f *localizingRemoteFS) add(relPath, body string) {
	f.bodies[relPath] = []byte(body)
}

func (f *localizingRemoteFS) Walk(_ context.Context, _ string, _ corpusfs.Options) ([]corpusfs.DiscoveredFile, error) {
	out := make([]corpusfs.DiscoveredFile, 0, len(f.bodies))
	for rel, body := range f.bodies {
		// AbsPath deliberately empty: an object store has no local path.
		out = append(out, corpusfs.DiscoveredFile{RelPath: rel, SizeBytes: int64(len(body))})
	}
	return out, nil
}

func (f *localizingRemoteFS) Open(_ context.Context, relPath string) (io.ReadSeekCloser, error) {
	body, ok := f.bodies[relPath]
	if !ok {
		return nil, os.ErrNotExist
	}
	return nopReadSeekCloser{bytes.NewReader(body)}, nil
}

func (f *localizingRemoteFS) Localize(_ context.Context, relPath string) (string, func(), error) {
	f.mu.Lock()
	f.localizeCalls++
	localizeErr := f.localizeErr
	f.mu.Unlock()
	if localizeErr != nil {
		return "", nil, localizeErr
	}
	body, ok := f.bodies[relPath]
	if !ok {
		return "", nil, os.ErrNotExist
	}
	tmp, err := os.CreateTemp(f.cacheDir, "obj-*"+filepath.Ext(relPath))
	if err != nil {
		return "", nil, err
	}
	path := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return "", nil, err
	}
	if err := tmp.Close(); err != nil {
		return "", nil, err
	}
	f.mu.Lock()
	f.lastLocalPath = path
	f.mu.Unlock()
	return path, func() {
		f.mu.Lock()
		f.cleanupCalls++
		f.mu.Unlock()
		_ = os.Remove(path)
	}, nil
}

func (f *localizingRemoteFS) counts() (localize, cleanup int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.localizeCalls, f.cleanupCalls
}

func (f *localizingRemoteFS) localPath() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastLocalPath
}

// readingRecognizer is a model.Recognizer double that actually opens the path it
// is handed, which is the whole point of #734: a path the backend cannot read is
// worthless no matter what it is named.
type readingRecognizer struct {
	result  model.RecognizeResult
	err     error
	calls   int
	path    string
	gotBody []byte
	readErr error
}

func (r *readingRecognizer) Recognize(_ context.Context, absPath string) (model.RecognizeResult, error) {
	r.calls++
	r.path = absPath
	r.gotBody, r.readErr = os.ReadFile(absPath) //nolint:gosec // test double reads the path under test
	if r.err != nil {
		return model.RecognizeResult{}, r.err
	}
	return r.result, nil
}

// TestRecognize_RemoteCorpusFS_FeedsLocalizedPath pins issue #734: with a
// non-local CorpusFS (source.kind=s3), recognition must obtain the media through
// CorpusFS.Localize instead of reconstructing RootDir+rel_path, which names a
// file that does not exist for an object store. The recognizer must be able to
// READ the path it receives, and the localized copy must be released afterwards.
func TestRecognize_RemoteCorpusFS_FeedsLocalizedPath(t *testing.T) {
	t.Parallel()
	// RootDir exists but is empty: for an object store it is not where the bytes
	// live, so any RootDir-joined path is a dangling reference.
	root := t.TempDir()
	fsys := newLocalizingRemoteFS(t)
	fsys.add("games/game7.mp4", "remote-video-bytes")

	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir()}, st)
	svc.SetCorpusFS(fsys)
	rec := &readingRecognizer{result: recognizeTestResult()}
	svc.SetRecognizer(rec)

	doc := model.Document{DocID: 1, RelPath: "games/game7.mp4", DocType: "video"}
	if err := svc.GenerateRecognitionRepresentation(context.Background(), doc); err != nil {
		t.Fatalf("GenerateRecognitionRepresentation: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("expected exactly one backend call, got %d", rec.calls)
	}
	if rec.readErr != nil {
		t.Fatalf("recognizer could not read the path it was handed (%q): %v", rec.path, rec.readErr)
	}
	if got := string(rec.gotBody); got != "remote-video-bytes" {
		t.Fatalf("recognizer read %q, want the object bytes %q", got, "remote-video-bytes")
	}
	if rec.path == filepath.Join(root, "games", "game7.mp4") {
		t.Fatalf("recognizer received a RootDir-joined path %q; it must receive the CorpusFS-localized copy", rec.path)
	}
	if want := fsys.localPath(); rec.path != want {
		t.Fatalf("recognizer path = %q, want the localized copy %q", rec.path, want)
	}
	localize, cleanup := fsys.counts()
	if localize != 1 || cleanup != 1 {
		t.Fatalf("Localize called %d time(s), cleanup %d time(s); want exactly 1 each", localize, cleanup)
	}
	if _, err := os.Stat(fsys.localPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("localized copy %q survived the call (stat err %v); cleanup must remove it", fsys.localPath(), err)
	}
	if len(st.reps) != 1 || st.reps[0].RepType != ingest.RepTypeRecognition {
		t.Fatalf("expected one recognition representation, got %+v", st.reps)
	}
}

// TestRecognize_RemoteCorpusFS_CleanupOnBackendError asserts the localized copy
// is released on the error path too: a recognize-backend failure must not leak a
// downloaded temp file per document, and the failure still classifies as a
// recognition provider failure (RECOGNIZE_FAILED).
func TestRecognize_RemoteCorpusFS_CleanupOnBackendError(t *testing.T) {
	t.Parallel()
	fsys := newLocalizingRemoteFS(t)
	fsys.add("games/game7.mp4", "remote-video-bytes")

	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{RootDir: t.TempDir(), StateDir: t.TempDir()}, st)
	svc.SetCorpusFS(fsys)
	svc.SetRecognizer(&readingRecognizer{err: errors.New("backend down")})

	doc := model.Document{DocID: 1, RelPath: "games/game7.mp4", DocType: "video"}
	err := svc.GenerateRecognitionRepresentation(context.Background(), doc)
	if err == nil {
		t.Fatal("expected the backend error to propagate")
	}
	if !errors.Is(err, ingest.ErrRecognitionProviderFailure) {
		t.Fatalf("backend failure must stay a recognition provider failure, got %v", err)
	}
	localize, cleanup := fsys.counts()
	if localize != 1 || cleanup != 1 {
		t.Fatalf("Localize called %d time(s), cleanup %d time(s); want exactly 1 each on the error path", localize, cleanup)
	}
	if _, statErr := os.Stat(fsys.localPath()); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("localized copy leaked on the error path (stat err %v)", statErr)
	}
}

// TestRecognize_LocalizeFailureIsRecognizeFailure asserts a media the CorpusFS
// cannot materialize (deleted object, download failure) is reported as a
// recognition failure rather than silently producing nothing, and that the
// backend is never called with an unusable path.
func TestRecognize_LocalizeFailureIsRecognizeFailure(t *testing.T) {
	t.Parallel()
	fsys := newLocalizingRemoteFS(t)
	fsys.add("games/game7.mp4", "remote-video-bytes")
	fsys.localizeErr = errors.New("download failed")

	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{RootDir: t.TempDir(), StateDir: t.TempDir()}, st)
	svc.SetCorpusFS(fsys)
	rec := &readingRecognizer{result: recognizeTestResult()}
	svc.SetRecognizer(rec)

	doc := model.Document{DocID: 1, RelPath: "games/game7.mp4", DocType: "video"}
	err := svc.GenerateRecognitionRepresentation(context.Background(), doc)
	if err == nil {
		t.Fatal("expected a localize failure to be reported")
	}
	if !errors.Is(err, ingest.ErrRecognitionProviderFailure) {
		t.Fatalf("localize failure must classify as RECOGNIZE_FAILED, got %v", err)
	}
	if rec.calls != 0 {
		t.Fatalf("backend must not be called when the media cannot be localized, got %d call(s)", rec.calls)
	}
	if len(st.reps) != 0 {
		t.Fatalf("expected no representation, got %+v", st.reps)
	}
}

// relativePathFS is a CorpusFS whose Localize returns a CWD-relative path,
// which an object-store backend can legitimately produce: the S3 temp copy
// lands under StateDir, and StateDir may be configured relative (several CLI
// paths default it to "./.dir2mcp"). The serve wire contract is an absolute
// media path (design 0004 §5), so recognition must absolutize before the call.
type relativePathFS struct {
	relPath string
}

func (f *relativePathFS) Walk(context.Context, string, corpusfs.Options) ([]corpusfs.DiscoveredFile, error) {
	return nil, nil
}

func (f *relativePathFS) Open(context.Context, string) (io.ReadSeekCloser, error) {
	return nil, os.ErrNotExist
}

func (f *relativePathFS) Localize(context.Context, string) (string, func(), error) {
	return f.relPath, func() {}, nil
}

func TestRecognize_LocalizedPathIsAbsolute(t *testing.T) {
	t.Parallel()
	media := filepath.Join(t.TempDir(), "game7.mp4")
	writeFile(t, media, "fake-video")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	rel, err := filepath.Rel(cwd, media)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	if filepath.IsAbs(rel) {
		t.Skipf("cannot express %q relative to the test CWD", media)
	}

	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{RootDir: t.TempDir(), StateDir: t.TempDir()}, st)
	svc.SetCorpusFS(&relativePathFS{relPath: rel})
	rec := &readingRecognizer{result: recognizeTestResult()}
	svc.SetRecognizer(rec)

	doc := model.Document{DocID: 1, RelPath: "games/game7.mp4", DocType: "video"}
	if err := svc.GenerateRecognitionRepresentation(context.Background(), doc); err != nil {
		t.Fatalf("GenerateRecognitionRepresentation: %v", err)
	}
	if !filepath.IsAbs(rec.path) {
		t.Fatalf("backend received a relative path %q; the serve contract is an absolute media path (design 0004 §5)", rec.path)
	}
	assertSameFile(t, media, rec.path)
}

// TestRecognize_LocalCorpusStillGetsInRootPath guards the no-regression half of
// #734: a local corpus must keep handing the recognizer the real in-root file
// (LocalFS.Localize returns the resolved path with a no-op cleanup), NOT a copy.
// The file must still be there after the call, and be the same file as the one
// in the corpus root.
func TestRecognize_LocalCorpusStillGetsInRootPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inRoot := filepath.Join(root, "games", "game7.mp4")
	writeFile(t, inRoot, "fake-video")

	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir()}, st)
	rec := &readingRecognizer{result: recognizeTestResult()}
	svc.SetRecognizer(rec)

	doc := model.Document{DocID: 1, RelPath: "games/game7.mp4", DocType: "video"}
	if err := svc.GenerateRecognitionRepresentation(context.Background(), doc); err != nil {
		t.Fatalf("GenerateRecognitionRepresentation: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("expected exactly one backend call, got %d", rec.calls)
	}
	// Still the in-root file itself (not a copy) and still there afterwards: the
	// local path is only symlink-resolved, so identity is checked with SameFile.
	assertSameFile(t, inRoot, rec.path)
}

// assertSameFile fails unless both paths name the same file on disk. It is the
// symlink-tolerant form of comparing a corpus path to the path handed to a
// backend: CorpusFS.Localize resolves symlinks (a temp root is symlinked on
// macOS), so string equality would compare resolution styles, not identity.
func assertSameFile(t *testing.T, wantPath, gotPath string) {
	t.Helper()
	want, err := os.Stat(wantPath)
	if err != nil {
		t.Fatalf("stat %q: %v", wantPath, err)
	}
	got, err := os.Stat(gotPath)
	if err != nil {
		t.Fatalf("stat %q (the path handed to the backend): %v", gotPath, err)
	}
	if !os.SameFile(want, got) {
		t.Fatalf("path %q is not the same file as %q", gotPath, wantPath)
	}
}

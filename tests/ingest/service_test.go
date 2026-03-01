package tests

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"dir2mcp/internal/appstate"
	"dir2mcp/internal/config"
	"dir2mcp/internal/elevenlabs"
	"dir2mcp/internal/ingest"
	"dir2mcp/internal/model"
)

func TestServiceRun_ProcessesFilesAndMarksMissingDeleted(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "keep.txt"), []byte("plain text"))
	mustWriteFile(t, filepath.Join(root, "code", "main.go"), []byte("package main\n"))
	mustWriteFile(t, filepath.Join(root, "archive.zip"), []byte("PK\x03\x04"))
	mustWriteFile(t, filepath.Join(root, "secret.txt"), []byte("Authorization: Bearer abcdefgh.ijklmnop.qrstuvwx\n"))
	mustWriteFile(t, filepath.Join(root, "exclude", "private.txt"), []byte("should be excluded"))

	st := newMemoryStore()
	st.docs["gone.txt"] = model.Document{
		RelPath:   "gone.txt",
		DocType:   "text",
		SizeBytes: 4,
		MTimeUnix: 1,
		Status:    "ok",
	}
	st.docs["exclude/private.txt"] = model.Document{
		RelPath:   "exclude/private.txt",
		DocType:   "text",
		SizeBytes: 1,
		MTimeUnix: 1,
		Status:    "ok",
	}

	cfg := config.Default()
	cfg.RootDir = root
	cfg.PathExcludes = []string{"**/exclude/**"}

	indexState := appstate.NewIndexingState(appstate.ModeIncremental)
	svc := mustNewIngestService(t, cfg, st)
	svc.SetIndexingState(indexState)

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	snapshot := indexState.Snapshot()
	if snapshot.Scanned != 5 {
		t.Fatalf("snapshot.Scanned=%d want=5", snapshot.Scanned)
	}
	if snapshot.Indexed != 2 {
		t.Fatalf("snapshot.Indexed=%d want=2", snapshot.Indexed)
	}
	if snapshot.Skipped != 3 {
		t.Fatalf("snapshot.Skipped=%d want=3", snapshot.Skipped)
	}
	if snapshot.Deleted != 2 {
		t.Fatalf("snapshot.Deleted=%d want=2", snapshot.Deleted)
	}
	if snapshot.Errors != 0 {
		t.Fatalf("snapshot.Errors=%d want=0", snapshot.Errors)
	}

	keep := st.docs["keep.txt"]
	if keep.Status != "ok" {
		t.Fatalf("keep.txt status=%q want=ok", keep.Status)
	}
	if keep.DocType != "text" {
		t.Fatalf("keep.txt doc_type=%q want=text", keep.DocType)
	}
	if keep.ContentHash == "" {
		t.Fatal("keep.txt content hash should not be empty")
	}

	code := st.docs["code/main.go"]
	if code.Status != "ok" || code.DocType != "code" {
		t.Fatalf("code/main.go unexpected doc: %#v", code)
	}

	archive := st.docs["archive.zip"]
	if archive.Status != "skipped" || archive.DocType != "archive" {
		t.Fatalf("archive.zip unexpected doc: %#v", archive)
	}

	secret := st.docs["secret.txt"]
	if secret.Status != "secret_excluded" {
		t.Fatalf("secret.txt status=%q want=secret_excluded", secret.Status)
	}

	excluded := st.docs["exclude/private.txt"]
	if !excluded.Deleted {
		t.Fatalf("exclude/private.txt should be marked deleted, got %#v", excluded)
	}

	gone := st.docs["gone.txt"]
	if !gone.Deleted {
		t.Fatalf("gone.txt should be marked deleted, got %#v", gone)
	}
}

func TestServiceRun_ReturnsErrorOnInvalidSecretPattern(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "keep.txt"), []byte("plain text"))

	cfg := config.Default()
	cfg.RootDir = root
	cfg.SecretPatterns = []string{"["}

	svc := mustNewIngestService(t, cfg, newMemoryStore())
	if err := svc.Run(context.Background()); err == nil {
		t.Fatal("expected error for invalid secret pattern")
	}
}

func TestServiceRun_ContextCancelled(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "keep.txt"), []byte("plain text"))

	cfg := config.Default()
	cfg.RootDir = root

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	svc := mustNewIngestService(t, cfg, newMemoryStore())
	if err := svc.Run(ctx); err == nil {
		t.Fatal("expected context cancellation error")
	}
}

// verify that when a document is upserted and processed, any generated
// representations include the persisted DocID instead of zero.  This
// guards against the previous bug where the in-memory doc lacked an ID and
// orphaned rows were written.
func TestProcessDocument_DocIDSetBeforeRepGeneration(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "foo.txt")
	if err := os.WriteFile(path, []byte("hello world"), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	cfg := config.Default()
	cfg.RootDir = root

	st := newMemoryStore()
	svc := mustNewIngestService(t, cfg, st)

	// run a single scan by invoking Run; service will create raw text
	// representation since memoryStore implements model.RepresentationStore.
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if len(st.reps) == 0 {
		t.Fatal("expected at least one representation")
	}
	if st.reps[0].DocID == 0 {
		t.Fatalf("representation created with zero DocID")
	}
}

func TestServiceRun_AudioGeneratesTranscriptRepresentation(t *testing.T) {
	root := t.TempDir()
	audioPath := filepath.Join(root, "audio", "sample.mp3")
	mustWriteFile(t, audioPath, []byte("fake-audio-bytes"))

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = filepath.Join(root, ".dir2mcp")

	st := newMemoryStore()
	svc := mustNewIngestService(t, cfg, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] hello\n[00:02] world"})

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if len(st.reps) != 1 {
		t.Fatalf("expected one representation, got %d", len(st.reps))
	}
	if st.reps[0].RepType != ingest.RepTypeTranscript {
		t.Fatalf("expected transcript rep type, got %q", st.reps[0].RepType)
	}
	if len(st.chunks) != 2 {
		t.Fatalf("expected two transcript chunks, got %d", len(st.chunks))
	}
	if len(st.spans) != 2 {
		t.Fatalf("expected two transcript spans, got %d", len(st.spans))
	}
	if st.spans[0].Kind != "time" || st.spans[0].StartMS != 0 || st.spans[0].EndMS != 2000 {
		t.Fatalf("unexpected first transcript span: %+v", st.spans[0])
	}
}

type errTranscriber struct {
	err error
}

func (e errTranscriber) Transcribe(context.Context, string, []byte) (string, error) {
	return "", e.err
}

func TestServiceRun_AudioTranscriberFailure_DoesNotFailRun(t *testing.T) {
	root := t.TempDir()
	audioPath := filepath.Join(root, "audio", "broken.mp3")
	mustWriteFile(t, audioPath, []byte("fake-audio-bytes"))

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = filepath.Join(root, ".dir2mcp")

	st := newMemoryStore()
	state := appstate.NewIndexingState(appstate.ModeIncremental)
	svc := mustNewIngestService(t, cfg, st)
	svc.SetIndexingState(state)
	svc.SetTranscriber(errTranscriber{err: errors.New("provider down")})

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run should continue on transcription failure, got: %v", err)
	}

	snapshot := state.Snapshot()
	if snapshot.Errors == 0 {
		t.Fatalf("expected indexing error count increment on transcription failure, got %+v", snapshot)
	}
	if len(st.reps) != 0 {
		t.Fatalf("expected no transcript representations when transcription fails, got %d", len(st.reps))
	}
	doc, ok := st.docs["audio/broken.mp3"]
	if !ok {
		t.Fatal("expected audio document to be upserted")
	}
	if doc.Status != "ok" {
		t.Fatalf("expected audio document status to remain ok, got %q", doc.Status)
	}
}

func TestDiscoverOptionsFromConfig_DefaultsRemainSafe(t *testing.T) {
	cfg := config.Default()
	options := ingest.DiscoverOptionsFromConfig(cfg)
	if options.FollowSymlinks {
		t.Fatal("expected follow_symlinks default to false")
	}
	if !options.UseGitIgnore {
		t.Fatal("expected gitignore default to true")
	}
	if options.MaxSizeBytes <= 0 {
		t.Fatalf("expected positive max size default, got %d", options.MaxSizeBytes)
	}
}

func TestTranscriberFromConfig_AutoWiresElevenLabsWhenAPIKeyPresent(t *testing.T) {
	cfg := config.Default()
	cfg.ElevenLabsAPIKey = "test-key"
	cfg.ElevenLabsBaseURL = "https://example.test"
	cfg.STTProvider = "elevenlabs"

	transcriber, err := ingest.TranscriberFromConfig(cfg)
	if err != nil {
		t.Fatalf("TranscriberFromConfig failed: %v", err)
	}
	if transcriber == nil {
		t.Fatal("expected transcriber instance")
	}
	client, ok := transcriber.(*elevenlabs.Client)
	if !ok {
		t.Fatalf("expected elevenlabs client, got %T", transcriber)
	}
	if client.BaseURL != "https://example.test" {
		t.Fatalf("unexpected base URL: %q", client.BaseURL)
	}
}

func TestTranscriberFromConfig_ExplicitProviderRequiresCredentials(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		wantErr  string
	}{
		{
			name:     "mistral missing key",
			provider: "mistral",
			wantErr:  "requires MISTRAL_API_KEY",
		},
		{
			name:     "elevenlabs missing key",
			provider: "elevenlabs",
			wantErr:  "requires ELEVENLABS_API_KEY",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.STTProvider = tc.provider
			cfg.MistralAPIKey = ""
			cfg.ElevenLabsAPIKey = ""

			transcriber, err := ingest.TranscriberFromConfig(cfg)
			if err == nil {
				t.Fatalf("expected error for provider %q", tc.provider)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error mismatch: got=%v want substring=%q", err, tc.wantErr)
			}
			if transcriber != nil {
				t.Fatalf("expected nil transcriber on config error, got %T", transcriber)
			}
		})
	}
}

func TestTranscriberFromConfig_AutoProviderWithoutCredentialsReturnsNil(t *testing.T) {
	cfg := config.Default()
	cfg.STTProvider = "auto"
	cfg.MistralAPIKey = ""
	cfg.ElevenLabsAPIKey = ""

	transcriber, err := ingest.TranscriberFromConfig(cfg)
	if err != nil {
		t.Fatalf("TranscriberFromConfig should not fail in auto mode without credentials: %v", err)
	}
	if transcriber != nil {
		t.Fatalf("expected nil transcriber in auto mode without credentials, got %T", transcriber)
	}
}

type memoryStore struct {
	docs map[string]model.Document
	// hold persisted representations for verification
	reps   []model.Representation
	chunks []model.Chunk
	spans  []model.Span
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		docs:   make(map[string]model.Document),
		reps:   make([]model.Representation, 0),
		chunks: make([]model.Chunk, 0),
		spans:  make([]model.Span, 0),
	}
}

func (s *memoryStore) Init(_ context.Context) error { return nil }

func (s *memoryStore) UpsertDocument(_ context.Context, doc model.Document) error {
	current, ok := s.docs[doc.RelPath]
	if ok {
		doc.DocID = current.DocID
	} else {
		doc.DocID = int64(len(s.docs) + 1)
	}
	s.docs[doc.RelPath] = doc
	return nil
}

// representationStore (model.RepresentationStore) methods ------------------------------------------------
func (s *memoryStore) UpsertRepresentation(_ context.Context, rep model.Representation) (int64, error) {
	rep.RepID = int64(len(s.reps) + 1)
	s.reps = append(s.reps, rep)
	return rep.RepID, nil
}

func (s *memoryStore) InsertChunkWithSpans(_ context.Context, chunk model.Chunk, spans []model.Span) (int64, error) {
	// assign a deterministic ID before storing, mirroring the behavior of
	// UpsertRepresentation above. This ensures that any tests examining the
	// returned identifier or verifying relationships between chunks and
	// spans will see consistent values.
	chunk.ChunkID = uint64(len(s.chunks) + 1)

	// store the chunk with its ID and append spans in sequence; the span
	// records themselves don’t hold the chunk ID, but callers may rely on the
	// returned ID to correlate the two.
	s.chunks = append(s.chunks, chunk)
	s.spans = append(s.spans, spans...)
	return int64(chunk.ChunkID), nil
}

func (s *memoryStore) SoftDeleteChunksFromOrdinal(_ context.Context, _ int64, _ int) error {
	return nil
}

// WithTx is a noop for the in-memory implementation since there is no
// underlying database to transact against.
func (s *memoryStore) WithTx(ctx context.Context, fn func(tx model.RepresentationStore) error) error {
	return fn(s)
}

func (s *memoryStore) GetDocumentByPath(_ context.Context, relPath string) (model.Document, error) {
	doc, ok := s.docs[relPath]
	if !ok {
		return model.Document{}, os.ErrNotExist
	}
	return doc, nil
}

func (s *memoryStore) ListFiles(_ context.Context, prefix, glob string, limit, offset int) ([]model.Document, int64, error) {
	if limit <= 0 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}

	keys := make([]string, 0, len(s.docs))
	for relPath := range s.docs {
		if strings.TrimSpace(prefix) != "" && !strings.HasPrefix(relPath, prefix) {
			continue
		}
		if strings.TrimSpace(glob) != "" {
			match, err := path.Match(glob, relPath)
			if err != nil || !match {
				continue
			}
		}
		keys = append(keys, relPath)
	}
	sort.Strings(keys)

	total := int64(len(keys))
	if offset >= len(keys) {
		return []model.Document{}, total, nil
	}
	end := offset + limit
	if end > len(keys) {
		end = len(keys)
	}

	out := make([]model.Document, 0, end-offset)
	for _, key := range keys[offset:end] {
		out = append(out, s.docs[key])
	}
	return out, total, nil
}

func (s *memoryStore) Close() error { return nil }

func (s *memoryStore) MarkDocumentDeleted(_ context.Context, relPath string) error {
	doc, ok := s.docs[relPath]
	if !ok {
		doc = model.Document{RelPath: relPath}
	}
	doc.Deleted = true
	s.docs[relPath] = doc
	return nil
}

func TestMemoryStoreListFilesPaging(t *testing.T) {
	st := newMemoryStore()
	st.docs["a.txt"] = model.Document{RelPath: "a.txt"}
	st.docs["b.txt"] = model.Document{RelPath: "b.txt"}
	st.docs["c.txt"] = model.Document{RelPath: "c.txt"}

	page1, total, err := st.ListFiles(context.Background(), "", "", 2, 0)
	if err != nil {
		t.Fatalf("ListFiles page1 failed: %v", err)
	}
	if total != 3 {
		t.Fatalf("total=%d want=3", total)
	}
	gotPage1 := []string{page1[0].RelPath, page1[1].RelPath}
	if !slices.Equal(gotPage1, []string{"a.txt", "b.txt"}) {
		t.Fatalf("page1 unexpected: %v", gotPage1)
	}

	page2, _, err := st.ListFiles(context.Background(), "", "", 2, 2)
	if err != nil {
		t.Fatalf("ListFiles page2 failed: %v", err)
	}
	if len(page2) != 1 || page2[0].RelPath != "c.txt" {
		t.Fatalf("page2 unexpected: %#v", page2)
	}
}

package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dir2mcp/internal/config"
	"dir2mcp/internal/ingest"
	"dir2mcp/internal/model"
	"dir2mcp/internal/retrieval"
)

type fakeTranscriber struct {
	text  string
	err   error
	calls int
}

func (f *fakeTranscriber) Transcribe(_ context.Context, _ string, _ []byte) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.text, nil
}

func TestGenerateTranscriptRepresentation_PersistsTimeChunks(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir}, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] intro\n[00:02] chapter one\n[00:05] chapter two"})

	doc := model.Document{
		DocID:   77,
		RelPath: "audio/lecture.mp3",
		DocType: "audio",
	}
	content := []byte("fake-audio-bytes")

	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, content); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation failed: %v", err)
	}

	if len(st.reps) != 1 {
		t.Fatalf("expected one transcript representation, got %d", len(st.reps))
	}
	if st.reps[0].RepType != ingest.RepTypeTranscript {
		t.Fatalf("expected rep type %q, got %q", ingest.RepTypeTranscript, st.reps[0].RepType)
	}
	if len(st.chunks) != 3 {
		t.Fatalf("expected three transcript chunks, got %d", len(st.chunks))
	}
	if len(st.spans) != 3 {
		t.Fatalf("expected three transcript spans, got %d", len(st.spans))
	}
	if st.spans[0].Kind != "time" || st.spans[0].StartMS != 0 || st.spans[0].EndMS != 2000 {
		t.Fatalf("unexpected first transcript span: %+v", st.spans[0])
	}
	if st.spans[1].Kind != "time" || st.spans[1].StartMS != 2000 || st.spans[1].EndMS != 5000 {
		t.Fatalf("unexpected second transcript span: %+v", st.spans[1])
	}
	// the last segment has no following timestamp, so endMS is computed as
	// startMS + estimated duration. with two words the estimate is 1000ms
	// (minimum), hence we expect exactly 6000.
	if st.spans[2].Kind != "time" || st.spans[2].StartMS != 5000 || st.spans[2].EndMS != 6000 {
		t.Fatalf("unexpected third transcript span (kind=%s start=%d end=%d); expect endMS==6000", st.spans[2].Kind, st.spans[2].StartMS, st.spans[2].EndMS)
	}

	cachePath := filepath.Join(stateDir, "cache", "transcribe", ingest.ComputeContentHash(content)+".txt")
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("expected transcript cache file at %s: %v", cachePath, err)
	}
}

func TestReadOrComputeTranscript_UsesCache(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	content := []byte("same-audio-bytes")
	cachePath := filepath.Join(stateDir, "cache", "transcribe", ingest.ComputeContentHash(content)+".txt")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatalf("mkdir transcript cache dir: %v", err)
	}
	if err := os.WriteFile(cachePath, []byte("[00:00] cached transcript"), 0o644); err != nil {
		t.Fatalf("seed transcript cache: %v", err)
	}

	transcriber := &fakeTranscriber{text: "[00:00] fresh transcript"}
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir}, nil)
	svc.SetTranscriber(transcriber)

	doc := model.Document{RelPath: "audio/cached.mp3", DocType: "audio"}
	got, err := svc.ReadOrComputeTranscript(context.Background(), doc, content)
	if err != nil {
		t.Fatalf("ReadOrComputeTranscript failed: %v", err)
	}
	if got != "[00:00] cached transcript" {
		t.Fatalf("expected cached transcript, got %q", got)
	}
	if transcriber.calls != 0 {
		t.Fatalf("expected transcriber not called, got %d call(s)", transcriber.calls)
	}
}

func TestGenerateTranscriptRepresentation_TranscriberError(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir}, st)
	svc.SetTranscriber(&fakeTranscriber{err: errors.New("provider down")})

	doc := model.Document{DocID: 9, RelPath: "audio/fail.wav", DocType: "audio"}
	err := svc.GenerateTranscriptRepresentation(context.Background(), doc, []byte("wav"))
	if err == nil {
		t.Fatal("expected transcriber error")
	}
	if len(st.reps) != 0 {
		t.Fatalf("expected no representations on transcriber failure, got %d", len(st.reps))
	}
}

func TestReadOrComputeTranscript_PrunesCacheByTTL(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir}, nil)
	svc.SetOCRCacheLimits(0, time.Second)

	oldContent := []byte("old-audio")
	oldPath := filepath.Join(stateDir, "cache", "transcribe", ingest.ComputeContentHash(oldContent)+".txt")
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		t.Fatalf("mkdir transcript cache dir: %v", err)
	}
	if err := os.WriteFile(oldPath, []byte("[00:00] old"), 0o644); err != nil {
		t.Fatalf("write old transcript cache: %v", err)
	}
	oldTime := time.Now().Add(-2 * time.Second)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes old transcript: %v", err)
	}

	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] fresh transcript"})
	if _, err := svc.ReadOrComputeTranscript(context.Background(), model.Document{RelPath: "audio/new.mp3", DocType: "audio"}, []byte("new-audio")); err != nil {
		t.Fatalf("ReadOrComputeTranscript failed: %v", err)
	}

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expected old transcript cache file removed by TTL, got err=%v", err)
	}
}

type fixedLabelsIndex struct {
	labels []uint64
}

func (i *fixedLabelsIndex) Add(uint64, []float32) error { return nil }
func (i *fixedLabelsIndex) Save(string) error           { return nil }
func (i *fixedLabelsIndex) Load(string) error           { return nil }
func (i *fixedLabelsIndex) Close() error                { return nil }
func (i *fixedLabelsIndex) Search([]float32, int) ([]uint64, []float32, error) {
	scores := make([]float32, len(i.labels))
	for n := range scores {
		scores[n] = float32(len(scores) - n)
	}
	return append([]uint64(nil), i.labels...), scores, nil
}

type staticEmbedder struct {
	vec []float32
}

// Embed returns one fixed vector per input text. The method copies the
// underlying vector for each element of texts so callers can safely modify
// the returned slices without affecting the mock's stored vector.
func (e *staticEmbedder) Embed(ctx context.Context, model string, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = append([]float32(nil), e.vec...)
	}
	return out, nil
}

func TestTranscriptIngest_EndToEnd_AppearsInAskWithCitations(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir}, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] intro\n[00:02] chapter one\n[00:05] chapter two"})

	doc := model.Document{
		DocID:   77,
		RelPath: "audio/lecture.mp3",
		DocType: "audio",
	}
	content := []byte("fake-audio-bytes")
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, content); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation failed: %v", err)
	}
	if len(st.chunks) == 0 || len(st.spans) == 0 {
		t.Fatal("expected transcript chunks and spans to be persisted")
	}

	labels := make([]uint64, 0, len(st.chunks))
	idx := &fixedLabelsIndex{}
	ret := retrieval.NewService(nil, idx, &staticEmbedder{vec: []float32{1, 0}}, nil)
	for i := range st.chunks {
		label := uint64(i + 1)
		labels = append(labels, label)
		ret.SetChunkMetadataForIndex("text", label, model.SearchHit{
			ChunkID: label,
			RelPath: doc.RelPath,
			DocType: doc.DocType,
			RepType: ingest.RepTypeTranscript,
			Snippet: st.chunks[i].Text,
			Span:    st.spans[i],
		})
	}
	idx.labels = labels

	res, err := ret.Ask(context.Background(), "What is in the lecture transcript?", model.SearchQuery{
		K:     3,
		Index: "text",
	})
	if err != nil {
		t.Fatalf("Ask failed: %v", err)
	}
	if len(res.Citations) == 0 {
		t.Fatal("expected citations in ask result")
	}
	if res.Citations[0].RelPath != doc.RelPath {
		t.Fatalf("unexpected citation rel_path: got %q want %q", res.Citations[0].RelPath, doc.RelPath)
	}
	if res.Citations[0].Span.Kind != "time" {
		t.Fatalf("expected time span citation, got %+v", res.Citations[0].Span)
	}
	if len(res.Hits) == 0 || res.Hits[0].RepType != ingest.RepTypeTranscript {
		t.Fatalf("expected transcript hit, got %+v", res.Hits)
	}
}

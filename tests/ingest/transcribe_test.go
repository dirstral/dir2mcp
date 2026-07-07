package tests

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
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

	assertTranscriptRepresentationCounts(t, st)
	assertTranscriptSpanWindows(t, st)

	cachePath := filepath.Join(stateDir, "cache", "transcribe", ingest.ComputeContentHash(content)+".txt")
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("expected transcript cache file at %s: %v", cachePath, err)
	}
}

func assertTranscriptRepresentationCounts(t *testing.T, st *fakeIngestStore) {
	t.Helper()
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
}

func assertTranscriptSpanWindows(t *testing.T, st *fakeIngestStore) {
	t.Helper()
	if st.spans[0].Kind != "time" || st.spans[0].StartMS != 0 || st.spans[0].EndMS != 2000 {
		t.Fatalf("unexpected first transcript span: %+v", st.spans[0])
	}
	if st.spans[1].Kind != "time" || st.spans[1].StartMS != 2000 || st.spans[1].EndMS != 5000 {
		t.Fatalf("unexpected second transcript span: %+v", st.spans[1])
	}
	if st.spans[2].Kind != "time" || st.spans[2].StartMS != 5000 || st.spans[2].EndMS != 6000 {
		t.Fatalf("unexpected third transcript span (kind=%s start=%d end=%d); expect endMS==6000", st.spans[2].Kind, st.spans[2].StartMS, st.spans[2].EndMS)
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
	got, err := svc.ReadOrComputeTranscript(context.Background(), doc, content, "")
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
	if _, err := svc.ReadOrComputeTranscript(context.Background(), model.Document{RelPath: "audio/new.mp3", DocType: "audio"}, []byte("new-audio"), ""); err != nil {
		t.Fatalf("ReadOrComputeTranscript failed: %v", err)
	}

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expected old transcript cache file removed by TTL, got err=%v", err)
	}
}

type fixedLabelsIndex struct {
	labels []uint64
}

func (i *fixedLabelsIndex) Upsert(context.Context, []float32, model.IndexPayload) error { return nil }
func (i *fixedLabelsIndex) Delete(context.Context, []uint64) error                      { return nil }
func (i *fixedLabelsIndex) Identity(context.Context) (string, error)                    { return "", nil }
func (i *fixedLabelsIndex) Reset(context.Context, string) error                         { return nil }
func (i *fixedLabelsIndex) Close() error                                                { return nil }
func (i *fixedLabelsIndex) Search(_ context.Context, _ []float32, _ int, _ model.Filter) ([]model.IndexHit, error) {
	hits := make([]model.IndexHit, len(i.labels))
	for n, label := range i.labels {
		hits[n] = model.IndexHit{ChunkID: label, Score: float32(len(i.labels) - n)}
	}
	return hits, nil
}

type staticEmbedder struct {
	vec []float32
}

// Embed returns one fixed vector per input text. The method copies the
// underlying vector for each element of texts so callers can safely modify
// the returned slices without affecting the mock's stored vector.
func (e *staticEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, texts []string) ([][]float32, error) {
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

// fakeStructuredTranscriber implements model.StructuredTranscriber, returning
// segment text plus per-word timing (mirrors the whisper client #252).
type fakeStructuredTranscriber struct {
	text  string
	words []model.TimedWord
}

func (f *fakeStructuredTranscriber) Transcribe(_ context.Context, _ string, _ []byte) (string, error) {
	return f.text, nil
}

func (f *fakeStructuredTranscriber) TranscribeStructured(_ context.Context, _ string, _ []byte) (model.TranscriptResult, error) {
	return model.TranscriptResult{Text: f.text, Words: f.words}, nil
}

// TestGenerateTranscriptRepresentation_AttachesWordSpans asserts a structured
// transcriber's per-word timing is carried onto the persisted transcript chunk
// spans (spec §8.6.1) without changing the chunk count or text.
func TestGenerateTranscriptRepresentation_AttachesWordSpans(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir}, st)
	svc.SetTranscriber(&fakeStructuredTranscriber{
		text: "[00:00] intro\n[00:02] chapter one\n[00:05] chapter two",
		words: []model.TimedWord{
			{Word: "intro", StartMS: 0, EndMS: 800},
			{Word: "chapter", StartMS: 2000, EndMS: 2500},
			{Word: "one", StartMS: 2500, EndMS: 2900},
			{Word: "chapter", StartMS: 5000, EndMS: 5400},
			{Word: "two", StartMS: 5400, EndMS: 5800},
		},
	})

	doc := model.Document{DocID: 88, RelPath: "audio/lecture.mp3", DocType: "audio"}
	content := []byte("structured-audio-bytes")

	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, content); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation failed: %v", err)
	}

	// Same three chunks/spans as the words-absent baseline.
	if len(st.chunks) != 3 || len(st.spans) != 3 {
		t.Fatalf("chunks=%d spans=%d, want 3/3 (words must not add chunks)", len(st.chunks), len(st.spans))
	}

	// Words land in the chunk whose time span contains them.
	total := 0
	for _, sp := range st.spans {
		for _, w := range sp.Words {
			total++
			if w.T < sp.StartMS {
				t.Errorf("word %q at %dms before span [%d,%d)", w.W, w.T, sp.StartMS, sp.EndMS)
			}
		}
	}
	if total != 5 {
		t.Fatalf("attached %d words across spans, want 5", total)
	}
	if len(st.spans[0].Words) != 1 || st.spans[0].Words[0].W != "intro" || st.spans[0].Words[0].D != 800 {
		t.Errorf("first span words = %+v, want [{0,800,intro}]", st.spans[0].Words)
	}

	// A per-language words sidecar cache is written next to the transcript.
	wordsCache := filepath.Join(stateDir, "cache", "transcribe", ingest.ComputeContentHash(content)+".words.json")
	if _, err := os.Stat(wordsCache); err != nil {
		t.Fatalf("expected words sidecar cache at %s: %v", wordsCache, err)
	}

	// §8.6.9: a word-timed transcript declares word granularity via meta_json.words.
	if got := transcriptMetaWordsFlag(t, st.reps[0].MetaJSON); got != true {
		t.Errorf("meta_json.words = %v, want true for a word-timed transcript (meta=%s)", got, st.reps[0].MetaJSON)
	}
}

// transcriptMetaWordsFlag decodes the §8.6.9 word-granularity flag from a
// transcript representation's meta_json. A missing key decodes to false, which
// §8.6.9 defines as "segment granularity only".
func transcriptMetaWordsFlag(t *testing.T, metaJSON string) bool {
	t.Helper()
	var meta struct {
		Words bool `json:"words"`
	}
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		t.Fatalf("meta_json %q is not valid JSON: %v", metaJSON, err)
	}
	return meta.Words
}

// TestGenerateTranscriptRepresentation_SegmentOnlyOmitsWordsFlag asserts a
// segment-only transcript (a plain, non-structured transcriber) records no
// word-granularity flag, so meta_json.words is absent/false (§8.6.9).
func TestGenerateTranscriptRepresentation_SegmentOnlyOmitsWordsFlag(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir}, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] intro\n[00:02] chapter one\n[00:05] chapter two"})

	doc := model.Document{DocID: 89, RelPath: "audio/segment.mp3", DocType: "audio"}
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, []byte("segment-only-bytes")); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation failed: %v", err)
	}
	if len(st.reps) != 1 {
		t.Fatalf("expected one transcript representation, got %d", len(st.reps))
	}
	if got := transcriptMetaWordsFlag(t, st.reps[0].MetaJSON); got != false {
		t.Errorf("meta_json.words = %v, want false/absent for a segment-only transcript (meta=%s)", got, st.reps[0].MetaJSON)
	}
	if strings.Contains(st.reps[0].MetaJSON, `"words"`) {
		t.Errorf("segment-only transcript must omit the words key, got %s", st.reps[0].MetaJSON)
	}
}

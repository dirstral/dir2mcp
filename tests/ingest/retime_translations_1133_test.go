package tests

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Re-timing of stored chat-engine translations (SPEC §8.6.2, dir2mcp #1133).
//
// A chat-engine translation stored before the "source-spoken" rule carries the
// older spans: each translated line ended where the next line started. The
// reconcile pass re-times such a representation from the source transcript's
// word timings, without a model call, records span_rule, and never looks at it
// again.

func timeChunk(id uint64, text string, startMS, endMS int) store.ChunkTimeSpan {
	return store.ChunkTimeSpan{ChunkID: id, Ordinal: int(id), Text: text, Span: model.Span{Kind: "time", StartMS: startMS, EndMS: endMS}}
}

// The source line [0,10000) was spoken until 1200; the translated chunk that
// carries the whole window ends there now. A middle line whose words end past
// its window is left as it is: the window end is the next marker.
func TestRetime_SingleLineWindowTakesSpokenEnd(t *testing.T) {
	t.Parallel()
	words := []model.TimedWord{{Word: "a", StartMS: 0, EndMS: 500}, {Word: "b", StartMS: 600, EndMS: 1200}, {Word: "c", StartMS: 10000, EndMS: 10800}, {Word: "d", StartMS: 20000, EndMS: 20200}}
	chunks := []store.ChunkTimeSpan{timeChunk(1, "one two", 0, 10000), timeChunk(2, "three", 10000, 10300), timeChunk(3, "four", 20000, 20200)}
	updates := ingest.RetimeTranslatedChunksForTest(chunks, words)
	if len(updates) != 1 || updates[0].ChunkID != 1 {
		t.Fatalf("updates = %+v, want one update for chunk 1", updates)
	}
	if updates[0].Span.StartMS != 0 || updates[0].Span.EndMS != 1200 {
		t.Fatalf("chunk 1 span = [%d,%d], want [0,1200]", updates[0].Span.StartMS, updates[0].Span.EndMS)
	}
}

// A merged retrieval window (SPEC 8.6.1) records one cue per source line; each
// cue takes the spoken end of its own words and the window ends at its last cue.
func TestRetime_MergedWindowRetimesEachCue(t *testing.T) {
	t.Parallel()
	words := []model.TimedWord{{Word: "a", StartMS: 0, EndMS: 900}, {Word: "b", StartMS: 5000, EndMS: 5400}, {Word: "c", StartMS: 20000, EndMS: 20500}}
	merged := timeChunk(7, "one two three", 0, 20000)
	merged.Span.Cues = []model.CueSpan{{T: 0, D: 5000, N: 3}, {T: 5000, D: 15000, N: 9}}
	updates := ingest.RetimeTranslatedChunksForTest([]store.ChunkTimeSpan{merged, timeChunk(8, "four", 20000, 21000)}, words)
	if len(updates) != 2 || updates[0].ChunkID != 7 || updates[1].ChunkID != 8 {
		t.Fatalf("updates = %+v, want the merged chunk and the plain chunk after it", updates)
	}
	if updates[1].Span.EndMS != 20500 {
		t.Fatalf("plain chunk after the window = %+v, want end 20500", updates[1].Span)
	}
	got := updates[0].Span
	if got.StartMS != 0 || got.EndMS != 5400 {
		t.Fatalf("merged span = [%d,%d], want [0,5400] (the last cue's spoken end)", got.StartMS, got.EndMS)
	}
	if len(got.Cues) != 2 || got.Cues[0].D != 900 || got.Cues[1].T != 5000 || got.Cues[1].D != 400 {
		t.Fatalf("cues = %+v, want [{0 900 3} {5000 400 9}]", got.Cues)
	}
	if got.Cues[0].N != 3 || got.Cues[1].N != 9 {
		t.Fatalf("cue rune lengths changed: %+v", got.Cues)
	}
}

// The two parts of one split line share the chunker's text overlap at their
// boundary; they take the line's spoken span split by rune weight.
func TestRetime_SplitLineRedistributesByRuneWeight(t *testing.T) {
	t.Parallel()
	overlap := strings.Repeat("overlap text ", 5) // 65 runes shared at the boundary
	first := strings.Repeat("x", 100) + overlap
	second := overlap + strings.Repeat("y", 100)
	words := []model.TimedWord{{Word: "a", StartMS: 0, EndMS: 2000}, {Word: "b", StartMS: 2500, EndMS: 3300}}
	chunks := []store.ChunkTimeSpan{timeChunk(1, first, 0, 5000), timeChunk(2, second, 5000, 10000), timeChunk(3, "next line", 10000, 12000)}
	updates := ingest.RetimeTranslatedChunksForTest(chunks, words)
	if len(updates) != 2 {
		t.Fatalf("updates = %+v, want the two parts", updates)
	}
	if updates[0].Span.StartMS != 0 || updates[0].Span.EndMS != 1650 {
		t.Fatalf("part 1 = [%d,%d], want [0,1650] (half of 3300 by equal rune weight)", updates[0].Span.StartMS, updates[0].Span.EndMS)
	}
	if updates[1].Span.StartMS != 1650 || updates[1].Span.EndMS != 3300 {
		t.Fatalf("part 2 = [%d,%d], want [1650,3300]", updates[1].Span.StartMS, updates[1].Span.EndMS)
	}
}

// No source word starts inside the window: the chunk keeps its stored span.
func TestRetime_WindowWithoutWordsIsLeft(t *testing.T) {
	t.Parallel()
	words := []model.TimedWord{{Word: "a", StartMS: 30000, EndMS: 30500}}
	updates := ingest.RetimeTranslatedChunksForTest([]store.ChunkTimeSpan{timeChunk(1, "one", 0, 10000)}, words)
	if len(updates) != 0 {
		t.Fatalf("updates = %+v, want none", updates)
	}
}

// retimingService is a service that transcribes with per-word timing and
// translates into English with the chat engine, so a scan stores a translated
// transcript under the source-spoken rule.
func retimingService(t *testing.T, cfg config.Config, st model.Store, transcriber model.Transcriber) *ingest.Service {
	t.Helper()
	svc := mustNewIngestService(t, cfg, st)
	svc.SetTranscriber(transcriber)
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	svc.SetTranscriptLanguage("ru")
	svc.SetTranslator(&fakeTranslator{}, "mistral", "mistral-small-2506", cfg.MediaTranslateTargetLangs)
	return svc
}

func wordedTranscriber() *fakeStructuredTranscriber {
	return &fakeStructuredTranscriber{
		text: "[00:00] раз два\n[00:10] три",
		words: []model.TimedWord{
			{Word: "раз", StartMS: 0, EndMS: 500},
			{Word: "два", StartMS: 600, EndMS: 1200},
			{Word: "три", StartMS: 10000, EndMS: 10800},
		},
	}
}

func repMetaJSON(t *testing.T, st *store.SQLiteStore, relPath string, repID int64) string {
	t.Helper()
	reps, err := st.ActiveRepresentations(context.Background(), relPath)
	if err != nil {
		t.Fatalf("ActiveRepresentations: %v", err)
	}
	for _, rep := range reps {
		if rep.RepID == repID {
			return rep.MetaJSON
		}
	}
	t.Fatalf("rep %d is not live on %s", repID, relPath)
	return ""
}

// ageTranslation turns a freshly stored translation into one stored before the
// rule: the span_rule marker is removed (and the given extra meta fields set),
// the first chunk's span is widened back to the next line's start, and the
// recorded pipeline output identity is the pre-rule one, so the next scan arms
// the reconcile pass exactly as an upgrade would.
func ageTranslation(t *testing.T, st *store.SQLiteStore, relPath string, repID int64, extra map[string]any) {
	t.Helper()
	ctx := context.Background()
	var meta map[string]any
	if err := json.Unmarshal([]byte(repMetaJSON(t, st, relPath, repID)), &meta); err != nil {
		t.Fatalf("meta: %v", err)
	}
	delete(meta, "span_rule")
	for k, v := range extra {
		meta[k] = v
	}
	metaJSON, _ := json.Marshal(meta)
	chunks, err := st.ChunkTimeSpans(ctx, repID)
	if err != nil {
		t.Fatalf("ChunkTimeSpans: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("translated chunks = %d, want 2: %+v", len(chunks), chunks)
	}
	old := chunks[0].Span
	old.EndMS = chunks[1].Span.StartMS
	if err := st.RetimeRepresentation(ctx, repID, string(metaJSON), []store.ChunkSpanUpdate{{ChunkID: chunks[0].ChunkID, Span: old}}); err != nil {
		t.Fatalf("age translation: %v", err)
	}
	if err := st.SetPipelineOutputIdentity(ctx, "translate=on:en|summary=off"); err != nil {
		t.Fatalf("record pre-rule identity: %v", err)
	}
}

func translatedSpans(t *testing.T, st *store.SQLiteStore, repID int64) []model.Span {
	t.Helper()
	chunks, err := st.ChunkTimeSpans(context.Background(), repID)
	if err != nil {
		t.Fatalf("ChunkTimeSpans: %v", err)
	}
	out := make([]model.Span, 0, len(chunks))
	for _, ch := range chunks {
		out = append(out, ch.Span)
	}
	return out
}

// The headline case: a corpus with a pre-rule translation is re-timed by the
// first scan after the upgrade, from the stored source words and without a
// model call, and a further scan changes nothing.
func TestRetime_StoredTranslationIsRetimedOnceByTheReconcilePass(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	ctx := context.Background()
	cfg := translateConfig(root, stateDir, "en")

	if err := retimingService(t, cfg, st, wordedTranscriber()).Run(ctx); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	enRepID := repIDForType(t, st, "talk.mp3", ingest.TranscriptRepType("en"))
	if !strings.Contains(repMetaJSON(t, st, "talk.mp3", enRepID), `"span_rule":"source-spoken"`) {
		t.Fatalf("a fresh translation must carry the marker: %s", repMetaJSON(t, st, "talk.mp3", enRepID))
	}
	ageTranslation(t, st, "talk.mp3", enRepID, nil)
	if spans := translatedSpans(t, st, enRepID); spans[0].EndMS != 10000 {
		t.Fatalf("aged span = %+v, want the next-start end 10000", spans[0])
	}

	translator := &fakeTranslator{}
	second := mustNewIngestService(t, cfg, st)
	second.SetTranscriber(wordedTranscriber())
	second.SetSTTIdentity("whisper", "whisper-large-v3")
	second.SetTranscriptLanguage("ru")
	second.SetTranslator(translator, "mistral", "mistral-small-2506", cfg.MediaTranslateTargetLangs)
	if err := second.Run(ctx); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	spans := translatedSpans(t, st, enRepID)
	if spans[0].StartMS != 0 || spans[0].EndMS != 1200 {
		t.Fatalf("re-timed span = [%d,%d], want [0,1200] (the spoken end of the source line)", spans[0].StartMS, spans[0].EndMS)
	}
	if spans[1].StartMS != 10000 {
		t.Fatalf("second span moved: %+v", spans[1])
	}
	meta := repMetaJSON(t, st, "talk.mp3", enRepID)
	if !strings.Contains(meta, `"span_rule":"source-spoken"`) {
		t.Fatalf("re-timed translation lacks the marker: %s", meta)
	}
	if !strings.Contains(meta, `"translate_model":"mistral-small-2506"`) || !strings.Contains(meta, `"source_language":"ru"`) {
		t.Fatalf("re-time dropped recorded meta fields: %s", meta)
	}
	if translator.calls != 0 {
		t.Fatalf("the re-time pass called the translator %d times; it must not re-translate", translator.calls)
	}

	// A further scan: the identity is recorded, the pass is not armed, and the
	// representation is unchanged byte for byte.
	if err := retimingService(t, cfg, st, wordedTranscriber()).Run(ctx); err != nil {
		t.Fatalf("third scan: %v", err)
	}
	if again := translatedSpans(t, st, enRepID); !reflect.DeepEqual(again, spans) {
		t.Fatalf("third scan changed the spans: %+v -> %+v", spans, again)
	}
	if again := repMetaJSON(t, st, "talk.mp3", enRepID); again != meta {
		t.Fatalf("third scan changed the meta: %s -> %s", meta, again)
	}
}

// A whisper-engine translation carries its own per-word timing (meta
// words=true) and is never re-timed, with or without the marker.
func TestRetime_WhisperTranslationIsUntouched(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	ctx := context.Background()
	cfg := translateConfig(root, stateDir, "en")

	if err := retimingService(t, cfg, st, wordedTranscriber()).Run(ctx); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	enRepID := repIDForType(t, st, "talk.mp3", ingest.TranscriptRepType("en"))
	ageTranslation(t, st, "talk.mp3", enRepID, map[string]any{"words": true})
	before := translatedSpans(t, st, enRepID)
	metaBefore := repMetaJSON(t, st, "talk.mp3", enRepID)

	if err := retimingService(t, cfg, st, wordedTranscriber()).Run(ctx); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if after := translatedSpans(t, st, enRepID); !reflect.DeepEqual(after, before) {
		t.Fatalf("whisper-style translation was re-timed: %+v -> %+v", before, after)
	}
	if after := repMetaJSON(t, st, "talk.mp3", enRepID); after != metaBefore {
		t.Fatalf("whisper-style translation meta changed: %s -> %s", metaBefore, after)
	}
}

// A source transcript without word timings cannot improve the translation: the
// spans stay as stored and only the marker is recorded, so the pass does not
// revisit the representation on every armed scan.
func TestRetime_SourceWithoutWordsRecordsOnlyTheMarker(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	ctx := context.Background()
	cfg := translateConfig(root, stateDir, "en")
	plain := &fakeTranscriber{text: "[00:00] раз два\n[00:10] три"}

	if err := retimingService(t, cfg, st, plain).Run(ctx); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	// A new corpus has no recorded identity, so its first scan already arms the
	// pass: the translation was stored without the marker (no source words at
	// derivation) and the pass recorded it in the same scan, spans untouched.
	enRepID := repIDForType(t, st, "talk.mp3", ingest.TranscriptRepType("en"))
	before := translatedSpans(t, st, enRepID)
	if len(before) != 2 || before[0].EndMS != 10000 {
		t.Fatalf("spans after the first scan = %+v, want the next-start end 10000 on the first line", before)
	}
	if !strings.Contains(repMetaJSON(t, st, "talk.mp3", enRepID), `"span_rule":"source-spoken"`) {
		t.Fatalf("marker not recorded on the first armed scan: %s", repMetaJSON(t, st, "talk.mp3", enRepID))
	}
	if err := st.SetPipelineOutputIdentity(ctx, "translate=on:en|summary=off"); err != nil {
		t.Fatalf("record pre-rule identity: %v", err)
	}

	if err := retimingService(t, cfg, st, plain).Run(ctx); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if after := translatedSpans(t, st, enRepID); !reflect.DeepEqual(after, before) {
		t.Fatalf("spans changed without source words: %+v -> %+v", before, after)
	}
	if !strings.Contains(repMetaJSON(t, st, "talk.mp3", enRepID), `"span_rule":"source-spoken"`) {
		t.Fatalf("marker lost: %s", repMetaJSON(t, st, "talk.mp3", enRepID))
	}
}

// failingRetimeStore is the real store with one failing re-time write: the
// scan must then leave the pipeline output identity unrecorded, so the next
// scan arms the pass again and retries the representation.
type failingRetimeStore struct {
	*store.SQLiteStore
	failures int
}

func (f *failingRetimeStore) RetimeRepresentation(ctx context.Context, repID int64, metaJSON string, updates []store.ChunkSpanUpdate) error {
	if f.failures > 0 {
		f.failures--
		return errors.New("injected re-time failure")
	}
	return f.SQLiteStore.RetimeRepresentation(ctx, repID, metaJSON, updates)
}

func TestRetime_FailedWriteLeavesTheIdentityUnrecordedSoTheNextScanRetries(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	ctx := context.Background()
	cfg := translateConfig(root, stateDir, "en")

	if err := retimingService(t, cfg, st, wordedTranscriber()).Run(ctx); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	enRepID := repIDForType(t, st, "talk.mp3", ingest.TranscriptRepType("en"))
	ageTranslation(t, st, "talk.mp3", enRepID, nil)

	failing := &failingRetimeStore{SQLiteStore: st, failures: 1}
	if err := retimingService(t, cfg, failing, wordedTranscriber()).Run(ctx); err != nil {
		t.Fatalf("scan with the failing write: %v", err)
	}
	if spans := translatedSpans(t, st, enRepID); spans[0].EndMS != 10000 {
		t.Fatalf("spans changed although the write failed: %+v", spans)
	}
	if recorded, err := st.PipelineOutputIdentity(ctx); err != nil || recorded != "translate=on:en|summary=off" {
		t.Fatalf("identity after the failed scan = %q (err %v), want the pre-rule value kept so the next scan retries", recorded, err)
	}

	if err := retimingService(t, cfg, st, wordedTranscriber()).Run(ctx); err != nil {
		t.Fatalf("retry scan: %v", err)
	}
	if spans := translatedSpans(t, st, enRepID); spans[0].EndMS != 1200 {
		t.Fatalf("retry scan did not re-time: %+v", spans)
	}
	if !strings.Contains(repMetaJSON(t, st, "talk.mp3", enRepID), `"span_rule":"source-spoken"`) {
		t.Fatalf("retry scan did not record the marker")
	}
	if recorded, err := st.PipelineOutputIdentity(ctx); err != nil || recorded == "translate=on:en|summary=off" {
		t.Fatalf("identity after the retry = %q (err %v), want the current value recorded", recorded, err)
	}
}

// The transcript's last line has no next marker, only the chunker's pace
// estimate as its stored end. Its words are the better measure, so the final
// chunk takes their end even past the estimate, as a fresh translation does.
func TestRetime_LastLineTakesTheSpokenEndPastTheEstimate(t *testing.T) {
	t.Parallel()
	words := []model.TimedWord{{Word: "a", StartMS: 0, EndMS: 800}, {Word: "b", StartMS: 10000, EndMS: 13500}}
	chunks := []store.ChunkTimeSpan{timeChunk(1, "one", 0, 10000), timeChunk(2, "two", 10000, 11000)}
	updates := ingest.RetimeTranslatedChunksForTest(chunks, words)
	if len(updates) != 2 {
		t.Fatalf("updates = %+v, want both chunks", updates)
	}
	if updates[0].Span.EndMS != 800 {
		t.Fatalf("first line end = %d, want 800", updates[0].Span.EndMS)
	}
	if updates[1].Span.StartMS != 10000 || updates[1].Span.EndMS != 13500 {
		t.Fatalf("last line = [%d,%d], want [10000,13500] (past the 11000 estimate)", updates[1].Span.StartMS, updates[1].Span.EndMS)
	}
}

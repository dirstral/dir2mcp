package tests

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Issue #961 / SPEC §8.6.13. A 73-minute recording was scheduled as eight decode
// windows and seven of them failed. The merged transcript covered the first ten
// minutes and was indexed as if it were whole: status ok, chunks searchable, and
// nothing anywhere saying that 88% of the audio was never transcribed. These
// tests pin the coverage record that says so, and the optional floor that refuses
// a transcript too thin to be honest.

// coverageMeta is the §8.6.13 coverage object as it is persisted on a transcript
// representation's meta_json. It is decoded from JSON rather than read off the
// internal struct so the test asserts the WIRE shape a consumer reads.
type coverageMeta struct {
	WindowsAttempted int `json:"windows_attempted"`
	WindowsDecoded   int `json:"windows_decoded"`
	DecodedMS        int `json:"decoded_ms"`
	DurationMS       int `json:"duration_ms"`
	Ranges           []struct {
		StartMS int `json:"start_ms"`
		EndMS   int `json:"end_ms"`
	} `json:"ranges"`
}

// transcriptCoverageFromMeta decodes the coverage object off a persisted
// representation's meta_json. present is false when the key is absent, which per
// §5.2 means "no assertion" and never "complete".
func transcriptCoverageFromMeta(t *testing.T, metaJSON string) (cov coverageMeta, present bool) {
	t.Helper()
	var envelope struct {
		Coverage *coverageMeta `json:"coverage"`
	}
	if err := json.Unmarshal([]byte(metaJSON), &envelope); err != nil {
		t.Fatalf("decode transcript meta_json %q: %v", metaJSON, err)
	}
	if envelope.Coverage == nil {
		return coverageMeta{}, false
	}
	return *envelope.Coverage, true
}

// onlyTranscriptCoverage runs a windowed decode through the harness and returns
// the coverage recorded on the single transcript representation it persisted.
func onlyTranscriptCoverage(t *testing.T, h *windowSTTHarness, relPath string, content []byte) (coverageMeta, bool) {
	t.Helper()
	if err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc(relPath), content); err != nil {
		t.Fatalf("transcription failed: %v", err)
	}
	if len(h.store.reps) != 1 {
		t.Fatalf("persisted %d representations, want exactly 1", len(h.store.reps))
	}
	return transcriptCoverageFromMeta(t, h.store.reps[0].MetaJSON)
}

// assertRangesAscendingAndDisjoint pins the §8.6.13 shape rule: coalesced,
// non-overlapping, ascending by start_ms. A consumer reads the GAPS off this
// array, so an unsorted or overlapping array makes the gaps unreadable.
func assertRangesAscendingAndDisjoint(t *testing.T, cov coverageMeta) {
	t.Helper()
	prevEnd := -1
	for i, r := range cov.Ranges {
		if r.EndMS <= r.StartMS {
			t.Errorf("range %d is empty or inverted: [%d,%d]", i, r.StartMS, r.EndMS)
		}
		if r.StartMS <= prevEnd {
			t.Errorf("range %d starts at %d, at or before the previous end %d; ranges are not coalesced and ascending: %+v",
				i, r.StartMS, prevEnd, cov.Ranges)
		}
		prevEnd = r.EndMS
	}
	summed := 0
	for _, r := range cov.Ranges {
		summed += r.EndMS - r.StartMS
	}
	if summed != cov.DecodedMS {
		t.Errorf("decoded_ms = %d, want the summed range length %d", cov.DecodedMS, summed)
	}
}

// TestTranscriptCoverage_Fraction pins the decoded-fraction rule of §8.6.13: the
// measured time wins, the window counts are the fallback when the duration probe
// failed, and a decode that was never windowed reports a full 1.
//
// Mutants killed: reporting the window-count ratio when a duration IS known (the
// 73-minute case would read 1/8 = 12.5% instead of the audio it really covered),
// and reporting 0 when the duration probe failed (every unprobeable recording
// would trip a floor it never measured).
func TestTranscriptCoverage_Fraction(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cov  *ingest.TranscriptCoverage
		want float64
	}{
		{"nil coverage asserts nothing, so it is whole", nil, 1},
		{
			"measured time wins over the window count",
			&ingest.TranscriptCoverage{WindowsAttempted: 8, WindowsDecoded: 1, DecodedMS: 600_000, DurationMS: 4_384_000},
			600_000.0 / 4_384_000.0,
		},
		{
			"an unprobeable recording falls back to the window count",
			&ingest.TranscriptCoverage{WindowsAttempted: 4, WindowsDecoded: 1, DecodedMS: 0, DurationMS: 0},
			0.25,
		},
		{
			"a full decode is 1",
			&ingest.TranscriptCoverage{WindowsAttempted: 4, WindowsDecoded: 4, DecodedMS: 1_800_000, DurationMS: 1_800_000},
			1,
		},
		{
			"an under-reporting duration probe is clamped, never over 1",
			&ingest.TranscriptCoverage{WindowsAttempted: 2, WindowsDecoded: 2, DecodedMS: 1_000, DurationMS: 500},
			1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cov.Fraction(); got != tc.want {
				t.Errorf("Fraction() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTranscriptCoverage_Complete pins the positive completeness statement
// §8.6.13 requires, so absence of the whole object keeps its §5.2 "no assertion"
// meaning rather than becoming ambiguous with a recorded partial.
func TestTranscriptCoverage_Complete(t *testing.T) {
	t.Parallel()
	if !(&ingest.TranscriptCoverage{WindowsAttempted: 4, WindowsDecoded: 4}).Complete() {
		t.Error("4/4 windows decoded must report complete")
	}
	if (&ingest.TranscriptCoverage{WindowsAttempted: 8, WindowsDecoded: 1}).Complete() {
		t.Error("1/8 windows decoded must NOT report complete")
	}
}

// TestWindowedSTT_PartialDecodeRecordsCoverage is the issue in miniature: a
// 30-minute recording is scheduled as four windows, the middle two fail, and the
// merged transcript is persisted. The representation MUST say what it covers.
//
// Mutant killed: dropping windowStats at the log line (the pre-fix behavior), in
// which case meta_json carries no coverage key at all and the partial transcript
// is indistinguishable from a complete one.
func TestWindowedSTT_PartialDecodeRecordsCoverage(t *testing.T) {
	t.Parallel()
	const totalMS = 30 * 60 * 1000
	content := make([]byte, 300_000)
	tr := &windowRecordingTranscriber{
		capBytes: 200_000,
		failAt: map[int]error{
			2: &model.ProviderError{Code: "WHISPER_FAILED", Message: "transcription request failed", Retryable: false},
			3: context.Canceled,
		},
	}
	h := newWindowSTTHarness(t, tr, totalMS, content)

	cov, present := onlyTranscriptCoverage(t, h, "talks/partial.m4a", content)
	if !present {
		t.Fatalf("no coverage recorded on a 2-of-4 windowed decode; the transcript claims to be whole: %s", h.store.reps[0].MetaJSON)
	}
	if cov.WindowsAttempted != 4 || cov.WindowsDecoded != 2 {
		t.Errorf("coverage = %d/%d windows, want 2/4", cov.WindowsDecoded, cov.WindowsAttempted)
	}
	if cov.DurationMS != totalMS {
		t.Errorf("duration_ms = %d, want %d", cov.DurationMS, totalMS)
	}
	if cov.DecodedMS >= totalMS || cov.DecodedMS <= 0 {
		t.Errorf("decoded_ms = %d, want a value strictly inside (0,%d): two of four windows decoded", cov.DecodedMS, totalMS)
	}
	// The decoded windows are the first and the last, so the gap in the middle is
	// the whole point: a consumer must be able to read it off the ranges.
	if len(cov.Ranges) != 2 {
		t.Fatalf("ranges = %+v, want two disjoint stretches with the failed middle between them", cov.Ranges)
	}
	if cov.Ranges[0].StartMS != 0 {
		t.Errorf("first range starts at %d, want 0", cov.Ranges[0].StartMS)
	}
	if last := cov.Ranges[len(cov.Ranges)-1]; last.EndMS != totalMS {
		t.Errorf("last range ends at %d, want the end of the recording %d", last.EndMS, totalMS)
	}
	assertRangesAscendingAndDisjoint(t, cov)

	// The operator-visible half of the `warn` default (§8.6.13): an incomplete
	// transcript is announced, not left for a later meta_json read to reveal.
	if logs := h.logs.String(); !strings.Contains(logs, "PARTIAL transcript") {
		t.Errorf("no partial-transcript warning logged:\n%s", logs)
	}
}

// TestWindowedSTT_FullDecodeRecordsCompleteCoverage pins the positive statement: a
// fully decoded multi-window transcript records coverage too, so absence keeps its
// §5.2 "no assertion" meaning instead of also meaning "complete".
//
// Mutant killed: skipping the coalescing step. Decode windows OVERLAP by design,
// so four raw ranges summed would report 1,830,000 ms of decoded audio in a
// 1,800,000 ms recording and four ranges where the recording is continuous.
func TestWindowedSTT_FullDecodeRecordsCompleteCoverage(t *testing.T) {
	t.Parallel()
	const totalMS = 30 * 60 * 1000
	content := make([]byte, 300_000)
	tr := &windowRecordingTranscriber{capBytes: 200_000}
	h := newWindowSTTHarness(t, tr, totalMS, content)

	cov, present := onlyTranscriptCoverage(t, h, "talks/full.m4a", content)
	if !present {
		t.Fatalf("a fully decoded multi-window transcript must still record coverage: %s", h.store.reps[0].MetaJSON)
	}
	if cov.WindowsAttempted != 4 || cov.WindowsDecoded != 4 {
		t.Errorf("coverage = %d/%d windows, want 4/4", cov.WindowsDecoded, cov.WindowsAttempted)
	}
	if len(cov.Ranges) != 1 {
		t.Fatalf("ranges = %+v, want ONE coalesced range: the overlapping windows tile a continuous recording", cov.Ranges)
	}
	if cov.Ranges[0].StartMS != 0 || cov.Ranges[0].EndMS != totalMS {
		t.Errorf("range = [%d,%d], want the whole recording [0,%d]", cov.Ranges[0].StartMS, cov.Ranges[0].EndMS, totalMS)
	}
	if cov.DecodedMS != totalMS {
		t.Errorf("decoded_ms = %d, want exactly the duration %d; overlapping windows were summed instead of coalesced", cov.DecodedMS, totalMS)
	}
	if logs := h.logs.String(); strings.Contains(logs, "PARTIAL transcript") {
		t.Errorf("a complete decode must not warn about a partial transcript:\n%s", logs)
	}
}

// TestWindowedSTT_SingleRequestRecordsNoCoverage pins the unchanged common case: a
// recording that fits in one request is NOT windowed, so it records no coverage
// and its meta_json is byte-for-byte what it always was.
//
// Mutant killed: recording coverage for every decode, which would turn the §5.2
// "absent means no assertion" rule into a claim about a decode that never had
// windows to fail.
func TestWindowedSTT_SingleRequestRecordsNoCoverage(t *testing.T) {
	t.Parallel()
	content := []byte("short-audio-bytes")
	tr := &windowRecordingTranscriber{capBytes: 50 * 1024 * 1024}
	h := newWindowSTTHarness(t, tr, 5*60*1000, content)

	_, present := onlyTranscriptCoverage(t, h, "talks/short.m4a", content)
	if present {
		t.Errorf("a single-request decode must record no coverage: %s", h.store.reps[0].MetaJSON)
	}
}

// TestWindowedSTT_CoverageSurvivesTheTranscriptCache pins the durability rule of
// §8.6.13. The transcript text is cached; a second run reads it back without
// touching the provider. If the coverage did not travel with the cached text, the
// very next run would re-index the same PARTIAL transcript as a complete one, so
// the fix would survive exactly one restart.
//
// Mutant killed: computing the coverage but never persisting or re-reading the
// cache sidecar.
func TestWindowedSTT_CoverageSurvivesTheTranscriptCache(t *testing.T) {
	t.Parallel()
	const totalMS = 30 * 60 * 1000
	content := make([]byte, 300_000)
	tr := &windowRecordingTranscriber{
		capBytes: 200_000,
		failAt: map[int]error{
			2: &model.ProviderError{Code: "WHISPER_FAILED", Message: "transcription request failed", Retryable: false},
			3: context.Canceled,
		},
	}
	h := newWindowSTTHarness(t, tr, totalMS, content)

	first, present := onlyTranscriptCoverage(t, h, "talks/cached.m4a", content)
	if !present {
		t.Fatalf("first run recorded no coverage: %s", h.store.reps[0].MetaJSON)
	}
	requestsAfterFirst := len(tr.requests())

	// A RESTART, not a second call on the same service. Reusing h.svc would pass
	// on in-memory state alone, which is not what "survives the cache" claims:
	// the next run is a new process reading the same state directory. So this
	// builds a fresh Service over the same cache with a fresh transcriber, and
	// the fresh transcriber is what makes "no provider request" mean the cache
	// answered rather than a warm field somewhere.
	restarted := &windowRecordingTranscriber{capBytes: tr.capBytes}
	svc2 := mustNewIngestService(t, config.Config{StateDir: h.stateDir}, h.store)
	svc2.SetTranscriber(restarted)
	svc2.SetLogger(log.New(h.logs, "", 0))
	svc2.ProbeDurationFunc = h.svc.ProbeDurationFunc
	svc2.ExtractSegmentFunc = h.svc.ExtractSegmentFunc

	if err := svc2.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/cached.m4a"), content); err != nil {
		t.Fatalf("second (cached) run failed: %v", err)
	}
	if got := len(restarted.requests()); got != 0 {
		t.Fatalf("the restarted run sent %d provider requests; it did not read the cache, so the test proves nothing", got)
	}
	if got := len(tr.requests()); got != requestsAfterFirst {
		t.Fatalf("the restarted run reached the first transcriber %d extra time(s)", got-requestsAfterFirst)
	}
	if len(h.store.reps) != 2 {
		t.Fatalf("persisted %d representations over two runs, want 2", len(h.store.reps))
	}
	second, present := transcriptCoverageFromMeta(t, h.store.reps[1].MetaJSON)
	if !present {
		t.Fatalf("the cached run dropped the coverage, so a restart re-indexes the partial transcript as complete: %s", h.store.reps[1].MetaJSON)
	}
	if second.WindowsDecoded != first.WindowsDecoded || second.WindowsAttempted != first.WindowsAttempted ||
		second.DecodedMS != first.DecodedMS || second.DurationMS != first.DurationMS {
		t.Errorf("cached coverage %+v does not match the decoded coverage %+v", second, first)
	}
	// The ranges are the part a consumer reads to know WHICH minutes exist, and
	// they are the part a scalar comparison cannot miss the loss of: a sidecar
	// that dropped them keeps every count intact and still cannot say what was
	// decoded.
	if len(second.Ranges) != len(first.Ranges) {
		t.Fatalf("cached run carried %d decoded range(s), the decode found %d: %+v vs %+v",
			len(second.Ranges), len(first.Ranges), second.Ranges, first.Ranges)
	}
	for i := range first.Ranges {
		if second.Ranges[i] != first.Ranges[i] {
			t.Errorf("decoded range %d survived the cache as %+v, want %+v",
				i, second.Ranges[i], first.Ranges[i])
		}
	}
}

// partialFloorService builds a media-ingesting service over a REAL store whose
// duration probe and segment extraction are deterministic stubs, so
// ProcessDocument drives a windowed decode end to end and exercises the durable
// status/skip_reason persistence the §8.6.13 floor depends on. Two of the four
// windows fail, giving a transcript that covers roughly a third of the recording.
func partialFloorService(t *testing.T, root string, st model.Store, minCoverage float64, action string) (*ingest.Service, *appstate.IndexingState, *syncBuffer) {
	t.Helper()
	const totalMS = 30 * 60 * 1000
	cfg := config.Config{RootDir: root, StateDir: testutil.TempDir(t), STTProvider: "off"}
	svc := mustNewIngestService(t, cfg, st)
	state := appstate.NewIndexingState(appstate.ModeIncremental)
	svc.SetIndexingState(state)
	logs := &syncBuffer{}
	svc.SetLogger(log.New(logs, "", 0))
	svc.SetTranscriber(&windowRecordingTranscriber{
		failAt: map[int]error{
			2: &model.ProviderError{Code: "WHISPER_FAILED", Message: "transcription request failed", Retryable: false},
			3: context.Canceled,
		},
	})
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) {
		return totalMS * time.Millisecond, nil
	}
	svc.ExtractSegmentFunc = func(_ context.Context, _ string, _, _ int) ([]byte, error) {
		return []byte("sliced-audio-bytes"), nil
	}
	svc.SetPartialTranscriptFloor(minCoverage, action)
	return svc, state, logs
}

// TestPartialTranscriptFloor_SkipRecordsDurableSkip is the strict half of §8.6.13:
// a transcript covering roughly a third of its recording is refused under
// media.stt.on_partial_transcript=skip, and the gap is recorded as a durable
// status="skipped" with skip_reason="transcript_partial". The corpus then reports
// what it does not hold, instead of answering "nothing found" from a transcript
// that never heard two thirds of the audio.
//
// Mutants killed: persisting the transcript anyway under skip, and recording the
// refusal as status="error" (which would retry the same refusal every run).
func TestPartialTranscriptFloor_SkipRecordsDurableSkip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "long.mp3"), "fake-audio")
	st := newRealStore(t)

	svc, state, logs := partialFloorService(t, root, st, 0.9, "skip")
	f := ingest.DiscoveredFile{RelPath: "long.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("ProcessDocument hard-failed on a non-fatal partial-transcript skip: %v", err)
	}

	doc := mustGetDoc(t, st, "long.mp3")
	if doc.Status != "skipped" {
		t.Fatalf("long.mp3: status = %q, want \"skipped\" (partial transcript under skip mode, §8.6.13)", doc.Status)
	}
	if doc.SkipReason != model.SkipReasonTranscriptPartial {
		t.Fatalf("long.mp3: skip_reason = %q, want %q", doc.SkipReason, model.SkipReasonTranscriptPartial)
	}
	if snap := state.Snapshot(); snap.Skipped != 1 {
		t.Errorf("long.mp3: run skipped = %d, want 1", snap.Skipped)
	}
	stats, err := st.CorpusStats(ctx)
	if err != nil {
		t.Fatalf("CorpusStats: %v", err)
	}
	if stats.SkipSummary == nil || stats.SkipSummary.Categories[model.SkipReasonTranscriptPartial] != 1 {
		t.Fatalf("expected SkipSummary category %q == 1, got %+v", model.SkipReasonTranscriptPartial, stats.SkipSummary)
	}
	if got := strings.Count(logs.String(), "media.stt.min_coverage"); got == 0 {
		t.Errorf("the refusal must name the knob that caused it:\n%s", logs.String())
	}
}

// TestPartialTranscriptFloor_WarnIndexesAnyway is the fail-open default: the SAME
// partially decoded recording under `warn` is indexed, because a partial
// transcript is useful as long as the index SAYS it is partial. This proves the
// skip branch is inert unless an operator asks for it.
func TestPartialTranscriptFloor_WarnIndexesAnyway(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "long.mp3"), "fake-audio")
	st := newRealStore(t)

	svc, _, logs := partialFloorService(t, root, st, 0.9, "warn")
	f := ingest.DiscoveredFile{RelPath: "long.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("ProcessDocument failed: %v", err)
	}

	doc := mustGetDoc(t, st, "long.mp3")
	if doc.Status == "skipped" {
		t.Fatalf("long.mp3: warn must not skip; status = %q, skip_reason = %q", doc.Status, doc.SkipReason)
	}
	if !strings.Contains(logs.String(), "PARTIAL transcript") {
		t.Errorf("warn must announce the partial transcript:\n%s", logs.String())
	}
}

// TestPartialTranscriptFloor_OffByDefault pins the shipped default: with
// media.stt.min_coverage at 0 the floor never trips, even under `skip` and even on
// a recording that decoded a third of its windows. Recording the coverage is
// mandatory; refusing on it is opt-in.
//
// Mutant killed: treating `min_coverage: 0` as "every window must decode", which
// would refuse a transcript on every corpus that upgrades into this feature.
func TestPartialTranscriptFloor_OffByDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "long.mp3"), "fake-audio")
	st := newRealStore(t)

	svc, _, _ := partialFloorService(t, root, st, 0, "skip")
	f := ingest.DiscoveredFile{RelPath: "long.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("ProcessDocument failed: %v", err)
	}

	doc := mustGetDoc(t, st, "long.mp3")
	if doc.Status == "skipped" {
		t.Fatalf("min_coverage=0 disables the floor; status = %q, skip_reason = %q", doc.Status, doc.SkipReason)
	}
}

// transcriptCachePath is the cache entry for content under the bytes-only key the
// harness writes (no STT identity is set), with the given suffix.
func transcriptCachePath(h *windowSTTHarness, content []byte, suffix string) string {
	return filepath.Join(h.stateDir, "cache", "transcribe", ingest.ComputeContentHash(content)+suffix)
}

// TestWindowedSTT_UnwritableCoverageWithholdsTheTextCache pins the publish order
// of §8.6.13: the coverage sidecar is written BEFORE the text it describes, and a
// sidecar that cannot be written withholds the text cache entirely.
//
// Mutant killed: writing the .txt first and treating the coverage write as
// best-effort. The transcript would then be cached WITHOUT its coverage, and the
// next run would read it back and index a partial transcript as a complete one:
// the same defect, one write failure away.
//
// The failure is injected by putting a directory where the sidecar file belongs,
// so the write fails deterministically without a fake filesystem.
func TestWindowedSTT_UnwritableCoverageWithholdsTheTextCache(t *testing.T) {
	t.Parallel()
	const totalMS = 30 * 60 * 1000
	content := make([]byte, 300_000)
	tr := &windowRecordingTranscriber{capBytes: 200_000}
	h := newWindowSTTHarness(t, tr, totalMS, content)

	blocked := transcriptCachePath(h, content, ".coverage.json")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatalf("stage the blocked coverage path: %v", err)
	}

	if err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/blocked.m4a"), content); err != nil {
		t.Fatalf("an unwritable coverage sidecar must not fail the document: %v", err)
	}
	// The transcript itself is still produced and indexed for THIS run.
	if len(h.store.reps) != 1 {
		t.Fatalf("persisted %d representations, want 1", len(h.store.reps))
	}
	if _, present := transcriptCoverageFromMeta(t, h.store.reps[0].MetaJSON); !present {
		t.Errorf("the in-memory coverage must still reach meta_json: %s", h.store.reps[0].MetaJSON)
	}
	// But the text cache is withheld, so the next run re-decodes rather than
	// reading a transcript that would silently claim to be whole.
	if _, err := os.Stat(transcriptCachePath(h, content, ".txt")); !os.IsNotExist(err) {
		t.Fatalf("the transcript text was cached without its coverage sidecar (stat err = %v)", err)
	}
	requestsAfterFirst := len(tr.requests())
	if err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/blocked.m4a"), content); err != nil {
		t.Fatalf("second run failed: %v", err)
	}
	if len(tr.requests()) == requestsAfterFirst {
		t.Error("the second run read the withheld cache instead of re-decoding")
	}
}

// TestWindowedSTT_SingleRequestClearsAStaleCoverageSidecar pins the other half of
// the cache rule. The cache key is the content bytes plus the STT identity, so a
// provider whose payload cap changed can decode the SAME media in one request
// where it previously used windows. A sidecar left by the windowed decode would
// then attach another decode's gaps to a complete transcript.
//
// Mutant killed: writing the sidecar only when coverage is non-nil, never
// removing it.
func TestWindowedSTT_SingleRequestClearsAStaleCoverageSidecar(t *testing.T) {
	t.Parallel()
	content := []byte("short-audio-bytes")
	tr := &windowRecordingTranscriber{capBytes: 50 * 1024 * 1024}
	h := newWindowSTTHarness(t, tr, 5*60*1000, content)

	stale := transcriptCachePath(h, content, ".coverage.json")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatalf("create cache dir: %v", err)
	}
	if err := os.WriteFile(stale, []byte(`{"windows_attempted":8,"windows_decoded":1,"decoded_ms":600000,"duration_ms":4384000}`), 0o644); err != nil {
		t.Fatalf("stage the stale sidecar: %v", err)
	}

	cov, present := onlyTranscriptCoverage(t, h, "talks/short.m4a", content)
	if present {
		t.Errorf("a single-request decode inherited a stale windowed coverage: %+v", cov)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale coverage sidecar survived a single-request decode (stat err = %v)", err)
	}
}

// silentWindowTranscriber decodes the FIRST window to empty text (a quiet stretch
// the provider heard and had nothing to transcribe) and fails every window after
// it. The merged transcript is therefore empty, but for a reason that is not
// silence: three quarters of the recording never reached the provider at all.
type silentWindowTranscriber struct{ calls int }

func (w *silentWindowTranscriber) Transcribe(ctx context.Context, relPath string, data []byte) (string, error) {
	res, err := w.TranscribeStructured(ctx, relPath, data)
	return res.Text, err
}

func (w *silentWindowTranscriber) TranscribeStructured(_ context.Context, _ string, _ []byte) (model.TranscriptResult, error) {
	w.calls++
	if w.calls == 1 {
		return model.TranscriptResult{}, nil
	}
	return model.TranscriptResult{}, &model.ProviderError{
		Code: "WHISPER_FAILED", Message: "transcription request failed", Retryable: false,
	}
}

// TestPartialTranscriptFloor_EmptyTranscriptFromAPartialDecodeIsNotSilence pins the
// case an empty-transcript early return used to swallow. When the one window that
// reached the provider was quiet and the rest failed, the merged transcript is
// empty, and recording that as ordinary silent media asserts the recording had
// nothing to say, over audio no window ever decoded.
//
// Mutant killed: returning "no transcript produced" for an empty transcript before
// the §8.6.13 floor is consulted, which leaves the document status="ok" with no
// transcript and no trace of the failed windows.
func TestPartialTranscriptFloor_EmptyTranscriptFromAPartialDecodeIsNotSilence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "quiet.mp3"), "fake-audio")
	st := newRealStore(t)

	svc, _, _ := partialFloorService(t, root, st, 0.9, "skip")
	svc.SetTranscriber(&silentWindowTranscriber{})

	f := ingest.DiscoveredFile{RelPath: "quiet.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("ProcessDocument hard-failed: %v", err)
	}

	doc := mustGetDoc(t, st, "quiet.mp3")
	if doc.SkipReason != model.SkipReasonTranscriptPartial {
		t.Fatalf("quiet.mp3: status = %q, skip_reason = %q, want a %q skip: an empty transcript from a 1-of-4 decode is not silence",
			doc.Status, doc.SkipReason, model.SkipReasonTranscriptPartial)
	}
}

// TestTranscriptCoverage_CompleteUsesTheMeasuredTime pins that Complete asks the
// measured question when a duration is known. A decode whose windows all came
// back but whose ranges stop short of the end is still a gap, and it is exactly
// the gap the window counts cannot see.
func TestTranscriptCoverage_CompleteUsesTheMeasuredTime(t *testing.T) {
	t.Parallel()
	short := &ingest.TranscriptCoverage{WindowsAttempted: 4, WindowsDecoded: 4, DecodedMS: 900_000, DurationMS: 1_800_000}
	if short.Complete() {
		t.Error("4/4 windows covering half the recording must not report complete")
	}
	whole := &ingest.TranscriptCoverage{WindowsAttempted: 4, WindowsDecoded: 4, DecodedMS: 1_800_000, DurationMS: 1_800_000}
	if !whole.Complete() {
		t.Error("4/4 windows covering the whole recording must report complete")
	}
}

// liveTranscriptReps counts the non-tombstoned transcript representations the
// store still holds for relPath: the ones a query can still reach.
func liveTranscriptReps(t *testing.T, st *store.SQLiteStore, relPath string) int {
	t.Helper()
	reps, err := st.ActiveRepresentations(context.Background(), relPath)
	if err != nil {
		t.Fatalf("ActiveRepresentations(%s): %v", relPath, err)
	}
	n := 0
	for _, rep := range reps {
		if rep.RepType == ingest.RepTypeTranscript || strings.HasPrefix(rep.RepType, ingest.RepTypeTranscript+"-") {
			n++
		}
	}
	return n
}

// TestPartialTranscriptFloor_SkipRetiresWhatAnEarlierRunIndexed pins the lifecycle
// rule of §8.6.13. The floor is evaluated on EVERY run, and the likely adoption
// path is "index the corpus with it off, discover the gap, turn it on".
//
// Mutant killed: recording status=skipped without retiring the representations an
// earlier run persisted from the same partial decode. The document would then
// report itself not indexed AND keep answering from the audio it never heard,
// which is worse than either half alone.
func TestPartialTranscriptFloor_SkipRetiresWhatAnEarlierRunIndexed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "long.mp3"), "fake-audio")
	st := newRealStore(t)
	f := ingest.DiscoveredFile{RelPath: "long.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}

	// Run 1: the floor is off, so the partial transcript is indexed and live.
	lenient, _, _ := partialFloorService(t, root, st, 0, "warn")
	if err := lenient.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("first (lenient) run: %v", err)
	}
	if n := liveTranscriptReps(t, st, "long.mp3"); n != 1 {
		t.Fatalf("first run left %d live transcript representation(s), want 1", n)
	}

	// Run 2: the operator turns the floor on and re-indexes.
	strict, _, _ := partialFloorService(t, root, st, 0.9, "skip")
	if err := strict.ProcessDocument(ctx, f, nil, true); err != nil {
		t.Fatalf("second (strict) run: %v", err)
	}

	doc := mustGetDoc(t, st, "long.mp3")
	if doc.SkipReason != model.SkipReasonTranscriptPartial {
		t.Fatalf("long.mp3: status = %q, skip_reason = %q, want %q", doc.Status, doc.SkipReason, model.SkipReasonTranscriptPartial)
	}
	if n := liveTranscriptReps(t, st, "long.mp3"); n != 0 {
		t.Fatalf("the refused document reports itself skipped but still holds %d live transcript representation(s); it would keep answering from audio it never heard", n)
	}
}

// TestPartialTranscriptFloor_SkipNeverRetiresASidecarTranscript pins the one thing
// a refusal must never touch. A sidecar transcript is authored, not model-derived
// (§8.6.4/§8.6.7), so a policy about the quality of MODEL output has no standing
// to delete an editor's work. A sidecar for track 0 is stored under a rep_type
// that shares the refused track's prefix, which is exactly how it would be swept
// up by a rep_type match alone.
//
// Mutant killed: matching purely on rep_type when deciding what a refusal retires.
func TestPartialTranscriptFloor_SkipNeverRetiresASidecarTranscript(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "long.mp3"), "fake-audio")
	st := newRealStore(t)

	// Seed an authored sidecar transcript under a rep_type in the refused track's
	// family, the way sidecar ingestion of a language-suffixed .vtt would.
	if err := st.UpsertDocument(ctx, model.Document{RelPath: "long.mp3", DocType: "audio", Status: "ok"}); err != nil {
		t.Fatalf("seed document: %v", err)
	}
	doc0 := mustGetDoc(t, st, "long.mp3")
	if _, err := st.UpsertRepresentation(ctx, model.Representation{
		DocID:       doc0.DocID,
		RepType:     ingest.RepTypeTranscript + "-ru",
		RepHash:     "sidecar-hash",
		MetaJSON:    `{"source":"sidecar","language":"ru","timestamps":true}`,
		CreatedUnix: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("seed sidecar transcript: %v", err)
	}

	// Not a forced reindex: --force deliberately retires stale sidecar transcripts
	// so STT can replace them (§8.6.4), which is a different rule from this one.
	svc, _, _ := partialFloorService(t, root, st, 0.9, "skip")
	f := ingest.DiscoveredFile{RelPath: "long.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}

	reps, err := st.ActiveRepresentations(ctx, "long.mp3")
	if err != nil {
		t.Fatalf("ActiveRepresentations: %v", err)
	}
	for _, rep := range reps {
		if rep.RepType == ingest.RepTypeTranscript+"-ru" {
			return // the authored sidecar survived the refusal, as it must
		}
	}
	t.Fatalf("the refusal retired the authored sidecar transcript; live reps: %+v", reps)
}

// retireFailingStore961 is a real store whose retirement always fails, which is
// what the SQLite store does under a locked database: it rolls the transaction
// back and every representation it was asked to tombstone stays active.
type retireFailingStore961 struct {
	*store.SQLiteStore
	err error
}

func (s *retireFailingStore961) SoftDeleteRepresentations(
	ctx context.Context, relPath string, repIDs []int64) (int, error) {
	return 0, s.err
}

// TestPartialFloor_ARefusalThatCannotRetireDoesNotRecordASkip pins the pair the
// §8.6.13 floor exists to prevent, reached from the other side.
//
// The refusal tombstones what an earlier run indexed. When that retirement
// fails, the store rolls it back and the earlier chunks are still live.
// Recording status="skipped" then describes a document that reports itself not
// indexed while it still answers from audio no window ever decoded, which is
// worse than either half alone. A failed retirement has to fail the document.
//
// Mutant killed: logging the retirement error and refusing anyway, which is what
// this PR did before review.
func TestPartialFloor_ARefusalThatCannotRetireDoesNotRecordASkip(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	base := newRealStore(t)
	st := &retireFailingStore961{SQLiteStore: base, err: errors.New("database is locked")}

	writeFile(t, filepath.Join(root, "long.mp3"), "fake-audio")
	f := ingest.DiscoveredFile{RelPath: "long.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	ctx := context.Background()

	// The adoption path the retirement exists for: index the corpus with the
	// floor off, discover the gap, then turn it on. Without the first run there
	// is nothing stored to retire, and the refusal would succeed trivially.
	warmed, _, _ := partialFloorService(t, root, base, 0, "warn")
	if err := warmed.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("the first run should index the partial transcript: %v", err)
	}
	reps, err := base.ActiveRepresentations(ctx, "long.mp3")
	if err != nil || len(reps) == 0 {
		t.Fatalf("the first run stored nothing to retire (%d reps, err=%v)", len(reps), err)
	}

	// force, because the document has not changed since the warm run: turning
	// the floor on is exactly the case where the content hash is identical and
	// the verdict is not.
	svc, _, _ := partialFloorService(t, root, st, 1.0, "skip")
	err = svc.ProcessDocument(ctx, f, nil, true)
	if err == nil {
		t.Fatal("a refusal whose retirement failed reported success, so the run records a skip over chunks that are still live")
	}
	if !strings.Contains(err.Error(), "database is locked") {
		t.Errorf("the error lost the store's own reason: %v", err)
	}
}

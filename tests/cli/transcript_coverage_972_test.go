package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"io"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// The SPEC §7.7 honest-coverage report for SPEECH (#972).
//
// #961 made a windowed decode record which part of a recording it produced text
// for, per representation. Nothing summed it, and the default is what makes that
// a gap: `media.stt.on_partial_transcript` defaults to `warn`, which PERSISTS the
// partial transcript and leaves its document `status=ok`. So the shortfall
// reaches no other report. `skip_reasons` sees only the `skip` path, the
// extraction verdict is scoped to format classes, and status says ok. A corpus
// can be 100% indexed, report no skips and no errors, and still be missing hours
// of speech.
//
// The measurement behind #961 is the shape used throughout: a 73-minute
// recording scheduled as 8 windows, of which 1 decoded.

const (
	minute = 60 * 1000
	// The #961 case: 73 minutes scheduled as 8 windows, 1 decoded.
	rfeDurationMS = 73 * minute
	rfeDecodedMS  = 10 * minute
)

// coverageMeta builds a transcript meta_json carrying a §8.6.13 coverage record.
// It also writes the TOP-LEVEL duration_ms that a real transcript carries, which
// is the media's length and not a decode result: an aggregate that summed the
// unqualified field would read the wrong denominator (dirstral-spec#107 review).
func coverageMeta(t *testing.T, provider, modelName string, attempted, decoded, decodedMS, durationMS int) string {
	t.Helper()
	meta := map[string]any{
		"provider":    provider,
		"model":       modelName,
		"timestamps":  true,
		"duration_ms": durationMS,
		"coverage": map[string]any{
			"windows_attempted": attempted,
			"windows_decoded":   decoded,
			"decoded_ms":        decodedMS,
			"duration_ms":       durationMS,
			"ranges":            []map[string]int{{"start_ms": 0, "end_ms": decodedMS}},
		},
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	return string(raw)
}

// plainMeta is a single-request decode: no coverage object at all. §5.2 absence
// is "no assertion", never "complete", so it must be counted as neither.
func plainMeta(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"provider": "whisper", "model": "large-v3", "duration_ms": 5 * minute,
	})
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	return string(raw)
}

type seedRep struct {
	relPath string
	status  string
	// repType defaults to "transcript". §8.6.12 gives an additional audio track
	// its own "transcript@t<N>".
	repType  string
	metaJSON string
	// deleted tombstones the REPRESENTATION, as a partial-transcript refusal does.
	deleted bool
}

func seedTranscripts(t *testing.T, dir string, reps ...seedRep) *store.SQLiteStore {
	t.Helper()
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(dir, ".dir2mcp", "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	for i, r := range reps {
		status := r.status
		if status == "" {
			status = "ok"
		}
		doc := model.Document{RelPath: r.relPath, DocType: "media", Status: status}
		if err := st.UpsertDocument(ctx, doc); err != nil {
			t.Fatalf("seed doc %s: %v", r.relPath, err)
		}
		stored, err := st.GetDocumentByPath(ctx, r.relPath)
		if err != nil {
			t.Fatalf("read back %s: %v", r.relPath, err)
		}
		repType := r.repType
		if repType == "" {
			repType = "transcript"
		}
		rep := model.Representation{
			DocID:    stored.DocID,
			RepType:  repType,
			RepHash:  fmt.Sprintf("hash-%d", i),
			MetaJSON: r.metaJSON,
			Deleted:  r.deleted,
		}
		if _, err := st.UpsertRepresentation(ctx, rep); err != nil {
			t.Fatalf("seed rep %s: %v", r.relPath, err)
		}
	}
	return st
}

// doctorCheckNamed runs doctor in dir and returns the named check.
func doctorCheckNamed(t *testing.T, dir, name string) (struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}, bool) {
	t.Helper()
	for _, c := range runDoctorReport(t, dir) {
		if c.Name == name {
			return c, true
		}
	}
	return struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	}{}, false
}

func TestPartialTranscriptCoverage_CountsOnlyWhatDoesNotStateCompleteness(t *testing.T) {
	dir := t.TempDir()
	st := seedTranscripts(t, dir,
		// The #961 case: 1 of 8 windows, and the document is `ok` because the
		// floor defaults to warn. This is the whole point of the report.
		seedRep{relPath: "rfe/interview.mp4",
			metaJSON: coverageMeta(t, "whisper", "large-v3", 8, 1, rfeDecodedMS, rfeDurationMS)},
		// Fully decoded multi-window: a POSITIVE statement of completeness.
		seedRep{relPath: "rfe/complete.mp4",
			metaJSON: coverageMeta(t, "whisper", "large-v3", 4, 4, 40*minute, 40*minute)},
		// Single request: records no coverage. Absence is no assertion.
		seedRep{relPath: "rfe/short.mp3", metaJSON: plainMeta(t)},
	)
	defer func() { _ = st.Close() }()

	got, err := st.PartialTranscriptCoverage(context.Background())
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if got.Transcripts != 1 {
		t.Errorf("transcripts = %d, want 1", got.Transcripts)
	}
	if got.DecodedMS != rfeDecodedMS || got.DurationMS != rfeDurationMS {
		t.Errorf("decoded/duration = %d/%d, want %d/%d",
			got.DecodedMS, got.DurationMS, rfeDecodedMS, rfeDurationMS)
	}
	if got.MissingMS() != rfeDurationMS-rfeDecodedMS {
		t.Errorf("missing = %d, want %d", got.MissingMS(), rfeDurationMS-rfeDecodedMS)
	}
	if want := []string{"whisper/large-v3"}; len(got.Providers) != 1 || got.Providers[0] != want[0] {
		t.Errorf("providers = %v, want %v", got.Providers, want)
	}
}

func TestPartialTranscriptCoverage_ReadsTheCoverageDurationNotTheMediaDuration(t *testing.T) {
	// A transcript meta_json carries BOTH a top-level duration_ms and
	// coverage.duration_ms. Here they differ, so an aggregate that read the
	// unqualified field would report a different shortfall.
	dir := t.TempDir()
	meta := `{"provider":"whisper","model":"large-v3","duration_ms":999999999,` +
		`"coverage":{"windows_attempted":8,"windows_decoded":1,` +
		`"decoded_ms":600000,"duration_ms":4380000}}`
	st := seedTranscripts(t, dir, seedRep{relPath: "rfe/a.mp4", metaJSON: meta})
	defer func() { _ = st.Close() }()

	got, err := st.PartialTranscriptCoverage(context.Background())
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if got.DurationMS != rfeDurationMS {
		t.Errorf("duration = %d, want coverage.duration_ms %d (not the media's)", got.DurationMS, rfeDurationMS)
	}
}

func TestPartialTranscriptCoverage_AnUnknownDurationIsReportedNotSummedAsZero(t *testing.T) {
	// §8.6.13: duration_ms is 0 when the probe failed. Summed as zero it would
	// report a shortfall of nothing, which is the silence §7.7 forbids.
	dir := t.TempDir()
	st := seedTranscripts(t, dir,
		seedRep{relPath: "rfe/known.mp4",
			metaJSON: coverageMeta(t, "whisper", "large-v3", 8, 1, rfeDecodedMS, rfeDurationMS)},
		// The probe failed (duration 0) but two windows DID decode, so
		// decoded_ms is non-zero. Folded into the length total it would claim
		// decoded audio against a denominator of nothing.
		seedRep{relPath: "rfe/unprobed.mp4",
			metaJSON: coverageMeta(t, "whisper", "large-v3", 8, 2, 20*minute, 0)},
	)
	defer func() { _ = st.Close() }()

	got, err := st.PartialTranscriptCoverage(context.Background())
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if got.Transcripts != 2 {
		t.Errorf("transcripts = %d, want 2 (the unprobed one still counts as a file)", got.Transcripts)
	}
	if got.UnknownDuration != 1 {
		t.Errorf("unknown = %d, want 1", got.UnknownDuration)
	}
	if got.DurationMS != rfeDurationMS || got.DecodedMS != rfeDecodedMS {
		t.Errorf("length total = %d/%d, want only the known one %d/%d",
			got.DecodedMS, got.DurationMS, rfeDecodedMS, rfeDurationMS)
	}
}

func TestPartialTranscriptCoverage_ARetiredTranscriptIsNotCounted(t *testing.T) {
	// A partial-transcript refusal (`on_partial_transcript: skip`) tombstones the
	// representation and its chunks. Counting it would report a shortfall for
	// audio that is no longer indexed at all; that case is skip_reasons'.
	dir := t.TempDir()
	st := seedTranscripts(t, dir, seedRep{
		relPath:  "rfe/refused.mp4",
		status:   "skipped",
		metaJSON: coverageMeta(t, "whisper", "large-v3", 8, 1, rfeDecodedMS, rfeDurationMS),
		deleted:  true,
	})
	defer func() { _ = st.Close() }()

	got, err := st.PartialTranscriptCoverage(context.Background())
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if got.Transcripts != 0 {
		t.Errorf("transcripts = %d, want 0 for a retired representation", got.Transcripts)
	}
}

func TestPartialTranscriptCoverage_EveryWindowBackAndTimeStillShortIsPartial(t *testing.T) {
	// The reading the window counts cannot see, and the reason dirstral-spec#107
	// was amended: completeness is the MEASURED question wherever it can be
	// asked. All 8 windows returned, and the decoded time still falls short.
	dir := t.TempDir()
	st := seedTranscripts(t, dir, seedRep{relPath: "rfe/gappy.mp4",
		metaJSON: coverageMeta(t, "whisper", "large-v3", 8, 8, 40*minute, rfeDurationMS)})
	defer func() { _ = st.Close() }()

	got, err := st.PartialTranscriptCoverage(context.Background())
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if got.Transcripts != 1 {
		t.Fatalf("transcripts = %d, want 1: the counts agree but the time does not", got.Transcripts)
	}
	if got.MissingMS() != 33*minute {
		t.Errorf("missing = %d, want %d", got.MissingMS(), 33*minute)
	}
}

func TestPartialTranscriptCoverage_AnUnreadableMetaAssertsNothing(t *testing.T) {
	dir := t.TempDir()
	st := seedTranscripts(t, dir,
		seedRep{relPath: "rfe/broken.mp4", metaJSON: `{"coverage": not json`},
		seedRep{relPath: "rfe/real.mp4",
			metaJSON: coverageMeta(t, "whisper", "large-v3", 8, 1, rfeDecodedMS, rfeDurationMS)},
	)
	defer func() { _ = st.Close() }()

	got, err := st.PartialTranscriptCoverage(context.Background())
	if err != nil {
		t.Fatalf("one unreadable row must not fail the report: %v", err)
	}
	if got.Transcripts != 1 {
		t.Errorf("transcripts = %d, want 1", got.Transcripts)
	}
}

func TestDoctorTranscriptCoverage_ReportsACleanCorpusPositively(t *testing.T) {
	// §7.7: an omitted line and a clean corpus read identically to the operator
	// deciding whether to trust a search result, so doctor states it.
	dir := t.TempDir()
	st := seedTranscripts(t, dir, seedRep{relPath: "rfe/complete.mp4",
		metaJSON: coverageMeta(t, "whisper", "large-v3", 4, 4, 40*minute, 40*minute)})
	_ = st.Close()

	check, ok := doctorCheckNamed(t, dir, "transcript_coverage")
	if !ok {
		t.Fatalf("doctor has no transcript_coverage check")
	}
	if check.Status != "ok" {
		t.Errorf("status = %q, want ok", check.Status)
	}
	if !strings.Contains(check.Detail, "no transcript records an incomplete decode") {
		t.Errorf("detail does not state the clean verdict: %q", check.Detail)
	}
}

func TestDoctorTranscriptCoverage_NamesTheShortfallAndAWorkingRemedy(t *testing.T) {
	dir := t.TempDir()
	st := seedTranscripts(t, dir, seedRep{relPath: "rfe/interview.mp4",
		metaJSON: coverageMeta(t, "whisper", "large-v3", 8, 1, rfeDecodedMS, rfeDurationMS)})
	_ = st.Close()

	check, ok := doctorCheckNamed(t, dir, "transcript_coverage")
	if !ok {
		t.Fatalf("doctor has no transcript_coverage check")
	}
	// A warning, not an error: the partial transcript IS indexed and its text is
	// useful. What was wrong is that the shortfall was silent.
	if check.Status != "warn" {
		t.Errorf("status = %q, want warn", check.Status)
	}
	for _, want := range []string{
		"1 transcript(s) record an incomplete decode",
		"1h 3m",              // never heard
		"whisper/large-v3",   // the identity the record holds
		"cache",              // the entries that have to go
		"re-decodes nothing", // and that a reindex alone will not do it
	} {
		if !strings.Contains(check.Detail, want) {
			t.Errorf("detail missing %q: %q", want, check.Detail)
		}
	}
	// The endpoint is NOT recorded, so the report must not claim to name one.
	if strings.Contains(check.Detail, "http://") || strings.Contains(check.Detail, "https://") {
		t.Errorf("detail names an endpoint the record does not hold: %q", check.Detail)
	}
}

func TestStartupTranscriptCoverage_IsSilentWhereNoBannerPrints(t *testing.T) {
	dir := t.TempDir()
	st := seedTranscripts(t, dir, seedRep{relPath: "rfe/interview.mp4",
		metaJSON: coverageMeta(t, "whisper", "large-v3", 8, 1, rfeDecodedMS, rfeDurationMS)})
	defer func() { _ = st.Close() }()

	cfg := config.Config{StateDir: filepath.Join(dir, ".dir2mcp")}
	app := cli.NewAppWithIO(io.Discard, io.Discard)
	var stderr strings.Builder

	if got := app.StartupTranscriptCoverageForTest(context.Background(), st, cfg, false, false, &stderr); got != 1 {
		t.Errorf("banner probe = %d, want 1", got)
	}
	if got := app.StartupTranscriptCoverageForTest(context.Background(), st, cfg, true, false, &stderr); got != 0 {
		t.Errorf("--json must print no banner and run no probe, got %d", got)
	}
	if got := app.StartupTranscriptCoverageForTest(context.Background(), st, cfg, false, true, &stderr); got != 0 {
		t.Errorf("--quiet must print no banner and run no probe, got %d", got)
	}
	if stderr.String() != "" {
		t.Errorf("unexpected warning: %q", stderr.String())
	}
}

func TestDoctorTranscriptCoverage_ASubSecondShortfallIsNotRenderedAsNothing(t *testing.T) {
	// "0s never heard" reads as nothing missing, which is the silence §7.7
	// forbids. Windows are minutes long, so this is the rounding edge rather
	// than a common case, and that is exactly why it must not round to zero.
	dir := t.TempDir()
	st := seedTranscripts(t, dir, seedRep{relPath: "rfe/nearly.mp4",
		metaJSON: coverageMeta(t, "whisper", "large-v3", 2, 1, 40*minute-400, 40*minute)})
	_ = st.Close()

	check, ok := doctorCheckNamed(t, dir, "transcript_coverage")
	if !ok {
		t.Fatalf("doctor has no transcript_coverage check")
	}
	if strings.Contains(check.Detail, "0s never heard") {
		t.Errorf("a real shortfall is reported as nothing: %q", check.Detail)
	}
	if !strings.Contains(check.Detail, "<1s never heard") {
		t.Errorf("detail does not name the sub-second shortfall: %q", check.Detail)
	}
}

func TestPartialTranscriptCoverage_SeesEveryAudioTrack(t *testing.T) {
	// §8.6.12 gives an additional audio track its own `transcript@t<N>` rep_type,
	// and each track is a separate decode with its own coverage. A predicate
	// matching only the bare `transcript` would call a multi-track recording
	// fully covered while every track past the first was missing most of its
	// speech, which on a multilingual archive is the common shape: the original
	// on track 0 and the interpreted feed on track 1.
	dir := t.TempDir()
	st := seedTranscripts(t, dir,
		seedRep{relPath: "rfe/dual.mp4", repType: "transcript",
			metaJSON: coverageMeta(t, "whisper", "large-v3", 4, 4, 40*minute, 40*minute)},
		seedRep{relPath: "rfe/dual.mp4", repType: "transcript@t1",
			metaJSON: coverageMeta(t, "whisper", "large-v3", 8, 1, rfeDecodedMS, rfeDurationMS)},
	)
	defer func() { _ = st.Close() }()

	got, err := st.PartialTranscriptCoverage(context.Background())
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if got.Transcripts != 1 {
		t.Fatalf("transcripts = %d, want 1: track 1 is partial and must be seen", got.Transcripts)
	}
	if got.MissingMS() != rfeDurationMS-rfeDecodedMS {
		t.Errorf("missing = %d, want %d", got.MissingMS(), rfeDurationMS-rfeDecodedMS)
	}
}

func TestPartialTranscriptCoverage_ATranslationIsNotASecondShortfall(t *testing.T) {
	// A translation derives from the source transcript's TEXT and records no
	// coverage of its own (§8.6.2). Counting it would report the same missing
	// audio twice for one recording.
	dir := t.TempDir()
	st := seedTranscripts(t, dir,
		seedRep{relPath: "rfe/ru.mp4", repType: "transcript",
			metaJSON: coverageMeta(t, "whisper", "large-v3", 8, 1, rfeDecodedMS, rfeDurationMS)},
		seedRep{relPath: "rfe/ru.mp4", repType: "transcript-en",
			metaJSON: `{"provider":"whisper","model":"large-v3","source_language":"ru",` +
				`"translate_provider":"openai","translate_model":"gpt-4o","language":"en"}`},
	)
	defer func() { _ = st.Close() }()

	got, err := st.PartialTranscriptCoverage(context.Background())
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if got.Transcripts != 1 {
		t.Errorf("transcripts = %d, want 1: the translation carries no coverage of its own", got.Transcripts)
	}
	if got.DurationMS != rfeDurationMS {
		t.Errorf("duration = %d, want %d (counted once)", got.DurationMS, rfeDurationMS)
	}
}

// #977. A corpus that asserts nothing read exactly like a clean one: both got
// "no transcript records an incomplete decode". Every corpus indexed before
// §8.6.13 existed is in that state, and §5.2 makes an absent field "no
// assertion", never a positive value.

func TestTranscriptCoverage_ACorpusThatAssertsNothingSaysSo(t *testing.T) {
	// The real RFE shape: decoded transcripts, not one coverage record between
	// them, because they were indexed before the record existed.
	dir := t.TempDir()
	st := seedTranscripts(t, dir,
		seedRep{relPath: "rfe/a.mp4", metaJSON: `{"source":"stt","provider":"whisper","model":"large-v3"}`},
		seedRep{relPath: "rfe/b.mp4", metaJSON: `{"source":"stt","provider":"whisper","model":"large-v3"}`},
	)
	_ = st.Close()

	check, ok := doctorCheckNamed(t, dir, "transcript_coverage")
	if !ok {
		t.Fatalf("doctor has no transcript_coverage check")
	}
	// Still ok: an unknown is not a known defect, and a corpus of short
	// single-request decodes is genuinely fine.
	if check.Status != "ok" {
		t.Errorf("status = %q, want ok: a no-assertion count is not a defect", check.Status)
	}
	if !strings.Contains(check.Detail, "2 transcript(s) assert nothing about coverage") {
		t.Errorf("the silent population is not named: %q", check.Detail)
	}
}

func TestTranscriptCoverage_ACleanCorpusStillReadsClean(t *testing.T) {
	// The clause must not appear when every transcript asserts, or it becomes
	// the noise it was added to remove.
	dir := t.TempDir()
	st := seedTranscripts(t, dir, seedRep{relPath: "rfe/complete.mp4",
		metaJSON: coverageMeta(t, "whisper", "large-v3", 4, 4, 40*minute, 40*minute)})
	_ = st.Close()

	check, _ := doctorCheckNamed(t, dir, "transcript_coverage")
	if strings.Contains(check.Detail, "assert nothing") {
		t.Errorf("a fully asserting corpus got the clause: %q", check.Detail)
	}
	if check.Detail != "no transcript records an incomplete decode" {
		t.Errorf("detail = %q, want the bare clean verdict", check.Detail)
	}
}

func TestTranscriptCoverage_TheClauseRidesAlongsideAKnownShortfall(t *testing.T) {
	// The two populations are independent: a corpus can hold a known partial
	// AND transcripts that say nothing, and the report must not drop either.
	dir := t.TempDir()
	st := seedTranscripts(t, dir,
		seedRep{relPath: "rfe/partial.mp4",
			metaJSON: coverageMeta(t, "whisper", "large-v3", 8, 1, rfeDecodedMS, rfeDurationMS)},
		seedRep{relPath: "rfe/silent.mp4", metaJSON: `{"source":"stt","provider":"whisper","model":"large-v3"}`},
	)
	_ = st.Close()

	check, _ := doctorCheckNamed(t, dir, "transcript_coverage")
	if check.Status != "warn" {
		t.Errorf("status = %q, want warn: there is a known shortfall", check.Status)
	}
	if !strings.Contains(check.Detail, "1 transcript(s) record an incomplete decode") {
		t.Errorf("the known shortfall is missing: %q", check.Detail)
	}
	if !strings.Contains(check.Detail, "1 transcript(s) assert nothing") {
		t.Errorf("the silent one is missing: %q", check.Detail)
	}
}

func TestPartialTranscriptCoverage_SilenceThatMeansNothingIsNotCounted(t *testing.T) {
	// A sidecar is AUTHORED, not decoded, and a translation derives from another
	// transcript's text. Neither could ever carry coverage, so counting their
	// silence would inflate the number with rows whose absence says nothing
	// about any decode.
	dir := t.TempDir()
	st := seedTranscripts(t, dir,
		seedRep{relPath: "rfe/decoded.mp4",
			metaJSON: `{"source":"stt","provider":"whisper","model":"large-v3"}`},
		seedRep{relPath: "rfe/authored.mp4", repType: "transcript",
			metaJSON: `{"source":"sidecar","language":"ru"}`},
		seedRep{relPath: "rfe/decoded.mp4", repType: "transcript-en",
			metaJSON: `{"source":"stt","provider":"whisper","model":"large-v3",` +
				`"translate_provider":"openai","translate_model":"gpt-4o"}`},
	)
	defer func() { _ = st.Close() }()

	got, err := st.PartialTranscriptCoverage(context.Background())
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if got.NoAssertion != 1 {
		t.Errorf("no-assertion = %d, want 1 (only the decoded one)", got.NoAssertion)
	}
}

func TestPartialTranscriptCoverage_TheWordCoverageInSomeOtherFieldIsNotACoverageRecord(t *testing.T) {
	// `track_label` carries the container's track title (§8.6.12), and
	// "coverage" is an ordinary broadcast word. Matching the quoted word rather
	// than the KEY would read such a transcript as carrying a coverage record
	// and drop it from the no-assertion count — under-reporting the silent
	// population, which is the one thing this count exists to get right.
	dir := t.TempDir()
	st := seedTranscripts(t, dir,
		seedRep{relPath: "rfe/live.mp4",
			metaJSON: `{"source":"stt","provider":"whisper","model":"large-v3",` +
				`"track_label":"Live coverage","language":"ru"}`},
		// Same trap on the other two exclusions.
		seedRep{relPath: "rfe/note.mp4",
			metaJSON: `{"source":"stt","provider":"whisper","model":"large-v3",` +
				`"track_label":"translate_provider"}`},
	)
	defer func() { _ = st.Close() }()

	got, err := st.PartialTranscriptCoverage(context.Background())
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if got.NoAssertion != 2 {
		t.Errorf("no-assertion = %d, want 2: neither transcript carries a coverage record", got.NoAssertion)
	}
}

func TestPartialTranscriptCoverage_WhitespaceAroundTheKeyIsStillACoverageRecord(t *testing.T) {
	// A LIKE on `"coverage":` encodes an assumption about the writer's
	// formatting. JSON permits a space before the colon, and a document written
	// that way asserts coverage exactly as much as one written without it.
	// Reading it as "no record" would under-report the very number this count
	// exists to make honest.
	dir := t.TempDir()
	st := seedTranscripts(t, dir, seedRep{relPath: "rfe/spaced.mp4",
		metaJSON: `{"source":"stt","provider":"whisper","model":"large-v3",` +
			`"coverage" : {"windows_attempted":4,"windows_decoded":4,` +
			`"decoded_ms":2400000,"duration_ms":2400000}}`})
	defer func() { _ = st.Close() }()

	got, err := st.PartialTranscriptCoverage(context.Background())
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if got.NoAssertion != 0 {
		t.Errorf("no-assertion = %d, want 0: the record is there, just spaced", got.NoAssertion)
	}
	if got.Transcripts != 0 {
		t.Errorf("transcripts = %d, want 0: it states completeness", got.Transcripts)
	}
}

func TestPartialTranscriptCoverage_AnUnreadableMetaIsCountedAsAssertingNothing(t *testing.T) {
	// json_extract raises on a document it cannot parse, so each test is guarded
	// by json_valid. A row that fails the guard is counted: it certainly carries
	// no coverage record, and it cannot be shown to be a sidecar or translation
	// either, so dropping it would be the same silence this count removes.
	dir := t.TempDir()
	st := seedTranscripts(t, dir,
		seedRep{relPath: "rfe/broken.mp4", metaJSON: `{"coverage": not json`},
		seedRep{relPath: "rfe/empty.mp4", metaJSON: ``},
	)
	defer func() { _ = st.Close() }()

	got, err := st.PartialTranscriptCoverage(context.Background())
	if err != nil {
		t.Fatalf("one unreadable row must not fail the report: %v", err)
	}
	if got.NoAssertion != 2 {
		t.Errorf("no-assertion = %d, want 2", got.NoAssertion)
	}
}

func TestStartupBanner_ANoAssertionOnlyCorpusPrintsNoSpeechSection(t *testing.T) {
	// Pins what the README now states. The banner is keyed on Partial, so a
	// corpus whose transcripts merely assert nothing produces no section, and
	// only `doctor` names that population.
	dir := t.TempDir()
	st := seedTranscripts(t, dir, seedRep{relPath: "rfe/silent.mp4",
		metaJSON: `{"source":"stt","provider":"whisper","model":"large-v3"}`})
	defer func() { _ = st.Close() }()

	cfg := config.Config{StateDir: filepath.Join(dir, ".dir2mcp")}
	app := cli.NewAppWithIO(io.Discard, io.Discard)
	var stderr strings.Builder
	// The probe finds no PARTIAL transcript, which is what the banner renders on.
	if got := app.StartupTranscriptCoverageForTest(context.Background(), st, cfg, false, false, &stderr); got != 0 {
		t.Errorf("banner probe = %d, want 0 partial transcripts", got)
	}
	// doctor still names them.
	_ = st.Close()
	check, _ := doctorCheckNamed(t, dir, "transcript_coverage")
	if !strings.Contains(check.Detail, "assert nothing about coverage") {
		t.Errorf("doctor must still name them: %q", check.Detail)
	}
}

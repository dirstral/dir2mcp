package tests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/avutil"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/store"
)

// perTrackTranscriber returns a transcript keyed by the exact bytes it is handed,
// so a per-track test (SPEC §8.6.12) can assert that each selected audio track is
// transcribed independently: track 0 receives the document's raw bytes, and each
// additional track N receives its extracted per-track audio bytes. An entry in
// failFor returns an error for that input (a track-scoped transcription failure);
// an unknown input transcribes to the empty string (a legitimately empty track).
type perTrackTranscriber struct {
	byInput map[string]string
	failFor map[string]error
	calls   int
}

func (p *perTrackTranscriber) Transcribe(_ context.Context, _ string, data []byte) (string, error) {
	p.calls++
	key := string(data)
	if err := p.failFor[key]; err != nil {
		return "", err
	}
	return p.byInput[key], nil
}

// trackAudioBytes is the deterministic per-track audio the stubbed extractor emits
// for an additional track N (N >= 1). Track 0 is transcribed from the document's
// raw bytes, never through the extractor.
func trackAudioBytes(audioIndex int) string {
	return fmt.Sprintf("track-%d-audio-bytes", audioIndex)
}

// threeTrackProbe injects a three-audio-stream census (an original, a per-language
// dub, and a music-&-effects track) without ffprobe.
func threeTrackProbe() func(context.Context, string) (avutil.MediaInfo, error) {
	return func(context.Context, string) (avutil.MediaInfo, error) {
		return avutil.MediaInfo{
			Container:  "matroska",
			AudioCodec: "aac",
			AudioStreams: []avutil.AudioStream{
				{Index: 0, CodecName: "aac", Channels: 2, Language: "eng", Title: "Original"},
				{Index: 1, CodecName: "aac", Channels: 2, Language: "rus", Title: "Dub"},
				{Index: 2, CodecName: "aac", Channels: 6, Title: "Music & Effects"},
			},
		}, nil
	}
}

// newPerTrackService wires an ingest Service with the injected probe/extractor
// seams so a multi-track selection is exercised hermetically (no ffprobe/ffmpeg).
func newPerTrackService(t *testing.T, root, stateDir string, st *store.SQLiteStore, tracks []string, tr *perTrackTranscriber) *ingest.Service {
	t.Helper()
	svc := mustNewIngestService(t, config.Config{
		RootDir:        root,
		StateDir:       stateDir,
		STTProvider:    "off",
		MediaSTTTracks: tracks,
	}, st)
	svc.SetTranscriber(tr)
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	svc.SetTranscriptLanguage("en")
	svc.ProbeMediaInfoFunc = threeTrackProbe()
	svc.ExtractAudioTrackIndexFunc = func(_ context.Context, _ string, audioIndex int) ([]byte, error) {
		return []byte(trackAudioBytes(audioIndex)), nil
	}
	return svc
}

func ingestPerTrack(t *testing.T, svc *ingest.Service, st *store.SQLiteStore, name, body string) map[string]bool {
	t.Helper()
	f := ingest.DiscoveredFile{RelPath: name, SizeBytes: int64(len(body)), MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument(%s): %v", name, err)
	}
	return repTypesFor(t, st, name)
}

// TestPerTrack_FirstOnlyTrackZeroBareKey pins the default: `media.stt.tracks` unset
// (⇒ first) transcribes ONLY track 0 under the BARE `transcript` rep_type, and the
// extractor is never invoked for additional tracks — byte-for-byte today's behavior
// and cost (SPEC §8.6.12).
func TestPerTrack_FirstOnlyTrackZeroBareKey(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	body := "audio-file-bytes"
	writeFile(t, filepath.Join(root, "talk.mp3"), body)
	st := newRealStore(t)

	tr := &perTrackTranscriber{byInput: map[string]string{body: "[00:00] first track"}}
	// nil tracks ⇒ default first.
	svc := newPerTrackService(t, root, t.TempDir(), st, nil, tr)
	extractorCalls := 0
	svc.ExtractAudioTrackIndexFunc = func(_ context.Context, _ string, audioIndex int) ([]byte, error) {
		extractorCalls++
		return []byte(trackAudioBytes(audioIndex)), nil
	}

	types := ingestPerTrack(t, svc, st, "talk.mp3", body)
	if !types["transcript"] {
		t.Fatalf("first mode: bare transcript missing; got %v", types)
	}
	if types["transcript@t1"] || types["transcript@t2"] {
		t.Fatalf("first mode transcribed additional tracks; got %v", types)
	}
	if extractorCalls != 0 {
		t.Fatalf("first mode invoked the per-track extractor %d times, want 0 (no extra cost)", extractorCalls)
	}
}

// TestPerTrack_AllTracksDistinctKeys verifies `all` transcribes every audio stream,
// keying track 0 as the bare `transcript` and each additional track N as
// `transcript@t<N>` with its container-declared track/language/label in meta_json
// (SPEC §8.6.12).
func TestPerTrack_AllTracksDistinctKeys(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	body := "audio-file-bytes"
	writeFile(t, filepath.Join(root, "multi.m4a"), body)
	st := newRealStore(t)

	tr := &perTrackTranscriber{byInput: map[string]string{
		body:               "[00:00] original english",
		trackAudioBytes(1): "[00:00] dubbed russian",
		trackAudioBytes(2): "[00:00] score and effects",
	}}
	svc := newPerTrackService(t, root, t.TempDir(), st, []string{"all"}, tr)

	types := ingestPerTrack(t, svc, st, "multi.m4a", body)
	for _, want := range []string{"transcript", "transcript@t1", "transcript@t2"} {
		if !types[want] {
			t.Fatalf("all mode missing rep_type %q; got %v", want, types)
		}
	}
	if tr.calls != 3 {
		t.Errorf("all mode STT calls = %d, want 3 (one per audio track)", tr.calls)
	}

	// Track 0 meta must NOT carry a `track` field (absence ⇒ track 0), keeping a
	// single-track transcript byte-for-byte unchanged.
	if _, ok := metaField(t, st, "multi.m4a", "transcript", "track"); ok {
		t.Error("track 0 transcript meta_json carries a `track` field; want absent")
	}
	// Track 1 meta records track=1 + declared language/label.
	if got, _ := metaField(t, st, "multi.m4a", "transcript@t1", "track"); got != float64(1) {
		t.Errorf("transcript@t1 track = %v, want 1", got)
	}
	if got, _ := metaField(t, st, "multi.m4a", "transcript@t1", "track_language"); got != "rus" {
		t.Errorf("transcript@t1 track_language = %v, want rus", got)
	}
	if got, _ := metaField(t, st, "multi.m4a", "transcript@t1", "track_label"); got != "Dub" {
		t.Errorf("transcript@t1 track_label = %v, want Dub", got)
	}
}

// TestPerTrack_ExplicitListSelectsSubset verifies an explicit `[0, 2]` selection
// transcribes exactly tracks 0 and 2 (skipping track 1), in container order.
func TestPerTrack_ExplicitListSelectsSubset(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	body := "audio-file-bytes"
	writeFile(t, filepath.Join(root, "sel.m4a"), body)
	st := newRealStore(t)

	tr := &perTrackTranscriber{byInput: map[string]string{
		body:               "[00:00] original english",
		trackAudioBytes(2): "[00:00] score and effects",
	}}
	// Written out of order to prove the set is processed in stream order, not list order.
	svc := newPerTrackService(t, root, t.TempDir(), st, []string{"2", "0"}, tr)

	types := ingestPerTrack(t, svc, st, "sel.m4a", body)
	if !types["transcript"] || !types["transcript@t2"] {
		t.Fatalf("explicit [0,2] missing expected reps; got %v", types)
	}
	if types["transcript@t1"] {
		t.Fatalf("explicit [0,2] transcribed unselected track 1; got %v", types)
	}
}

// TestPerTrack_OneTrackFailsOthersSucceed pins the track-scoped failure rule: when
// one selected track fails transcription but a sibling succeeds, only the failing
// track's representation is dropped and the DOCUMENT stays ready (SPEC §8.6.12).
func TestPerTrack_OneTrackFailsOthersSucceed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	body := "audio-file-bytes"
	writeFile(t, filepath.Join(root, "partial.m4a"), body)
	st := newRealStore(t)

	tr := &perTrackTranscriber{
		byInput: map[string]string{body: "[00:00] the original survives"},
		failFor: map[string]error{trackAudioBytes(1): errors.New("provider 503")},
	}
	svc := newPerTrackService(t, root, t.TempDir(), st, []string{"0", "1"}, tr)

	types := ingestPerTrack(t, svc, st, "partial.m4a", body)
	if !types["transcript"] {
		t.Fatalf("surviving track 0 transcript missing; got %v", types)
	}
	if types["transcript@t1"] {
		t.Fatalf("failed track 1 left a representation; want it dropped. got %v", types)
	}
	doc, err := st.GetDocumentByPath(context.Background(), "partial.m4a")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.Status != "ok" {
		t.Fatalf("document status = %q, want ok (a partial-track failure must not error the document)", doc.Status)
	}
}

// TestPerTrack_AllTracksFailDocumentError pins the other half of the rule: when
// EVERY selected track fails, the document is marked status=error (SPEC §8.6.12,
// the zero-successful-tracks case of §8.6.7).
func TestPerTrack_AllTracksFailDocumentError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	body := "audio-file-bytes"
	writeFile(t, filepath.Join(root, "dead.m4a"), body)
	st := newRealStore(t)

	tr := &perTrackTranscriber{
		failFor: map[string]error{
			body:               errors.New("provider 503 track 0"),
			trackAudioBytes(1): errors.New("provider 503 track 1"),
		},
	}
	svc := newPerTrackService(t, root, t.TempDir(), st, []string{"0", "1"}, tr)

	_ = ingestPerTrack(t, svc, st, "dead.m4a", body)
	doc, err := st.GetDocumentByPath(context.Background(), "dead.m4a")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.Status != "error" {
		t.Fatalf("document status = %q, want error (every selected track failed)", doc.Status)
	}
}

// TestPerTrack_ExplicitOutOfRangeProducesNothing pins that an explicit selection
// whose indices are all past a file's track count transcribes NOTHING for that
// file — it must not silently fall back to the unselected first track (SPEC
// §8.6.12: an out-of-range index is a per-file, track-scoped skip).
func TestPerTrack_ExplicitOutOfRangeProducesNothing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	body := "audio-file-bytes"
	writeFile(t, filepath.Join(root, "toohigh.m4a"), body)
	st := newRealStore(t)

	// Track 0 would transcribe if wrongly selected; assert it is NOT.
	tr := &perTrackTranscriber{byInput: map[string]string{body: "[00:00] track zero must not appear"}}
	svc := newPerTrackService(t, root, t.TempDir(), st, []string{"5"}, tr) // probe has only 3 tracks

	types := ingestPerTrack(t, svc, st, "toohigh.m4a", body)
	if types["transcript"] || types["transcript@t5"] {
		t.Fatalf("out-of-range selection produced a transcript; want none. got %v", types)
	}
}

// metaField unmarshals the meta_json of the given rep_type for relPath and returns
// the named field's value (and whether it was present). A missing representation is
// a test failure; a missing field returns (nil, false).
func metaField(t *testing.T, st *store.SQLiteStore, relPath, repType, field string) (any, bool) {
	t.Helper()
	raw, err := st.RepresentationMetaByType(context.Background(), relPath, repType)
	if err != nil {
		t.Fatalf("RepresentationMetaByType(%s, %s): %v", relPath, repType, err)
	}
	if raw == "" {
		t.Fatalf("no %q representation for %s", repType, relPath)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("meta_json for %q not valid json (%q): %v", repType, raw, err)
	}
	v, ok := parsed[field]
	return v, ok
}

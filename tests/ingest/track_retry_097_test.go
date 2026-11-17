package tests

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC 0.97.0 §8.6.13, §7.6: a selected audio track that failed with a
// transport or provider error, in a document that stays ok through another
// track, is retried on the next run. The document is processed again with an
// unchanged content_hash until the track produces a transcript or a no-speech
// result. The good tracks are read from the stored results. A track that the
// quality gate refused is not retried this way.

const (
	retryTrackPath = "retry/dub.m4a"
	retryTrackBody = "two-track-audio-bytes"
	retryTrack0    = "[00:00] the original english track"
	retryTrack1    = "[00:00] the dubbed track arrives later"
	retryMTimeUnix = int64(1_700_000_000)
)

// countingTrackTranscriber is a fake STT upstream that counts each request by
// the input it received. failLeft holds, for an input, how many more requests
// fail with a provider error; a negative value fails every request.
type countingTrackTranscriber struct {
	mu       sync.Mutex
	byInput  map[string]string
	failLeft map[string]int
	requests []string
}

func (c *countingTrackTranscriber) Transcribe(_ context.Context, _ string, data []byte) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	in := string(data)
	c.requests = append(c.requests, in)
	if n, ok := c.failLeft[in]; ok && n != 0 {
		if n > 0 {
			c.failLeft[in] = n - 1
		}
		return "", errors.New("provider 503")
	}
	return c.byInput[in], nil
}

// take returns the requests since the last call and clears them.
func (c *countingTrackTranscriber) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.requests
	c.requests = nil
	return out
}

type trackRetryHarness struct {
	svc    *ingest.Service
	st     *store.SQLiteStore
	state  *appstate.IndexingState
	tr     *countingTrackTranscriber
	root   string
	state0 string
}

// newTrackRetryHarness wires a two-track selection (tracks 0 and 1) over the
// three-track probe of the #567 tests, with a fixed size and mtime so the
// media fast path is live on every scan after the first.
func newTrackRetryHarness(t *testing.T, tr *countingTrackTranscriber, gate bool) *trackRetryHarness {
	t.Helper()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, filepath.FromSlash(retryTrackPath)), retryTrackBody)
	st := newRealStore(t)
	svc := mustNewIngestService(t, config.Config{
		RootDir:             root,
		StateDir:            stateDir,
		STTProvider:         "off",
		MediaSTTTracks:      []string{"0", "1"},
		QualityGatesEnabled: gate,
	}, st)
	svc.SetTranscriber(tr)
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	svc.SetTranscriptLanguage("en")
	svc.ProbeMediaInfoFunc = threeTrackProbe()
	svc.ExtractAudioTrackIndexFunc = func(_ context.Context, _ string, audioIndex int) ([]byte, error) {
		return []byte(trackAudioBytes(audioIndex)), nil
	}
	state := appstate.NewIndexingState(appstate.ModeIncremental)
	svc.SetIndexingState(state)
	return &trackRetryHarness{svc: svc, st: st, state: state, tr: tr, root: root, state0: stateDir}
}

// scan processes the unchanged file once and returns the STT requests it sent.
func (h *trackRetryHarness) scan(t *testing.T) []string {
	t.Helper()
	f := ingest.DiscoveredFile{RelPath: retryTrackPath, SizeBytes: int64(len(retryTrackBody)), MTimeUnix: retryMTimeUnix}
	if err := h.svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	return h.tr.take()
}

// assertOKWithTrack0 checks that the document is ok and searchable through the
// track 0 transcript, that it is not counted as an error, and whether the done
// marker is stamped.
func (h *trackRetryHarness) assertOKWithTrack0(t *testing.T, when string, wantMarker bool) {
	t.Helper()
	doc := mustGetDoc(t, h.st, retryTrackPath)
	if doc.Status != "ok" || doc.ErrorMessage != "" {
		t.Fatalf("%s: status=%q error=%q, want ok with no error", when, doc.Status, doc.ErrorMessage)
	}
	if got := strings.TrimSpace(doc.ContentHash) != ""; got != wantMarker {
		t.Fatalf("%s: done marker stamped=%v (content_hash=%q), want %v", when, got, doc.ContentHash, wantMarker)
	}
	if types := repTypesFor(t, h.st, retryTrackPath); !types["transcript"] {
		t.Fatalf("%s: the track 0 transcript must stay live, got %v", when, types)
	}
	if snap := h.state.Snapshot(); snap.Errors != 0 {
		t.Fatalf("%s: run errors=%d, want 0: a retried track is not a document error", when, snap.Errors)
	}
	absent, err := h.st.NoSpeechTranscriptAbsent(context.Background(), retryTrackPath)
	if err != nil || absent {
		t.Fatalf("%s: NoSpeechTranscriptAbsent=%v err=%v, want false", when, absent, err)
	}
}

// assertOpenFileServesTrack0 checks that open_file serves the track 0
// transcript, not MEDIA_NO_TEXT and not OCR_NOT_READY.
func (h *trackRetryHarness) assertOpenFileServesTrack0(t *testing.T, when string) {
	t.Helper()
	ret := retrieval.NewService(h.st, nil, nil, nil)
	ret.SetRootDir(h.root)
	ret.SetStateDir(h.state0)
	ret.SetDerivationCacheIdentities(h.svc.ActiveOCRIdentity(), h.svc.ActiveTranscriptIdentity())
	text, err := ret.OpenFile(context.Background(), retryTrackPath, model.Span{}, 4000)
	if err != nil {
		t.Fatalf("%s: open_file: %v", when, err)
	}
	if !strings.Contains(text, "original english track") {
		t.Fatalf("%s: open_file must serve the track 0 transcript, got %q", when, text)
	}
}

func assertRequests(t *testing.T, when string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: STT requests %q, want %q", when, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: STT requests %q, want %q", when, got, want)
		}
	}
}

// TestTrackRetry_ProviderErrorIsRetriedUntilTheTrackSucceeds covers a track
// that fails once and then succeeds. Scan 1 keeps the document ok with the
// track 0 transcript and leaves the done marker unstamped. Scan 2 decodes only
// track 1, reads track 0 from the cache and stamps the marker. Scan 3 sends no
// request.
func TestTrackRetry_ProviderErrorIsRetriedUntilTheTrackSucceeds(t *testing.T) {
	t.Parallel()
	tr := &countingTrackTranscriber{
		byInput:  map[string]string{retryTrackBody: retryTrack0, trackAudioBytes(1): retryTrack1},
		failLeft: map[string]int{trackAudioBytes(1): 1},
	}
	h := newTrackRetryHarness(t, tr, false)

	assertRequests(t, "scan 1", h.scan(t), retryTrackBody, trackAudioBytes(1))
	h.assertOKWithTrack0(t, "scan 1", false)
	if types := repTypesFor(t, h.st, retryTrackPath); types["transcript@t1"] {
		t.Fatalf("scan 1: the failed track left a representation: %v", types)
	}
	h.assertOpenFileServesTrack0(t, "scan 1")

	assertRequests(t, "scan 2", h.scan(t), trackAudioBytes(1))
	h.assertOKWithTrack0(t, "scan 2", true)
	if types := repTypesFor(t, h.st, retryTrackPath); !types["transcript@t1"] {
		t.Fatalf("scan 2: the retried track must produce transcript@t1, got %v", types)
	}

	assertRequests(t, "scan 3", h.scan(t))
	h.assertOKWithTrack0(t, "scan 3", true)
	if snap := h.state.Snapshot(); snap.Indexed != 3 {
		t.Errorf("run indexed=%d, want 3: the document counts as indexed on each scan", snap.Indexed)
	}
}

// TestTrackRetry_QualityGateRefusalIsNotRetried covers a track that the
// quality gate refused. Its decode is stored, so scan 2 sends no request.
func TestTrackRetry_QualityGateRefusalIsNotRetried(t *testing.T) {
	t.Parallel()
	tr := &countingTrackTranscriber{
		byInput: map[string]string{retryTrackBody: retryTrack0, trackAudioBytes(1): loopText},
	}
	h := newTrackRetryHarness(t, tr, true)

	assertRequests(t, "scan 1", h.scan(t), retryTrackBody, trackAudioBytes(1))
	doc := mustGetDoc(t, h.st, retryTrackPath)
	if strings.TrimSpace(doc.ContentHash) == "" {
		t.Fatalf("scan 1: a gate refusal must not withhold the done marker (status=%q)", doc.Status)
	}
	assertRequests(t, "scan 2", h.scan(t))
}

// TestTrackRetry_NoSpeechTrackIsNotRetried covers a track whose decode holds
// no text. It is a settled outcome, so scan 2 sends no request.
func TestTrackRetry_NoSpeechTrackIsNotRetried(t *testing.T) {
	t.Parallel()
	tr := &countingTrackTranscriber{
		byInput: map[string]string{retryTrackBody: retryTrack0, trackAudioBytes(1): ""},
	}
	h := newTrackRetryHarness(t, tr, false)

	assertRequests(t, "scan 1", h.scan(t), retryTrackBody, trackAudioBytes(1))
	h.assertOKWithTrack0(t, "scan 1", true)
	assertRequests(t, "scan 2", h.scan(t))
}

// TestTrackRetry_TrackThatAlwaysFailsIsRetriedEveryScan covers a track that
// fails on every request. Each scan after the first sends exactly one request,
// for track 1 only, and the document stays ok and searchable.
func TestTrackRetry_TrackThatAlwaysFailsIsRetriedEveryScan(t *testing.T) {
	t.Parallel()
	tr := &countingTrackTranscriber{
		byInput:  map[string]string{retryTrackBody: retryTrack0},
		failLeft: map[string]int{trackAudioBytes(1): -1},
	}
	h := newTrackRetryHarness(t, tr, false)

	assertRequests(t, "scan 1", h.scan(t), retryTrackBody, trackAudioBytes(1))
	h.assertOKWithTrack0(t, "scan 1", false)
	for _, when := range []string{"scan 2", "scan 3", "scan 4"} {
		assertRequests(t, when, h.scan(t), trackAudioBytes(1))
		h.assertOKWithTrack0(t, when, false)
	}
	h.assertOpenFileServesTrack0(t, "scan 4")
}

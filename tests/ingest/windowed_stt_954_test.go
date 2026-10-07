package tests

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/avutil"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// windowRecordingTranscriber is a fake STT provider for issue #954: it records the
// byte length of every request, enforces a per-request payload cap exactly as the
// whisperapi/Voxtral clients do, and answers with WINDOW-RELATIVE timestamps (each
// decode starts at 00:00), so a test can prove the merge rebased them to absolute
// media time.
type windowRecordingTranscriber struct {
	mu sync.Mutex
	// capBytes is the declared+enforced payload cap. 0 declares no cap, so the
	// transcriber is treated as uncapped (the duration rule alone applies).
	capBytes int
	reqBytes []int
	// failAt fails the Nth request (1-based) with the given error; failAll fails
	// every request.
	failAt  map[int]error
	failAll error
}

func (w *windowRecordingTranscriber) Transcribe(ctx context.Context, relPath string, data []byte) (string, error) {
	res, err := w.TranscribeStructured(ctx, relPath, data)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

func (w *windowRecordingTranscriber) TranscribeStructured(_ context.Context, _ string, data []byte) (model.TranscriptResult, error) {
	w.mu.Lock()
	w.reqBytes = append(w.reqBytes, len(data))
	n := len(w.reqBytes)
	failAt := w.failAt[n]
	w.mu.Unlock()

	if w.failAll != nil {
		return model.TranscriptResult{}, w.failAll
	}
	if failAt != nil {
		return model.TranscriptResult{}, failAt
	}
	if w.capBytes > 0 && len(data) > w.capBytes {
		// The exact #954 refusal: non-retryable, so the document lands at
		// status=error with no transcript at all.
		return model.TranscriptResult{}, &model.ProviderError{
			Code:      "WHISPER_FAILED",
			Message:   fmt.Sprintf("transcription input too large (%d bytes, limit %d)", len(data), w.capBytes),
			Retryable: false,
		}
	}
	return model.TranscriptResult{
		Text: fmt.Sprintf("[00:00] opening line of decode %d\n[00:30] closing line of decode %d", n, n),
		Words: []model.TimedWord{
			{Word: fmt.Sprintf("opening%d", n), StartMS: 0, EndMS: 400},
			{Word: fmt.Sprintf("closing%d", n), StartMS: 30000, EndMS: 30400},
		},
	}, nil
}

// MaxTranscribePayloadBytes implements model.PayloadLimitedTranscriber only when a
// cap is declared; a 0 cap still satisfies the interface and reports "no cap", so
// both branches of sttPayloadCapBytes are exercised.
func (w *windowRecordingTranscriber) MaxTranscribePayloadBytes() int { return w.capBytes }

func (w *windowRecordingTranscriber) requests() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]int(nil), w.reqBytes...)
}

// sttWindowCut is one window the service asked ffmpeg (here, the stub) to cut.
type sttWindowCut struct{ startMS, endMS int }

// windowSTTHarness wires an ingest Service whose duration probe and segment
// extraction are deterministic stubs, so the #954 windowing decision is exercised
// without ffmpeg/ffprobe. The stub slice is sized proportionally to its duration,
// exactly as a real re-encoded slice of a constant-bitrate recording would be.
type windowSTTHarness struct {
	svc      *ingest.Service
	store    *fakeIngestStore
	tr       *windowRecordingTranscriber
	logs     *syncBuffer
	stateDir string

	mu   sync.Mutex
	cuts []sttWindowCut
	// oversize multiplies the stub slice's size, so a test can make a window come
	// out larger than its duration-proportional estimate (ffmpeg copies the source
	// codec, so a variable-bitrate stretch really can).
	oversize int
	// failCut fails the cut of the window starting at the given offset (ms).
	failCut map[int]error
}

func newWindowSTTHarness(t *testing.T, tr *windowRecordingTranscriber, totalMS int, payload []byte) *windowSTTHarness {
	t.Helper()
	h := &windowSTTHarness{
		store:    &fakeIngestStore{},
		tr:       tr,
		logs:     &syncBuffer{},
		stateDir: testutil.TempDir(t),
	}
	h.svc = mustNewIngestService(t, config.Config{StateDir: h.stateDir}, h.store)
	h.svc.SetTranscriber(tr)
	h.svc.SetLogger(log.New(h.logs, "", 0))
	h.svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) {
		return time.Duration(totalMS) * time.Millisecond, nil
	}
	h.svc.ExtractSegmentFunc = func(_ context.Context, _ string, startMS, endMS int) ([]byte, error) {
		h.mu.Lock()
		h.cuts = append(h.cuts, sttWindowCut{startMS: startMS, endMS: endMS})
		failCut := h.failCut[startMS]
		oversize := h.oversize
		h.mu.Unlock()
		if failCut != nil {
			return nil, failCut
		}
		if totalMS <= 0 {
			return nil, errors.New("no duration")
		}
		if oversize < 1 {
			oversize = 1
		}
		return make([]byte, oversize*len(payload)*(endMS-startMS)/totalMS), nil
	}
	return h
}

func (h *windowSTTHarness) windows() []sttWindowCut {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]sttWindowCut(nil), h.cuts...)
}

// transcriptText returns the merged transcript the run persisted to the cache.
func (h *windowSTTHarness) transcriptText(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(h.stateDir, "cache", "transcribe", ingest.ComputeContentHash(content)+".txt")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read transcript cache %s: %v", path, err)
	}
	return string(raw)
}

// transcriptStarts parses the [mm:ss] markers of a merged transcript into ms.
func transcriptStarts(t *testing.T, transcript string) []int {
	t.Helper()
	var out []int
	for _, line := range strings.Split(transcript, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[") {
			continue
		}
		end := strings.Index(line, "]")
		if end < 0 {
			t.Fatalf("transcript line has no closing marker: %q", line)
		}
		var mins, secs int
		if _, err := fmt.Sscanf(line[1:end], "%d:%d", &mins, &secs); err != nil {
			t.Fatalf("unparsable timestamp %q: %v", line[1:end], err)
		}
		out = append(out, (mins*60+secs)*1000)
	}
	return out
}

func mediaDoc(relPath string) model.Document {
	return model.Document{DocID: 1, RelPath: relPath, DocType: "audio"}
}

// TestSTTWindowMS pins the two trigger rules and the cap-derived window size. It is
// the pure core of #954: a recording that fits in one request must return 0 (send
// it whole, today's behaviour), and everything else must return a window that both
// providers and the request timeout can survive.
func TestSTTWindowMS(t *testing.T) {
	t.Parallel()
	const mb = 1024 * 1024
	for _, tc := range []struct {
		name         string
		totalMS      int
		payloadBytes int
		capBytes     int
		want         int
	}{
		{"short recording under the cap goes out whole", 5 * 60 * 1000, 4 * mb, 50 * mb, 0},
		{"exactly the default window still goes out whole", ingest.DefaultSTTWindowMS, mb, 50 * mb, 0},
		{"uncapped provider still windows a long recording", 3 * 60 * 60 * 1000, 10 * mb, 0, ingest.DefaultSTTWindowMS},
		{"the #954 recording windows on the duration rule first", 3*60*60*1000 + 2*60*1000, 89309044, 52428800, ingest.DefaultSTTWindowMS},
		{"a short recording over the cap is windowed too", 5 * 60 * 1000, 9000, 5000, 133_333},
		{"a dense payload is floored, never sliced into rubble", 60 * 1000, 100 * mb, mb, 30_000},
		{"an unknown duration is never windowed", 0, 100 * mb, 50 * mb, 0},
		{"a cap larger than the payload leaves the default window", 30 * 60 * 1000, mb, 50 * mb, ingest.DefaultSTTWindowMS},
		// media.stt.max_payload_mb clamps to math.MaxInt, so the cap arithmetic must
		// not overflow and collapse the window to its floor.
		{"an enormous configured cap keeps the default window", 3 * 60 * 60 * 1000, 2000 * mb, math.MaxInt, ingest.DefaultSTTWindowMS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ingest.STTWindowMS(tc.totalMS, tc.payloadBytes, tc.capBytes); got != tc.want {
				t.Errorf("STTWindowMS(%d, %d, %d) = %d, want %d", tc.totalMS, tc.payloadBytes, tc.capBytes, got, tc.want)
			}
		})
	}
}

// TestWindowedSTT_LongRecordingIsWindowed is the end-to-end shape of the fix: a
// recording longer than the decode window is sent as SEVERAL requests, every one
// of them inside the provider's cap, and the merged transcript carries ABSOLUTE,
// monotonic timestamps rather than each window's own 00:00.
//
// Mutants killed: timestamps not rebased by the window start (every marker would
// be 00:00/00:30 and the windows would collapse), and a long recording routed
// through a single request (one request, no cuts).
func TestWindowedSTT_LongRecordingIsWindowed(t *testing.T) {
	t.Parallel()
	const totalMS = 30 * 60 * 1000 // 30 minutes: past the 10-minute decode window
	content := make([]byte, 300_000)
	tr := &windowRecordingTranscriber{capBytes: 200_000}
	h := newWindowSTTHarness(t, tr, totalMS, content)

	if err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/long.m4a"), content); err != nil {
		t.Fatalf("windowed transcription failed: %v", err)
	}

	cuts := h.windows()
	if len(cuts) != 4 {
		t.Fatalf("cut %d windows, want 4 (30 min at a %d ms window, stepping by window minus overlap): %+v", len(cuts), ingest.DefaultSTTWindowMS, cuts)
	}
	if cuts[0].startMS != 0 || cuts[len(cuts)-1].endMS != totalMS {
		t.Errorf("windows do not tile the recording: %+v", cuts)
	}
	reqs := tr.requests()
	if len(reqs) != len(cuts) {
		t.Fatalf("provider saw %d requests, want one per window (%d)", len(reqs), len(cuts))
	}
	for i, n := range reqs {
		if n > tr.capBytes {
			t.Errorf("request %d carried %d bytes, over the provider cap %d", i+1, n, tr.capBytes)
		}
		if n == len(content) {
			t.Errorf("request %d carried the WHOLE file (%d bytes); windowing did not slice it", i+1, n)
		}
	}

	transcript := h.transcriptText(t, content)
	starts := transcriptStarts(t, transcript)
	if len(starts) != 2*len(cuts) {
		t.Fatalf("merged %d segments, want two per window (%d):\n%s", len(starts), 2*len(cuts), transcript)
	}
	for i := 1; i < len(starts); i++ {
		if starts[i] <= starts[i-1] {
			t.Fatalf("timestamps are not monotonic (%v); windows were not rebased:\n%s", starts, transcript)
		}
	}
	if last := starts[len(starts)-1]; last < cuts[len(cuts)-1].startMS {
		t.Errorf("last segment at %d ms is before the last window start %d ms; timestamps were not rebased", last, cuts[len(cuts)-1].startMS)
	}
	if !strings.Contains(h.logs.String(), "4/4 windows decoded") {
		t.Errorf("missing the per-document progress line in:\n%s", h.logs.String())
	}
}

// TestWindowedSTT_ShortRecordingIsOneRequest pins the unchanged common case: a
// recording that fits goes out as exactly ONE request carrying the whole file, no
// segment is cut, and the transcript is the provider's answer verbatim.
//
// Mutant killed: routing the short-recording path through windowing.
func TestWindowedSTT_ShortRecordingIsOneRequest(t *testing.T) {
	t.Parallel()
	content := []byte("short-audio-bytes")
	tr := &windowRecordingTranscriber{capBytes: 50 * 1024 * 1024}
	h := newWindowSTTHarness(t, tr, 5*60*1000, content)

	if err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/short.m4a"), content); err != nil {
		t.Fatalf("transcription failed: %v", err)
	}

	if cuts := h.windows(); len(cuts) != 0 {
		t.Errorf("a recording that fits must not be sliced, got %+v", cuts)
	}
	reqs := tr.requests()
	if len(reqs) != 1 || reqs[0] != len(content) {
		t.Fatalf("provider saw %v, want exactly one request of %d bytes (the whole file)", reqs, len(content))
	}
	if got, want := h.transcriptText(t, content), "[00:00] opening line of decode 1\n[00:30] closing line of decode 1"; got != want {
		t.Errorf("single-request transcript changed:\ngot  %q\nwant %q", got, want)
	}
}

// TestWindowedSTT_PayloadOverCapNowSucceeds reproduces the #954 failure exactly: a
// recording SHORT enough that the duration rule does not fire, whose extracted
// audio is still over the provider's payload cap. Before the fix the single
// request was refused ("transcription input too large") and the document was
// stamped status=error with no transcript; now the payload cap alone triggers
// windowing and the document is transcribed.
//
// Mutant killed: the payload-cap trigger ignored (the run fails with the refusal).
func TestWindowedSTT_PayloadOverCapNowSucceeds(t *testing.T) {
	t.Parallel()
	const totalMS = 5 * 60 * 1000 // well under the 10-minute decode window
	content := make([]byte, 9000)
	tr := &windowRecordingTranscriber{capBytes: 5000}
	h := newWindowSTTHarness(t, tr, totalMS, content)

	if err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/dense.wav"), content); err != nil {
		t.Fatalf("a recording over the payload cap must be windowed, not refused: %v", err)
	}

	if len(h.windows()) < 2 {
		t.Fatalf("expected the over-cap payload to be sliced, got %+v", h.windows())
	}
	for i, n := range tr.requests() {
		if n > tr.capBytes {
			t.Errorf("request %d carried %d bytes, still over the cap %d", i+1, n, tr.capBytes)
		}
	}
	if len(h.store.reps) != 1 {
		t.Fatalf("expected one persisted transcript representation, got %d", len(h.store.reps))
	}
}

// TestWindowedSTT_WindowFailures covers the failure contract: one window that
// fails (silence, a provider error that survived the client's retries) is skipped
// and the rest of the recording survives, while EVERY window failing is systemic
// and fails the document exactly as a single refused request does today.
//
// Mutant killed: the all-windows-failed rule removed (the run would return an
// empty transcript and no error).
func TestWindowedSTT_WindowFailures(t *testing.T) {
	t.Parallel()
	const totalMS = 30 * 60 * 1000
	content := make([]byte, 300_000)

	t.Run("one failing window is skipped", func(t *testing.T) {
		t.Parallel()
		tr := &windowRecordingTranscriber{
			capBytes: 200_000,
			failAt:   map[int]error{2: &model.ProviderError{Code: "WHISPER_FAILED", Message: "upstream hiccup", Retryable: true}},
		}
		h := newWindowSTTHarness(t, tr, totalMS, content)
		if err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/gap.m4a"), content); err != nil {
			t.Fatalf("one failed window must not lose the document: %v", err)
		}
		transcript := h.transcriptText(t, content)
		if !strings.Contains(transcript, "decode 1") || !strings.Contains(transcript, "decode 3") {
			t.Errorf("surviving windows were lost:\n%s", transcript)
		}
		if strings.Contains(transcript, "decode 2") {
			t.Errorf("the failed window contributed text:\n%s", transcript)
		}
		if !strings.Contains(h.logs.String(), "3/4 windows decoded") {
			t.Errorf("missing the windows done/total line in:\n%s", h.logs.String())
		}
	})

	t.Run("every window failing fails the document", func(t *testing.T) {
		t.Parallel()
		boom := &model.ProviderError{Code: "WHISPER_FAILED", Message: "provider down", Retryable: true}
		tr := &windowRecordingTranscriber{capBytes: 200_000, failAll: boom}
		h := newWindowSTTHarness(t, tr, totalMS, content)
		err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/dead.m4a"), content)
		if err == nil {
			t.Fatal("every window failing must fail the document")
		}
		if !strings.Contains(err.Error(), "all 4 windows failed") {
			t.Errorf("error does not name the systemic failure: %v", err)
		}
		var pe *model.ProviderError
		if !errors.As(err, &pe) {
			t.Errorf("provider classification lost in aggregation: %v", err)
		}
		if len(h.store.reps) != 0 {
			t.Errorf("expected no representation when every window failed, got %d", len(h.store.reps))
		}
	})
}

// TestWindowedSTT_UncappedProviderWindowsOnDurationOnly pins the optional-capability
// contract: a transcriber that declares no payload cap is treated as uncapped, so a
// short recording is never sliced no matter how big its payload is, and a long one
// is still windowed on the duration rule.
func TestWindowedSTT_UncappedProviderWindowsOnDurationOnly(t *testing.T) {
	t.Parallel()
	content := make([]byte, 300_000)

	tr := &windowRecordingTranscriber{} // capBytes 0: declares no cap
	h := newWindowSTTHarness(t, tr, 5*60*1000, content)
	if err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/uncapped-short.m4a"), content); err != nil {
		t.Fatalf("transcription failed: %v", err)
	}
	if cuts := h.windows(); len(cuts) != 0 {
		t.Errorf("an uncapped provider must not window a short recording, got %+v", cuts)
	}

	trLong := &windowRecordingTranscriber{}
	hLong := newWindowSTTHarness(t, trLong, 30*60*1000, content)
	if err := hLong.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/uncapped-long.m4a"), content); err != nil {
		t.Fatalf("transcription failed: %v", err)
	}
	if cuts := hLong.windows(); len(cuts) != 4 {
		t.Errorf("an uncapped provider must still window a long recording, got %+v", cuts)
	}
}

// TestWindowedSTT_UncuttableAudioFallsBackToOneRequest pins the honest degradation:
// ffmpeg is what slices the audio, so when it is missing OR cannot cut this
// container the recording cannot be windowed, but it must still be sent whole
// exactly as before #954. A slicing failure must never turn a transcript into a
// failed document.
func TestWindowedSTT_UncuttableAudioFallsBackToOneRequest(t *testing.T) {
	t.Parallel()
	content := make([]byte, 300_000)
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"ffmpeg is not installed", avutil.ErrToolNotFound},
		{"ffmpeg cannot cut the container", errors.New("ffmpeg segment: exit status 1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := &windowRecordingTranscriber{}
			h := newWindowSTTHarness(t, tr, 30*60*1000, content)
			h.svc.ExtractSegmentFunc = func(context.Context, string, int, int) ([]byte, error) {
				return nil, tc.err
			}

			if err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/uncuttable.m4a"), content); err != nil {
				t.Fatalf("a slicing failure must not fail the document: %v", err)
			}
			reqs := tr.requests()
			if len(reqs) != 1 || reqs[0] != len(content) {
				t.Fatalf("provider saw %v, want one request carrying the whole file (%d bytes)", reqs, len(content))
			}
			if !strings.Contains(h.logs.String(), "cannot be sliced") {
				t.Errorf("the fallback was not reported in:\n%s", h.logs.String())
			}
		})
	}
}

// TestWindowedSTT_RealSegmentExtraction exercises the same path against REAL
// ffmpeg/ffprobe on a synthesized recording, so the stubbed duration probe and
// segment cut in the tests above cannot hide an avutil integration break (wrong
// extension, an empty slice, an off-by-one window). It is skipped when the
// binaries are absent, which is how the rest of the suite treats external tools.
func TestWindowedSTT_RealSegmentExtraction(t *testing.T) {
	t.Parallel()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed: the real segment-extraction path cannot be exercised")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed: the real duration probe cannot be exercised")
	}
	dir := testutil.TempDir(t)
	src := filepath.Join(dir, "tone.wav")
	cmd := exec.CommandContext(context.Background(), ffmpeg, "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=70", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg could not synthesize the fixture: %v: %s", err, out)
	}
	content, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	// A cap under the whole file forces windowing; each window's share stays inside
	// it, so every request the provider sees is acceptable.
	tr := &windowRecordingTranscriber{capBytes: len(content) * 2 / 3}
	st := &fakeIngestStore{}
	stateDir := testutil.TempDir(t)
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir}, st)
	svc.SetTranscriber(tr)

	if err := svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("tone.wav"), content); err != nil {
		t.Fatalf("real windowed transcription failed: %v", err)
	}
	reqs := tr.requests()
	if len(reqs) < 2 {
		t.Fatalf("expected the over-cap recording to be sliced by real ffmpeg, got %d request(s)", len(reqs))
	}
	for i, n := range reqs {
		if n == 0 {
			t.Errorf("request %d carried an empty slice", i+1)
		}
		if n > tr.capBytes {
			t.Errorf("request %d carried %d bytes, over the cap %d", i+1, n, tr.capBytes)
		}
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, "cache", "transcribe", ingest.ComputeContentHash(content)+".txt"))
	if err != nil {
		t.Fatalf("read transcript cache: %v", err)
	}
	starts := transcriptStarts(t, string(raw))
	if len(starts) < 2 {
		t.Fatalf("expected several merged segments, got %q", raw)
	}
	for i := 1; i < len(starts); i++ {
		if starts[i] <= starts[i-1] {
			t.Fatalf("timestamps are not monotonic (%v):\n%s", starts, raw)
		}
	}
}

// TestWindowedSTT_OversizedWindowIsSplit pins the guard behind the size estimate:
// the window is sized from the WHOLE file's bytes per millisecond, but ffmpeg
// copies the source codec, so a window can still come out over the provider cap.
// Such a window is halved and sent as pieces, instead of being refused with the
// very error #954 is about.
func TestWindowedSTT_OversizedWindowIsSplit(t *testing.T) {
	t.Parallel()
	const totalMS = 30 * 60 * 1000
	content := make([]byte, 300_000)
	tr := &windowRecordingTranscriber{capBytes: 200_000}
	h := newWindowSTTHarness(t, tr, totalMS, content)
	h.oversize = 3 // every cut comes out three times its estimate

	if err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/vbr.m4a"), content); err != nil {
		t.Fatalf("an oversized window must be split, not refused: %v", err)
	}
	reqs := tr.requests()
	if len(reqs) <= 4 {
		t.Fatalf("expected the oversized windows to be split into more than 4 requests, got %v", reqs)
	}
	for i, n := range reqs {
		if n > tr.capBytes {
			t.Errorf("request %d carried %d bytes, over the provider cap %d", i+1, n, tr.capBytes)
		}
	}
	starts := transcriptStarts(t, h.transcriptText(t, content))
	for i := 1; i < len(starts); i++ {
		if starts[i] <= starts[i-1] {
			t.Fatalf("split pieces broke the timeline: %v", starts)
		}
	}
}

// TestWindowedSTT_OneUncuttableWindowIsSkipped pins that a single bad region costs
// one window, not the document: ffmpeg failing on one window leaves the rest of the
// recording transcribed.
func TestWindowedSTT_OneUncuttableWindowIsSkipped(t *testing.T) {
	t.Parallel()
	const totalMS = 30 * 60 * 1000
	content := make([]byte, 300_000)
	tr := &windowRecordingTranscriber{capBytes: 200_000}
	h := newWindowSTTHarness(t, tr, totalMS, content)
	// The second scheduled window starts one step (window minus overlap) in.
	secondStart := ingest.DefaultSTTWindowMS - ingest.TranscriptWindowOverlapMS(ingest.DefaultSTTWindowMS)
	h.failCut = map[int]error{secondStart: errors.New("ffmpeg segment: exit status 1")}

	if err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/one-bad-region.m4a"), content); err != nil {
		t.Fatalf("one uncuttable window must not fail the document: %v", err)
	}
	if got := len(tr.requests()); got != 3 {
		t.Fatalf("provider saw %d requests, want 3 (the four windows minus the uncuttable one)", got)
	}
	if !strings.Contains(h.logs.String(), "3/4 windows decoded") {
		t.Errorf("missing the windows done/total line in:\n%s", h.logs.String())
	}
	if !strings.Contains(h.logs.String(), "cannot cut window") {
		t.Errorf("the skipped cut was not reported in:\n%s", h.logs.String())
	}
}

// TestWindowedSTT_UnshrinkableWindowIsNeverSent pins the last line of the payload
// contract: when even the smallest split is still over the provider cap, the piece
// is NOT sent. The client enforces the same cap locally, so the request would be
// refused without reaching the server; skipping it costs the same audio and says
// exactly why. With every window in that state the document fails with that reason.
func TestWindowedSTT_UnshrinkableWindowIsNeverSent(t *testing.T) {
	t.Parallel()
	content := make([]byte, 300_000)
	tr := &windowRecordingTranscriber{capBytes: 200_000}
	h := newWindowSTTHarness(t, tr, 30*60*1000, content)
	// Even the shortest piece the splitter will produce stays over the cap.
	h.oversize = 200

	err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/unshrinkable.m4a"), content)
	if err == nil {
		t.Fatal("a recording no window of which fits the cap must fail the document")
	}
	if !strings.Contains(err.Error(), "over the provider cap") {
		t.Errorf("error does not name the payload cap: %v", err)
	}
	if reqs := tr.requests(); len(reqs) != 0 {
		t.Errorf("an oversized piece was sent anyway: %v", reqs)
	}
}

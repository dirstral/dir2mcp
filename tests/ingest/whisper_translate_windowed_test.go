package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/providerhttp"
	"github.com/dirstral/dir2mcp/internal/whisperapi"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// The whisper translate engine decodes a long recording in time windows, the way
// the source transcription does (SPEC §8.6.2, §8.6.13). On the TV Rain archive a
// 72-minute broadcast transcribed as "8/8 windows decoded (window 600s)", but its
// audio was under the whisper server's payload cap, so the translate pass sent the
// whole track in ONE request and timed out at 120 s on every scan
// ("whisper-translate …: WHISPER_FAILED: transcription request timed out"): no
// English track, ever.

// broadcastMS is the TV Rain example recording (140cd8e9…, 4318 s).
const broadcastMS = 4318 * 1000

// translateServerMaxAudioMS is the longest audio the fake server answers before
// the client gives up: anything longer stands for a decode slower than the
// request timeout. A 600 s window fits; the whole 4318 s broadcast does not.
const translateServerMaxAudioMS = 700 * 1000

// fakeAudio encodes the absolute range a slice of the staged audio covers, so the
// fake server can tell how much audio one request carries and where it sits.
func fakeAudio(startMS, endMS int) []byte {
	return []byte(fmt.Sprintf("AUDIO start=%d end=%d", startMS, endMS))
}

// translateServer is a faster-whisper style /v1/audio/transcriptions endpoint.
// A request carrying more than maxAudioMS of audio hangs until the client hangs
// up (a timeout, as in production); rejectStarts answers the window starting at
// that absolute ms with a non-retryable 400. Every answer is WINDOW-RELATIVE
// (each decode starts at 0 s), so a test can prove the merge rebased it.
type translateServer struct {
	mu           sync.Mutex
	maxAudioMS   int
	rejectStarts map[int]bool
	rejectAll    bool
	audioMS      []int
	tasks        []string
}

func (f *translateServer) handler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	raw, _ := io.ReadAll(file)
	var startMS, endMS int
	if _, err := fmt.Sscanf(string(raw), "AUDIO start=%d end=%d", &startMS, &endMS); err != nil {
		http.Error(w, "unrecognised audio", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.audioMS = append(f.audioMS, endMS-startMS)
	f.tasks = append(f.tasks, r.FormValue("task"))
	reject := f.rejectAll || f.rejectStarts[startMS]
	f.mu.Unlock()

	if endMS-startMS > f.maxAudioMS {
		<-r.Context().Done() // still decoding when the client gives up
		return
	}
	if reject {
		http.Error(w, "decoder failed on this window", http.StatusBadRequest)
		return
	}
	sec := startMS / 1000
	segment := func(relSec int, text string) map[string]any {
		return map[string]any{
			"start": float64(relSec), "end": float64(relSec + 4), "text": text,
			"words": []map[string]any{{"word": text, "start": float64(relSec), "end": float64(relSec + 1)}},
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"text":     "",
		"language": "en",
		"duration": float64(endMS-startMS) / 1000,
		"segments": []map[string]any{
			segment(0, fmt.Sprintf("english-at-%d", sec)),
			segment(300, fmt.Sprintf("english-at-%d", sec+300)),
		},
	})
}

func (f *translateServer) requests() ([]int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.audioMS...), append([]string(nil), f.tasks...)
}

// translateClient is the whisper translate-task client against srv, with a short
// explicit request timeout so the hang above fails the request within the test.
func translateClient(srv *httptest.Server) *whisperapi.Client {
	c := whisperapi.NewClient(srv.URL, "")
	c.Task = whisperapi.TaskTranslate
	c.HTTPClient = providerhttp.NewClient(500 * time.Millisecond)
	c.RequestTimeout = 500 * time.Millisecond
	c.MaxRetries = 0
	return c
}

// newBroadcastService wires a service that transcribes and whisper-translates a
// 4318 s broadcast video whose audio slices are fakeAudio ranges.
func newBroadcastService(t *testing.T, stateDir string, st *fakeIngestStore, tr model.Transcriber, cfg config.Config) *ingest.Service {
	t.Helper()
	cfg.StateDir = stateDir
	svc := mustNewIngestService(t, cfg, st)
	svc.SetTranscriber(&windowRecordingTranscriber{})
	svc.SetTranscriptLanguage("ru")
	svc.SetTranslateTranscriber(tr, "whisper", "large-v3", []string{"en"})
	svc.ExtractAudioTrackFunc = func(context.Context, string) ([]byte, error) { return fakeAudio(0, broadcastMS), nil }
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) {
		return broadcastMS * time.Millisecond, nil
	}
	svc.ExtractSegmentFunc = func(_ context.Context, _ string, startMS, endMS int) ([]byte, error) {
		return fakeAudio(startMS, endMS), nil
	}
	return svc
}

var broadcastDoc = model.Document{DocID: 4318, RelPath: "14/0c/140cd8e9_480p.mp4", DocType: "video"}

// translateCache returns the whisper-translate cache text and words, or "" and
// nil when nothing was cached.
func translateCache(t *testing.T, stateDir string) (string, []model.TimedWord) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(stateDir, "cache", "transcribe", "*-translate*.txt"))
	if len(matches) == 0 {
		return "", nil
	}
	if len(matches) > 1 {
		t.Fatalf("expected one translate cache entry, got %v", matches)
	}
	text, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read translate cache: %v", err)
	}
	var words []model.TimedWord
	if raw, err := os.ReadFile(strings.TrimSuffix(matches[0], ".txt") + ".words.json"); err == nil {
		if err := json.Unmarshal(raw, &words); err != nil {
			t.Fatalf("decode translate words: %v", err)
		}
	}
	return string(text), words
}

func hasEnglishRep(st *fakeIngestStore) bool {
	for _, r := range st.reps {
		if r.RepType == ingest.TranscriptRepType("en") {
			return true
		}
	}
	return false
}

// broadcastWindowStarts is the transcription schedule for 4318 s: 600 s windows,
// 10 s overlap, a 590 s step, 8 windows.
func broadcastWindowStarts() []int {
	return ingest.TranscriptWindowStarts(broadcastMS, ingest.DefaultSTTWindowMS-ingest.TranscriptWindowOverlapMS(ingest.DefaultSTTWindowMS), ingest.TranscriptWindowOverlapMS(ingest.DefaultSTTWindowMS))
}

// TestWhisperTranslate_LongRecordingUnderCapIsWindowed is the production
// regression: audio under the payload cap but longer than the transcription
// window is translated in the same 600 s windows, no request carries more than a
// window, and the merged English track sits at absolute broadcast time.
func TestWhisperTranslate_LongRecordingUnderCapIsWindowed(t *testing.T) {
	t.Parallel()
	fake := &translateServer{maxAudioMS: translateServerMaxAudioMS}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	stateDir := testutil.TempDir(t)
	st := &fakeIngestStore{}
	svc := newBroadcastService(t, stateDir, st, translateClient(srv), config.Config{})

	if err := svc.GenerateTranscriptRepresentation(context.Background(), broadcastDoc, []byte("VIDEO")); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation: %v", err)
	}
	starts := broadcastWindowStarts()
	assertOneRequestPerWindow(t, fake, len(starts))
	if !hasEnglishRep(st) {
		t.Fatalf("expected an English transcript representation, got %+v", st.reps)
	}
	assertAbsoluteEnglishTrack(t, stateDir, starts)
}

// assertOneRequestPerWindow checks the translate pass sent one task=translate
// request per transcription window, none longer than a window.
func assertOneRequestPerWindow(t *testing.T, fake *translateServer, windows int) {
	t.Helper()
	audio, tasks := fake.requests()
	if len(audio) != windows {
		t.Fatalf("translate requests = %d (audio ms %v), want one per transcription window (%d)", len(audio), audio, windows)
	}
	for i, ms := range audio {
		if ms > ingest.DefaultSTTWindowMS {
			t.Fatalf("translate request %d carried %d ms of audio, more than one %d ms window", i+1, ms, ingest.DefaultSTTWindowMS)
		}
		if tasks[i] != "translate" {
			t.Fatalf("translate request %d sent task=%q, want translate", i+1, tasks[i])
		}
	}
}

// assertAbsoluteEnglishTrack checks the cached English track holds each window's
// two segments, and their words, at absolute broadcast time.
func assertAbsoluteEnglishTrack(t *testing.T, stateDir string, starts []int) {
	t.Helper()
	text, words := translateCache(t, stateDir)
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 2*len(starts) {
		t.Fatalf("merged English track has %d lines, want %d:\n%s", len(lines), 2*len(starts), text)
	}
	for i, start := range starts {
		for j, rel := range []int{0, 300} {
			abs := start/1000 + rel
			want := fmt.Sprintf("english-at-%d", abs)
			line := lines[2*i+j]
			if !strings.HasSuffix(line, " "+want) || !strings.HasPrefix(line, fmtMarker(abs)) {
				t.Fatalf("line %d = %q, want %s %s", 2*i+j, line, fmtMarker(abs), want)
			}
		}
	}
	if len(words) != 2*len(starts) {
		t.Fatalf("merged English words = %d, want %d: %+v", len(words), 2*len(starts), words)
	}
	for _, w := range words {
		var abs int
		if _, err := fmt.Sscanf(w.Word, "english-at-%d", &abs); err != nil || w.StartMS != abs*1000 {
			t.Fatalf("word %+v is not at its absolute time %d ms", w, abs*1000)
		}
	}
}

// fmtMarker renders the [mm:ss] / [h:mm:ss] transcript marker for sec.
func fmtMarker(sec int) string {
	if sec >= 3600 {
		return fmt.Sprintf("[%02d:%02d:%02d]", sec/3600, sec/60%60, sec%60)
	}
	return fmt.Sprintf("[%02d:%02d]", sec/60, sec%60)
}

// TestWhisperTranslate_FailedWindowIsSkipped pins the §8.6.13 failed-window rule
// on the translate pass: one window the server rejects is skipped with the rest
// of the recording still translated and cached.
func TestWhisperTranslate_FailedWindowIsSkipped(t *testing.T) {
	t.Parallel()
	starts := broadcastWindowStarts()
	failed := starts[3]
	fake := &translateServer{maxAudioMS: translateServerMaxAudioMS, rejectStarts: map[int]bool{failed: true}}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	stateDir := testutil.TempDir(t)
	st := &fakeIngestStore{}
	svc := newBroadcastService(t, stateDir, st, translateClient(srv), config.Config{})

	if err := svc.GenerateTranscriptRepresentation(context.Background(), broadcastDoc, []byte("VIDEO")); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation: %v", err)
	}
	if !hasEnglishRep(st) {
		t.Fatalf("one failed window must not cost the English track, got %+v", st.reps)
	}
	text, _ := translateCache(t, stateDir)
	if strings.Contains(text, fmt.Sprintf("english-at-%d", failed/1000)) {
		t.Fatalf("the rejected window's text is in the track:\n%s", text)
	}
	for _, start := range starts {
		if start == failed {
			continue
		}
		if want := fmt.Sprintf("english-at-%d", start/1000); !strings.Contains(text, want) {
			t.Fatalf("window at %d ms missing from the track (%s):\n%s", start, want, text)
		}
	}
}

// TestWhisperTranslate_AllWindowsFailedIsRetried pins that a translation whose
// every window failed is a failure, not an empty English track: nothing is
// cached or indexed, and the next run translates again and succeeds.
func TestWhisperTranslate_AllWindowsFailedIsRetried(t *testing.T) {
	t.Parallel()
	fake := &translateServer{maxAudioMS: translateServerMaxAudioMS, rejectAll: true}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	stateDir := testutil.TempDir(t)
	st := &fakeIngestStore{}
	svc := newBroadcastService(t, stateDir, st, translateClient(srv), config.Config{})

	_ = svc.GenerateTranscriptRepresentation(context.Background(), broadcastDoc, []byte("VIDEO"))
	if hasEnglishRep(st) {
		t.Fatalf("an all-failed translation must not index an English track, got %+v", st.reps)
	}
	if text, _ := translateCache(t, stateDir); text != "" {
		t.Fatalf("an all-failed translation must not be cached, got:\n%s", text)
	}
	if audio, _ := fake.requests(); len(audio) != len(broadcastWindowStarts()) {
		t.Fatalf("every window should have been attempted once, got %v", audio)
	}

	fake.mu.Lock()
	fake.rejectAll = false
	fake.mu.Unlock()
	st2 := &fakeIngestStore{}
	svc2 := newBroadcastService(t, stateDir, st2, translateClient(srv), config.Config{})
	if err := svc2.GenerateTranscriptRepresentation(context.Background(), broadcastDoc, []byte("VIDEO")); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !hasEnglishRep(st2) {
		t.Fatalf("the next run must translate again, got %+v", st2.reps)
	}
}

// TestWhisperTranslate_ConfiguredWindowNarrowsOnly pins that
// media.translate.whisper_window_sec narrows the translate window but never
// widens a request past the transcription window.
func TestWhisperTranslate_ConfiguredWindowNarrowsOnly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		windowSec int
		wantMaxMS int
	}{
		{"narrower window applies", 300, 300 * 1000},
		{"wider window is capped at the transcription window", 1800, ingest.DefaultSTTWindowMS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &translateServer{maxAudioMS: translateServerMaxAudioMS}
			srv := httptest.NewServer(http.HandlerFunc(fake.handler))
			defer srv.Close()
			st := &fakeIngestStore{}
			svc := newBroadcastService(t, testutil.TempDir(t), st, translateClient(srv),
				config.Config{MediaTranslateWhisperWindowSec: tc.windowSec})
			if err := svc.GenerateTranscriptRepresentation(context.Background(), broadcastDoc, []byte("VIDEO")); err != nil {
				t.Fatalf("GenerateTranscriptRepresentation: %v", err)
			}
			audio, _ := fake.requests()
			longest := 0
			for _, ms := range audio {
				longest = max(longest, ms)
			}
			if longest != tc.wantMaxMS {
				t.Fatalf("longest translate request = %d ms, want %d (requests %v)", longest, tc.wantMaxMS, audio)
			}
			if !hasEnglishRep(st) {
				t.Fatalf("expected an English transcript representation, got %+v", st.reps)
			}
		})
	}
}

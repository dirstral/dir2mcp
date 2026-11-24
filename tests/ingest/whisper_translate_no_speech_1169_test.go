package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Where the no-speech rules (spec 0.99.0, SPEC §8.2, §8.6.13) meet the windowed
// whisper translation (spec 0.100.0, SPEC §8.6.2). A no-speech window is a
// decoded window, not a failed one. A window split under the payload cap that
// lost a piece is a failed translate window, also when its other piece decoded.
// So a translation with a no-speech window and a window that lost a piece is
// incomplete: it is not cached and the next run decodes it again. With the
// lost piece back, the no-speech window alone does not make it incomplete, and
// the complete translation is cached.

// silentTranslateServer wraps translateServer. A request whose audio starts at
// a silent start (or any request, with silentAll) gets a well-formed response
// with no speech, `{"text":"","segments":[]}` (SPEC §8.2).
type silentTranslateServer struct {
	*translateServer
	mu        sync.Mutex
	silent    map[int]bool
	silentAll bool
}

func (f *silentTranslateServer) handler(w http.ResponseWriter, r *http.Request) {
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
	_ = file.Close()
	var startMS, endMS int
	if _, err := fmt.Sscanf(string(raw), "AUDIO start=%d end=%d", &startMS, &endMS); err != nil {
		http.Error(w, "unrecognised audio", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	silent := f.silentAll || f.silent[startMS]
	f.mu.Unlock()
	f.translateServer.mu.Lock()
	reject := f.rejectStarts[startMS]
	f.translateServer.mu.Unlock()
	if !silent || reject {
		f.translateServer.handler(w, r)
		return
	}
	f.translateServer.mu.Lock()
	f.audioMS = append(f.audioMS, endMS-startMS)
	f.tasks = append(f.tasks, r.FormValue("task"))
	f.translateServer.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"text": "", "segments": []any{}})
}

// translateCacheExists reports whether a whisper-translate cache entry exists,
// also an empty one (translateCache cannot tell an empty entry from none).
func translateCacheExists(t *testing.T, stateDir string) bool {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(stateDir, "cache", "transcribe", "*-translate*.txt"))
	if err != nil {
		t.Fatalf("glob translate cache: %v", err)
	}
	return len(matches) > 0
}

// TestWhisperTranslate_NoSpeechWindowAndLostPiece: one window holds no speech
// and another is split under the payload cap and loses a piece. The translation
// is incomplete (spec 0.100.0): it is not cached and the next run translates
// again. In the second case every decoded piece holds no speech, so the
// translation is empty; the lost piece can still hold speech, so it is not
// cached as an empty translation either (§8.6.13 "a failed window can hold
// speech"). Once the piece decodes, the translation is complete and cached,
// although a window holds no speech.
func TestWhisperTranslate_NoSpeechWindowAndLostPiece(t *testing.T) {
	t.Parallel()
	t.Run("speech elsewhere", func(t *testing.T) {
		t.Parallel()
		assertNoSpeechAndLostPiece(t, false)
	})
	t.Run("no speech in any decoded piece", func(t *testing.T) {
		t.Parallel()
		assertNoSpeechAndLostPiece(t, true)
	})
}

// noSpeechLostPieceCase is one recording for the test above: a fake translate
// server and a service factory whose window at dense is split under the cap.
type noSpeechLostPieceCase struct {
	fake     *silentTranslateServer
	stateDir string
	quiet    int
	newSvc   func(st *fakeIngestStore) *ingest.Service
}

func newNoSpeechLostPieceCase(t *testing.T, silentAll bool) *noSpeechLostPieceCase {
	t.Helper()
	starts := broadcastWindowStarts()
	quiet := starts[1]                              // a window with no speech
	dense := starts[2]                              // this window's cut overshoots the cap
	lostHalf := dense + ingest.DefaultSTTWindowMS/2 // its second half fails on run 1
	fake := &silentTranslateServer{
		translateServer: &translateServer{maxAudioMS: translateServerMaxAudioMS, rejectStarts: map[int]bool{lostHalf: true}},
		silent:          map[int]bool{quiet: true},
		silentAll:       silentAll,
	}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	stateDir := testutil.TempDir(t)
	const capBytes = 1000
	newSvc := func(st *fakeIngestStore) *ingest.Service {
		client := translateClient(srv)
		client.MaxPayloadBytes = capBytes
		svc := newBroadcastService(t, stateDir, st, client, config.Config{})
		svc.ExtractSegmentFunc = func(_ context.Context, _ string, startMS, endMS int) ([]byte, error) {
			data := fakeAudio(startMS, endMS)
			if startMS == dense && endMS-startMS == ingest.DefaultSTTWindowMS {
				data = append(data, make([]byte, 2*capBytes)...)
			}
			return data, nil
		}
		return svc
	}
	return &noSpeechLostPieceCase{fake: fake, stateDir: stateDir, quiet: quiet, newSvc: newSvc}
}

// run indexes the recording once and returns the store and the total number of
// translate requests the fake server saw so far.
func (c *noSpeechLostPieceCase) run(t *testing.T) (*fakeIngestStore, int) {
	t.Helper()
	st := &fakeIngestStore{}
	if err := c.newSvc(st).GenerateTranscriptRepresentation(context.Background(), broadcastDoc, []byte("VIDEO")); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation: %v", err)
	}
	audio, _ := c.fake.requests()
	return st, len(audio)
}

func assertNoSpeechAndLostPiece(t *testing.T, silentAll bool) {
	c := newNoSpeechLostPieceCase(t, silentAll)

	// Run 1: a no-speech window and a window that lost a piece.
	st, firstRun := c.run(t)
	if translateCacheExists(t, c.stateDir) {
		t.Fatalf("a translation that lost a piece must not be cached (spec 0.100.0), also when the other windows hold no speech")
	}
	if !silentAll {
		meta, text := englishRep(t, st)
		if !strings.Contains(meta, `"translation_incomplete":true`) {
			t.Fatalf("the translation must be marked incomplete, meta %s", meta)
		}
		if strings.Contains(text, fmt.Sprintf("english-at-%d", c.quiet/1000)) {
			t.Fatalf("the no-speech window must add no text:\n%s", text)
		}
	}
	if firstRun <= len(broadcastWindowStarts()) {
		t.Fatalf("run 1 sent %d requests, want every window plus the split halves", firstRun)
	}

	// Run 2: the piece decodes; the no-speech window stays silent.
	c.fake.translateServer.mu.Lock()
	c.fake.rejectStarts = nil
	c.fake.translateServer.mu.Unlock()
	st2, total := c.run(t)
	if total != 2*firstRun {
		t.Fatalf("run 2 must translate every window again: %d requests after run 1, %d after run 2", firstRun, total)
	}
	if !translateCacheExists(t, c.stateDir) {
		t.Fatalf("a complete translation must be cached; a no-speech window is decoded, not failed")
	}
	if !silentAll {
		if meta, _ := englishRep(t, st2); strings.Contains(meta, "translation_incomplete") {
			t.Fatalf("a no-speech window must not make the translation incomplete, meta %s", meta)
		}
	}

	// Run 3 reads the cache.
	if _, after := c.run(t); after != total {
		t.Fatalf("run 3 sent %d translate requests, want 0: the complete translation is cached", after-total)
	}
}

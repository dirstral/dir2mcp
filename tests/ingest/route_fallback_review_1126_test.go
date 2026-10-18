package tests

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §8.2.4 (spec 0.82.0, dir2mcp #1122): the review fixes of PR #1126. A
// window dropped under fail stays visible to the gate; the gate costs one
// query per scan; a cancelled run or a host problem changes nothing; the
// derivation pass never retries; a future stamp is due; a stamp-only retry
// does not rewrite the text.

// gateWindowHarness is a window-scope Service on a real store, driven through
// ProcessDocument as a scan drives it: an unchanged document reaches the
// transcript only when the re-derivation gate says so.
type gateWindowHarness struct {
	svc  *ingest.Service
	st   *store.SQLiteStore
	def  *langWindowTranscriber
	cand *kkCandidate
	now  time.Time
	f    ingest.DiscoveredFile
}

func newGateWindowHarness(t *testing.T) *gateWindowHarness {
	t.Helper()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "mixed.m4a"), "fake-audio")
	h := &gateWindowHarness{
		st:   newRealStore(t),
		def:  &langWindowTranscriber{def: langReply{lang: "kk", conf: 0.9}},
		cand: &kkCandidate{failOn: map[int]bool{2: true}},
		now:  time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC),
		f:    ingest.DiscoveredFile{RelPath: "mixed.m4a", SizeBytes: 10, MTimeUnix: time.Now().Unix()},
	}
	h.svc = mustNewIngestService(t, config.Config{RootDir: root, StateDir: testutil.TempDir(t), STTProvider: "off"}, h.st)
	h.svc.SetLogger(log.New(&syncBuffer{}, "", 0))
	h.svc.SetTranscriber(h.def)
	h.svc.SetSTTIdentity("whisper", "large-v3")
	h.svc.SetLanguageScope("window")
	h.svc.SetRouteErrorFallback(true)
	h.svc.SetRouteRetryAfterForTest(24 * time.Hour)
	h.svc.SetRouteTranscriber("kk", h.cand, "kk-ckpt", nil)
	h.svc.SetClockForTest(func() time.Time { return h.now })
	h.svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) {
		return time.Duration(scopeTotalMS) * time.Millisecond, nil
	}
	h.svc.ExtractSegmentFunc = func(_ context.Context, _ string, startMS, endMS int) ([]byte, error) {
		return make([]byte, 1+(endMS-startMS)/1000), nil
	}
	return h
}

func (h *gateWindowHarness) scan(t *testing.T) string {
	t.Helper()
	if err := h.svc.ProcessDocument(context.Background(), h.f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	return transcriptMetaFor(t, h.st, h.f.RelPath)
}

// TestRouteFallbackReview_DroppedWindowIsRetriedOnTheNextScan (fix 1): under
// fail a window whose route fails again is dropped, and the transcript records
// it (route_fallback_dropped). The unchanged document is processed again on
// the next scan, so the route decodes the window again and, once it works,
// takes over.
func TestRouteFallbackReview_DroppedWindowIsRetriedOnTheNextScan(t *testing.T) {
	h := newGateWindowHarness(t)
	if meta := h.scan(t); !strings.Contains(meta, `"fallback_from":"kk-ckpt"`) {
		t.Fatalf("the first scan left no fallback window: %s", meta)
	}
	h.svc.SetRouteErrorFallback(false)
	h.cand.setBroken(true)
	if meta := h.scan(t); !strings.Contains(meta, `"route_fallback_dropped"`) {
		t.Fatalf("the drop under fail is not recorded: %s", meta)
	}
	calls := h.cand.count()
	if meta := h.scan(t); !strings.Contains(meta, `"route_fallback_dropped"`) || h.cand.count() != calls+1 {
		t.Fatalf("the unchanged document was not retried on the next scan (%d candidate calls): %s", h.cand.count()-calls, meta)
	}
	h.cand.setBroken(false)
	meta := h.scan(t)
	if strings.Contains(meta, "fallback") || h.cand.count() != calls+2 {
		t.Fatalf("the repaired checkpoint did not take over: %s", meta)
	}
}

// TestRouteFallbackReview_DroppedWindowReturnsUnderDefault (fix 1): after a
// drop under fail, a return to default restores the cached fallback text by
// the default rule: not due yet, so no decode, and the window is indexed
// again with its fallback record.
func TestRouteFallbackReview_DroppedWindowReturnsUnderDefault(t *testing.T) {
	h := newGateWindowHarness(t)
	h.scan(t)
	h.svc.SetRouteErrorFallback(false)
	h.cand.setBroken(true)
	h.scan(t)
	h.svc.SetRouteErrorFallback(true)
	h.now = h.now.Add(time.Hour)
	calls := h.cand.count()
	meta := h.scan(t)
	if h.cand.count() != calls {
		t.Fatalf("a fallback result that is not due was decoded under default")
	}
	if !strings.Contains(meta, `"fallback_from":"kk-ckpt"`) || strings.Contains(meta, "route_fallback_dropped") {
		t.Fatalf("the cached fallback window is not indexed again: %s", meta)
	}
}

// countingFallbackStore counts the §8.2.4 gate's store queries.
type countingFallbackStore struct {
	*store.SQLiteStore
	all, per atomic.Int32
}

func (c *countingFallbackStore) RouteFallbackTranscripts(ctx context.Context) ([]model.RouteFallbackRecord, error) {
	c.all.Add(1)
	return c.SQLiteStore.RouteFallbackTranscripts(ctx)
}

func (c *countingFallbackStore) RouteFallbackTranscriptsFor(ctx context.Context, relPath string) ([]model.RouteFallbackRecord, error) {
	c.per.Add(1)
	return c.SQLiteStore.RouteFallbackTranscriptsFor(ctx, relPath)
}

// TestRouteFallbackReview_GateCostsOneQueryPerScan (fix 2): a scan over
// unchanged documents with no fallback record asks the store once, not once
// per document, and a document that is not audio or video costs nothing.
func TestRouteFallbackReview_GateCostsOneQueryPerScan(t *testing.T) {
	ctx := context.Background()
	root := testutil.TempDir(t)
	for _, name := range []string{"a.mp3", "b.mp3", "c.mp3", "a.txt", "b.txt", "c.txt"} {
		writeFile(t, filepath.Join(root, name), "content of "+name)
	}
	st := &countingFallbackStore{SQLiteStore: newRealStore(t)}
	cfg := config.Default()
	cfg.RootDir, cfg.StateDir, cfg.STTProvider = root, testutil.TempDir(t), "off"
	svc := mustNewIngestService(t, cfg, st)
	svc.SetLogger(log.New(&syncBuffer{}, "", 0))
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] hello there"})
	svc.SetSTTIdentity("whisper", "large-v3")
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	st.all.Store(0)
	st.per.Store(0)
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if st.all.Load() != 1 || st.per.Load() != 0 {
		t.Fatalf("the second scan made %d corpus and %d per-document fallback queries, want 1 and 0", st.all.Load(), st.per.Load())
	}
	// Outside a scan a text document costs nothing, a media document one query.
	if svc.RouteFallbackStaleForTest(ctx, "a.txt") || st.per.Load() != 0 {
		t.Fatalf("a text document made %d per-document queries", st.per.Load())
	}
	if svc.RouteFallbackStaleForTest(ctx, "a.mp3") || st.per.Load() != 1 {
		t.Fatalf("a media document made %d per-document queries, want 1", st.per.Load())
	}
}

// TestRouteFallbackReview_CancelledItemRetryChangesNothing (fix 3): a run
// cancelled during the retry keeps the cache entry and its stamp, and retires
// nothing, even under fail.
func TestRouteFallbackReview_CancelledItemRetryChangesNothing(t *testing.T) {
	h := newFallbackItemHarness(t, "default")
	content := []byte(strings.Repeat("k", 4000))
	h.seedFallback(t, content)
	before := readRouteCache(t, h.state)
	h.svc.SetRouteErrorFallback(false)
	ctx, cancel := context.WithCancel(context.Background())
	h.stt.mu.Lock()
	h.stt.onKK = cancel
	h.stt.mu.Unlock()
	h.store.chunks = nil
	err := h.svc.GenerateTranscriptRepresentation(ctx, mediaDoc("talks/kk.m4a"), content)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
	if len(h.store.retired) != 0 {
		t.Errorf("a cancelled retry retired %v", h.store.retired)
	}
	if after := readRouteCache(t, h.state); after != before {
		t.Errorf("a cancelled retry changed the cache entry:\nbefore %s\nafter  %s", before, after)
	}
}

// TestRouteFallbackReview_CancelledWindowRetryChangesNothing (fix 3): the
// same for window scope: no window is dropped and the cache stays.
func TestRouteFallbackReview_CancelledWindowRetryChangesNothing(t *testing.T) {
	w := newWindowFallbackHarness(t)
	before := strings.Join(w.seed(t), "\n")
	coverageBefore := readCoverageCache(t, w.stateDir)
	w.svc.SetRouteErrorFallback(false)
	ctx, cancel := context.WithCancel(context.Background())
	w.cand.mu.Lock()
	w.cand.broken, w.cand.onCall = true, cancel
	w.cand.mu.Unlock()
	w.store.reps, w.store.chunks = nil, nil
	err := w.svc.GenerateTranscriptRepresentation(ctx, mediaDoc("talks/mixed.m4a"), w.content)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
	if len(w.store.reps) != 0 {
		t.Errorf("a cancelled retry indexed %d representations", len(w.store.reps))
	}
	if strings.Join(w.cachedLines(t), "\n") != before || readCoverageCache(t, w.stateDir) != coverageBefore {
		t.Errorf("a cancelled retry changed the cache entry")
	}
}

// TestRouteFallbackReview_ItemEnvironmentErrorChangesNothing (fix 4): a host
// that cannot extract the audio is not a route failure: under fail the item
// is not retired, no route is asked, and the cache stays.
func TestRouteFallbackReview_ItemEnvironmentErrorChangesNothing(t *testing.T) {
	h := newFallbackItemHarness(t, "default")
	extractFails := false
	h.svc.ExtractAudioTrackFunc = func(context.Context, string) ([]byte, error) {
		if extractFails {
			return nil, errors.New("ffmpeg not found")
		}
		return []byte(strings.Repeat("v", 4000)), nil
	}
	video := model.Document{DocID: 1, RelPath: "talks/kk.mp4", DocType: "video"}
	content := []byte(strings.Repeat("m", 4000))
	if err := h.svc.GenerateTranscriptRepresentation(context.Background(), video, content); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before := readRouteCache(t, h.state)
	h.svc.SetRouteErrorFallback(false)
	extractFails = true
	requests := h.stt.count()
	err := h.svc.GenerateTranscriptRepresentation(context.Background(), video, content)
	if err == nil || !strings.Contains(err.Error(), "could not run") {
		t.Fatalf("err = %v, want the retry reported as not run", err)
	}
	if h.stt.count() != requests || len(h.store.retired) != 0 {
		t.Errorf("a host error asked the route (%d requests) or retired %v", h.stt.count()-requests, h.store.retired)
	}
	if after := readRouteCache(t, h.state); after != before {
		t.Errorf("a host error changed the cache entry")
	}
}

// TestRouteFallbackReview_WindowEnvironmentErrorsChangeNothing (fix 4): a cut
// that fails, or a rebuilt schedule that does not match the cached record
// (another duration), is not a route failure: under fail no window is dropped,
// no decode runs, and the cache stays.
func TestRouteFallbackReview_WindowEnvironmentErrorsChangeNothing(t *testing.T) {
	for name, breakHost := range map[string]func(w *windowFallbackHarness){
		"cut fails": func(w *windowFallbackHarness) {
			w.mu.Lock()
			w.failCut = map[int]error{win2StartMS: errors.New("ffmpeg not found")}
			w.mu.Unlock()
		},
		"schedule mismatch": func(w *windowFallbackHarness) {
			w.svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 40 * time.Minute, nil }
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWindowFallbackHarness(t)
			before := strings.Join(w.seed(t), "\n")
			coverageBefore := readCoverageCache(t, w.stateDir)
			w.svc.SetRouteErrorFallback(false)
			breakHost(w)
			calls := w.cand.count() + w.def.callCount()
			if _, err := w.run(t); err == nil || !strings.Contains(err.Error(), "could not run") {
				t.Fatalf("err = %v, want the retry reported as not run", err)
			}
			if w.cand.count()+w.def.callCount() != calls {
				t.Errorf("a host error decoded a window")
			}
			if strings.Join(w.cachedLines(t), "\n") != before || readCoverageCache(t, w.stateDir) != coverageBefore {
				t.Errorf("a host error changed the cache entry")
			}
		})
	}
}

// TestRouteFallbackReview_DerivationPassNeverRetries (fix 5): the derivation
// pass reads the cached transcript without the retry, so a due fallback
// result (route_retry_after 0) is not decoded a second time in one run.
func TestRouteFallbackReview_DerivationPassNeverRetries(t *testing.T) {
	h := newFallbackItemHarness(t, "default")
	h.svc.SetRouteRetryAfterForTest(0)
	content := []byte(strings.Repeat("n", 4000))
	h.seedFallback(t, content)
	requests := h.stt.count()
	_ = h.svc.DeriveTranscriptTranslationsForTest(context.Background(), mediaDoc("talks/kk.m4a"), content)
	if got := h.stt.since(requests); len(got) != 0 {
		t.Fatalf("the derivation pass made STT requests %v", got)
	}
	h.mustRun(t, content)
	if got := h.stt.since(requests); len(got) != 1 {
		t.Errorf("the transcription pass made %v, want one retry", got)
	}
}

// TestRouteFallbackReview_StampOnlyRetryKeepsTheText (fix 8): a failed retry
// that only moves route_fallback_at writes the route sidecar, not the text.
func TestRouteFallbackReview_StampOnlyRetryKeepsTheText(t *testing.T) {
	h := newFallbackItemHarness(t, "default")
	content := []byte(strings.Repeat("p", 4000))
	h.seedFallback(t, content)
	texts, _ := filepath.Glob(filepath.Join(h.state, "cache", "transcribe", "*.txt"))
	if len(texts) != 1 {
		t.Fatalf("text cache files = %v", texts)
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(texts[0], old, old); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(25 * time.Hour)
	meta, _ := h.mustRun(t, content)
	if meta.RouteFallbackAt != "2026-10-19T10:00:00Z" {
		t.Fatalf("route_fallback_at = %q, want the retry time", meta.RouteFallbackAt)
	}
	info, err := os.Stat(texts[0])
	if err != nil || !info.ModTime().Equal(old) {
		t.Errorf("the text cache was rewritten for a stamp-only change (%v, %v)", info.ModTime(), err)
	}
	if !strings.Contains(readRouteCache(t, h.state), `"route_fallback_at":"2026-10-19T10:00:00Z"`) {
		t.Errorf("the route sidecar lacks the new stamp")
	}
}

func readRouteCache(t *testing.T, state string) string {
	t.Helper()
	return readOneCacheFile(t, state, "*.route.json")
}

func readCoverageCache(t *testing.T, state string) string {
	t.Helper()
	return readOneCacheFile(t, state, "*.coverage.json")
}

func readOneCacheFile(t *testing.T, state, pattern string) string {
	t.Helper()
	paths, _ := filepath.Glob(filepath.Join(state, "cache", "transcribe", pattern))
	if len(paths) != 1 {
		t.Fatalf("cache files %s = %v, want one", pattern, paths)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

package tests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §8.2.4 (spec 0.82.0, dir2mcp #1122), window scope. The 30-minute
// recording is four windows (0, 590, 1180 and 1770 s). The default profile
// reports Kazakh, so every window routes to the kk-ckpt candidate. The
// candidate fails on its second call, which is window 2, so window 2 is
// decoded by the default profile and records fallback_from: kk-ckpt. Windows
// 1, 3 and 4 are route decodes.

// kkCandidate is a fake route candidate that reports Kazakh. It fails on the
// call ordinals in failOn, and on every call while broken.
type kkCandidate struct {
	mu     sync.Mutex
	calls  int
	failOn map[int]bool
	broken bool
	// onCall runs on each call, before the answer.
	onCall func()
}

func (c *kkCandidate) Transcribe(ctx context.Context, relPath string, data []byte) (string, error) {
	res, err := c.TranscribeStructured(ctx, relPath, data)
	return res.Text, err
}

func (c *kkCandidate) TranscribeStructured(context.Context, string, []byte) (model.TranscriptResult, error) {
	c.mu.Lock()
	c.calls++
	n, fail, onCall := c.calls, c.broken || c.failOn[c.calls], c.onCall
	c.mu.Unlock()
	if onCall != nil {
		onCall()
	}
	if fail {
		return model.TranscriptResult{}, errors.New("checkpoint not loaded")
	}
	text := fmt.Sprintf("[00:00] candidate line %d spoken at some length\n[00:30] candidate tail %d spoken at some length", n, n)
	return model.TranscriptResult{Text: text, Language: "kk", LanguageConfidence: 0.9}, nil
}

func (c *kkCandidate) MaxTranscribePayloadBytes() int { return 0 }

func (c *kkCandidate) setBroken(b bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.broken = b
}

func (c *kkCandidate) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// windowFallbackMeta is the §8.2.4 part of a window-scoped meta_json.
type windowFallbackMeta struct {
	RouteFallbackAt      string    `json:"route_fallback_at"`
	RouteFallbackBy      string    `json:"route_fallback_by"`
	RouteFallbackDropped []covLang `json:"route_fallback_dropped"`
	Coverage             *struct {
		WindowsDecoded int `json:"windows_decoded"`
		DecodedMS      int `json:"decoded_ms"`
		Ranges         []struct {
			StartMS int `json:"start_ms"`
			EndMS   int `json:"end_ms"`
		} `json:"ranges"`
		Languages []covLang `json:"languages"`
	} `json:"coverage"`
}

type windowFallbackHarness struct {
	*windowSTTHarness
	def     *langWindowTranscriber
	cand    *kkCandidate
	content []byte
	now     time.Time
}

func newWindowFallbackHarness(t *testing.T) *windowFallbackHarness {
	t.Helper()
	def := &langWindowTranscriber{def: langReply{lang: "kk", conf: 0.9}}
	h, content := newScopedHarness(t, def, scopeTotalMS)
	w := &windowFallbackHarness{windowSTTHarness: h, def: def, cand: &kkCandidate{failOn: map[int]bool{2: true}}, content: content,
		now: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)}
	h.svc.SetRouteErrorFallback(true)
	h.svc.SetRouteRetryAfterForTest(24 * time.Hour)
	h.svc.SetRouteTranscriber("kk", w.cand, "kk-ckpt", nil)
	h.svc.SetClockForTest(func() time.Time { return w.now })
	return w
}

// run transcribes the recording into an empty store and returns the meta.
func (w *windowFallbackHarness) run(t *testing.T) (windowFallbackMeta, error) {
	t.Helper()
	w.store.reps, w.store.chunks = nil, nil
	err := w.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/mixed.m4a"), w.content)
	if err != nil {
		return windowFallbackMeta{}, err
	}
	if len(w.store.reps) != 1 {
		t.Fatalf("persisted %d representations, want 1\nlogs:\n%s", len(w.store.reps), w.logs.String())
	}
	var m windowFallbackMeta
	if err := json.Unmarshal([]byte(w.store.reps[0].MetaJSON), &m); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	return m, nil
}

func (w *windowFallbackHarness) mustRun(t *testing.T) windowFallbackMeta {
	t.Helper()
	m, err := w.run(t)
	if err != nil {
		t.Fatalf("transcription failed: %v\nlogs:\n%s", err, w.logs.String())
	}
	return m
}

// cachedLines is the cached merged transcript, one line per segment.
func (w *windowFallbackHarness) cachedLines(t *testing.T) []string {
	t.Helper()
	paths, _ := filepath.Glob(filepath.Join(w.stateDir, "cache", "transcribe", "*.txt"))
	if len(paths) != 1 {
		t.Fatalf("transcript cache files = %v, want one", paths)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(raw), "\n")
}

// seed makes the first decode and checks the fallback window.
func (w *windowFallbackHarness) seed(t *testing.T) []string {
	t.Helper()
	m := w.mustRun(t)
	assertLangs(t, m.Coverage.Languages, []covLang{
		{StartMS: 0, EndMS: win2StartMS, Language: "kk", LanguageSource: "detected", LanguageConfidence: f64(0.9), Route: "kk-ckpt", Covered: true},
		{StartMS: win2StartMS, EndMS: win3StartMS, Language: "kk", LanguageSource: "detected", LanguageConfidence: f64(0.9), Route: "whisper", Covered: true},
		{StartMS: win3StartMS, EndMS: scopeTotalMS, Language: "kk", LanguageSource: "detected", LanguageConfidence: f64(0.9), Route: "kk-ckpt", Covered: true},
	})
	if m.Coverage.Languages[1].FallbackFrom != "kk-ckpt" {
		t.Fatalf("window 2 entry %+v: want fallback_from kk-ckpt", m.Coverage.Languages[1])
	}
	if m.RouteFallbackAt != "2026-10-18T09:00:00Z" || m.RouteFallbackBy != "whisper|large-v3" {
		t.Fatalf("stamp = %q / %q, want 2026-10-18T09:00:00Z / whisper|large-v3", m.RouteFallbackAt, m.RouteFallbackBy)
	}
	return w.cachedLines(t)
}

// window2Lines are the cached lines inside window 2's core [590 s, 1180 s).
func isWindow2Line(line string) bool {
	return strings.HasPrefix(line, "[09:50]") || strings.HasPrefix(line, "[10:20]")
}

func withoutWindow2(lines []string) []string {
	var out []string
	for _, l := range lines {
		if !isWindow2Line(l) {
			out = append(out, l)
		}
	}
	return out
}

func (w *windowFallbackHarness) cutsSince(n int) []sttWindowCut {
	return w.windows()[n:]
}

// TestRouteFallbackWindow_StampRestoredOnCacheHit: a cache hit that is not
// due restores the coverage, its fallback entry and the stamp, and decodes
// nothing.
func TestRouteFallbackWindow_StampRestoredOnCacheHit(t *testing.T) {
	w := newWindowFallbackHarness(t)
	w.seed(t)
	w.now = w.now.Add(time.Hour)
	defCalls, candCalls := w.def.callCount(), w.cand.count()
	m := w.mustRun(t)
	if w.def.callCount() != defCalls || w.cand.count() != candCalls {
		t.Fatalf("a fallback result that is not due was decoded again")
	}
	if m.RouteFallbackAt != "2026-10-18T09:00:00Z" || m.RouteFallbackBy != "whisper|large-v3" || m.Coverage.Languages[1].FallbackFrom != "kk-ckpt" {
		t.Errorf("cache hit meta = %+v, want the stamp and the fallback entry restored", m)
	}
}

// TestRouteFallbackWindow_RetryRedecodesOnlyTheFallbackWindow: a due retry
// cuts and decodes window 2 only, on the candidate, and splices its text into
// the cached transcript in place. The other windows keep their text and
// entries, and the stamp goes with the last fallback window.
func TestRouteFallbackWindow_RetryRedecodesOnlyTheFallbackWindow(t *testing.T) {
	w := newWindowFallbackHarness(t)
	before := w.seed(t)
	w.now = w.now.Add(24 * time.Hour)
	defCalls, candCalls, cuts := w.def.callCount(), w.cand.count(), len(w.windows())

	m := w.mustRun(t)
	if got := w.cutsSince(cuts); len(got) != 1 || got[0].startMS != win2StartMS || got[0].endMS != win2StartMS+scopeWinMS {
		t.Fatalf("retry cut %+v, want only window 2 [%d,%d)", got, win2StartMS, win2StartMS+scopeWinMS)
	}
	if w.def.callCount() != defCalls || w.cand.count() != candCalls+1 {
		t.Fatalf("retry made %d default and %d candidate calls, want 0 and 1", w.def.callCount()-defCalls, w.cand.count()-candCalls)
	}
	assertLangs(t, m.Coverage.Languages, []covLang{
		{StartMS: 0, EndMS: scopeTotalMS, Language: "kk", LanguageSource: "detected", LanguageConfidence: f64(0.9), Route: "kk-ckpt", Covered: true},
	})
	if m.RouteFallbackAt != "" || m.RouteFallbackBy != "" {
		t.Errorf("stamp = %q / %q, want none: no fallback window remains", m.RouteFallbackAt, m.RouteFallbackBy)
	}
	after := w.cachedLines(t)
	if strings.Join(withoutWindow2(after), "\n") != strings.Join(withoutWindow2(before), "\n") {
		t.Errorf("the other windows changed:\nbefore %q\nafter  %q", withoutWindow2(before), withoutWindow2(after))
	}
	want := []string{"[09:50] candidate line 5 spoken at some length", "[10:20] candidate tail 5 spoken at some length"}
	var got []string
	for _, l := range after {
		if isWindow2Line(l) {
			got = append(got, l)
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("window 2 lines = %q, want %q", got, want)
	}
	if strings.Join(after, "\n") == strings.Join(before, "\n") || !strings.HasPrefix(after[0], "[00:00] candidate line 1") {
		t.Errorf("the splice is out of place: %q", after)
	}
	if !strings.Contains(w.logs.String(), "route fallback retry talks/mixed.m4a: candidate kk-ckpt, 1 window(s) retried: the route recovered every window") {
		t.Errorf("no one-line retry log:\n%s", w.logs.String())
	}
}

// TestRouteFallbackWindow_FailedRetryKeepsTextAndMovesTheTime: under default a
// window whose route fails again keeps its cached fallback text and entry, the
// default profile is not asked again, and route_fallback_at moves to now.
func TestRouteFallbackWindow_FailedRetryKeepsTextAndMovesTheTime(t *testing.T) {
	w := newWindowFallbackHarness(t)
	before := w.seed(t)
	w.cand.setBroken(true)
	w.now = w.now.Add(30 * time.Hour)
	defCalls := w.def.callCount()

	m := w.mustRun(t)
	if w.def.callCount() != defCalls {
		t.Fatalf("the default profile decoded again after a failed retry")
	}
	if m.Coverage.Languages[1].FallbackFrom != "kk-ckpt" || m.RouteFallbackAt != "2026-10-19T15:00:00Z" || m.RouteFallbackBy != "whisper|large-v3" {
		t.Errorf("meta = %+v, want the fallback entry kept and route_fallback_at 2026-10-19T15:00:00Z", m)
	}
	if strings.Join(w.cachedLines(t), "\n") != strings.Join(before, "\n") {
		t.Errorf("the cached text changed after a failed retry")
	}
	candCalls := w.cand.count()
	w.now = w.now.Add(time.Hour)
	w.mustRun(t)
	if w.cand.count() != candCalls {
		t.Errorf("one hour after a failed retry the window was decoded again")
	}
}

// TestRouteFallbackWindow_PolicyFailDropsAFailedWindow: under fail a fallback
// window whose route fails again is a failed window: its text, range and entry
// leave the indexed transcript and no stamp remains. The cache entry is not
// purged, so a return to default costs no decode until the result is due.
func TestRouteFallbackWindow_PolicyFailDropsAFailedWindow(t *testing.T) {
	w := newWindowFallbackHarness(t)
	before := w.seed(t)
	w.cand.setBroken(true)
	w.svc.SetRouteErrorFallback(false)
	defCalls := w.def.callCount()

	m := w.mustRun(t)
	if w.def.callCount() != defCalls {
		t.Fatalf("the default profile decoded under fail")
	}
	assertWindow2Failed(t, w, m)
	if strings.Join(w.cachedLines(t), "\n") != strings.Join(before, "\n") {
		t.Errorf("the cache entry changed under fail; it must keep the fallback text")
	}

	w.svc.SetRouteErrorFallback(true)
	candCalls := w.cand.count()
	back := w.mustRun(t)
	if w.cand.count() != candCalls || back.Coverage.Languages[1].FallbackFrom != "kk-ckpt" {
		t.Errorf("after the return to default the fallback result was not served from the cache: %+v", back)
	}
}

// assertWindow2Failed checks that window 2 is a failed window in the indexed
// transcript: no text, no range, no entry, and no stamp.
func assertWindow2Failed(t *testing.T, w *windowFallbackHarness, m windowFallbackMeta) {
	t.Helper()
	if m.Coverage.WindowsDecoded != 3 || len(m.Coverage.Ranges) != 2 || m.Coverage.Ranges[0].EndMS != win2StartMS || m.Coverage.Ranges[1].StartMS != win3StartMS {
		t.Errorf("coverage = %+v, want window 2 as a failed window: 3 decoded, a gap [%d,%d)", m.Coverage, win2StartMS, win3StartMS)
	}
	for _, e := range m.Coverage.Languages {
		if e.FallbackFrom != "" || (e.StartMS < win3StartMS && e.EndMS > win2StartMS) {
			t.Errorf("entry %+v survives in the failed window", e)
		}
	}
	assertWindow2DropRecorded(t, m)
	if text := persistedText(w.windowSTTHarness); strings.Contains(text, "decode 2") {
		t.Errorf("the fallback text is still indexed: %q", text)
	}
}

// assertWindow2DropRecorded checks the dropped-window record: the cached
// fallback text is kept, so the transcript records the dropped window with the
// cached stamp and the gate keeps processing it.
func assertWindow2DropRecorded(t *testing.T, m windowFallbackMeta) {
	t.Helper()
	if m.RouteFallbackAt != "2026-10-18T09:00:00Z" || m.RouteFallbackBy != "whisper|large-v3" {
		t.Errorf("stamp = %q / %q, want the cached stamp kept with the dropped window", m.RouteFallbackAt, m.RouteFallbackBy)
	}
	if len(m.RouteFallbackDropped) != 1 || m.RouteFallbackDropped[0].StartMS != win2StartMS || m.RouteFallbackDropped[0].EndMS != win3StartMS || m.RouteFallbackDropped[0].FallbackFrom != "kk-ckpt" {
		t.Errorf("route_fallback_dropped = %+v, want window 2 from kk-ckpt", m.RouteFallbackDropped)
	}
}

// TestRouteFallbackWindow_ProducerChangeIsStaleAtOnce: a cached
// route_fallback_by that names another default profile makes the result stale
// on the next run. The route fails, so the current default decodes window 2
// once and the stamp names it.
func TestRouteFallbackWindow_ProducerChangeIsStaleAtOnce(t *testing.T) {
	w := newWindowFallbackHarness(t)
	w.seed(t)
	paths, _ := filepath.Glob(filepath.Join(w.stateDir, "cache", "transcribe", "*.coverage.json"))
	if len(paths) != 1 {
		t.Fatalf("coverage cache files = %v, want one", paths)
	}
	raw, _ := os.ReadFile(paths[0])
	if !strings.Contains(string(raw), `"route_fallback_by":"whisper|large-v3"`) {
		t.Fatalf("coverage cache %s lacks the stamp", raw)
	}
	edited := strings.Replace(string(raw), `"route_fallback_by":"whisper|large-v3"`, `"route_fallback_by":"whisper|old-model"`, 1)
	if err := os.WriteFile(paths[0], []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	w.cand.setBroken(true)
	w.now = w.now.Add(time.Hour)
	defCalls := w.def.callCount()

	m := w.mustRun(t)
	if w.def.callCount() != defCalls+1 {
		t.Fatalf("the current default decoded %d times, want once", w.def.callCount()-defCalls)
	}
	if m.Coverage.Languages[1].FallbackFrom != "kk-ckpt" || m.Coverage.Languages[1].Route != "whisper" || m.RouteFallbackBy != "whisper|large-v3" || m.RouteFallbackAt != "2026-10-18T10:00:00Z" {
		t.Errorf("meta = %+v, want window 2 decoded by the current default and the stamp naming it", m)
	}
}

// TestRouteFallbackWindow_GateReprocessesOnlyWhenDue drives the §8.6.7 gate
// end to end on a real store: an unchanged document whose transcript holds a
// fallback result that is not due is not processed again; once it is due, the
// scan processes it and the route's text replaces the fallback window.
func TestRouteFallbackWindow_GateReprocessesOnlyWhenDue(t *testing.T) {
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "mixed.m4a"), "fake-audio")
	st := newRealStore(t)
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	def := &langWindowTranscriber{def: langReply{lang: "kk", conf: 0.9}}
	cand := &kkCandidate{failOn: map[int]bool{2: true}}
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: testutil.TempDir(t), STTProvider: "off"}, st)
	svc.SetLogger(log.New(&syncBuffer{}, "", 0))
	svc.SetTranscriber(def)
	svc.SetSTTIdentity("whisper", "large-v3")
	svc.SetLanguageScope("window")
	svc.SetRouteErrorFallback(true)
	svc.SetRouteRetryAfterForTest(24 * time.Hour)
	svc.SetRouteTranscriber("kk", cand, "kk-ckpt", nil)
	svc.SetClockForTest(func() time.Time { return now })
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) {
		return time.Duration(scopeTotalMS) * time.Millisecond, nil
	}
	svc.ExtractSegmentFunc = func(_ context.Context, _ string, startMS, endMS int) ([]byte, error) {
		return make([]byte, 1+(endMS-startMS)/1000), nil
	}
	f := ingest.DiscoveredFile{RelPath: "mixed.m4a", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	scan := func() {
		t.Helper()
		if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
			t.Fatalf("ProcessDocument: %v", err)
		}
	}

	scan()
	if !strings.Contains(transcriptMetaFor(t, st, "mixed.m4a"), `"fallback_from":"kk-ckpt"`) {
		t.Fatalf("the first scan left no fallback window: %s", transcriptMetaFor(t, st, "mixed.m4a"))
	}
	cand.setBroken(false)
	now = now.Add(time.Hour)
	calls := cand.count()
	scan()
	if cand.count() != calls {
		t.Fatalf("a fallback result that is not due was processed again")
	}
	now = now.Add(24 * time.Hour)
	scan()
	if cand.count() != calls+1 {
		t.Fatalf("a due fallback result made %d candidate calls, want 1", cand.count()-calls)
	}
	if meta := transcriptMetaFor(t, st, "mixed.m4a"); strings.Contains(meta, "fallback_from") || strings.Contains(meta, "route_fallback_at") {
		t.Errorf("the recovered transcript still records a fallback: %s", meta)
	}
}

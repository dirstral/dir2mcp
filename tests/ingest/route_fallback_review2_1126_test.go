package tests

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §8.2.4 (spec 0.82.0, dir2mcp #1122): the second review of PR #1126. A
// retry the host cannot run keeps a document indexed under default; the
// rebuilt schedule tolerates a rounding change and split pieces; the gate
// uses the stored document type; a window a changed producer left with no
// text is retried when due.

// skipMeta is the §8.2.4 skip record on a transcript meta_json.
type skipMeta struct {
	RouteFallbackFrom          string       `json:"route_fallback_from"`
	RouteFallbackAt            string       `json:"route_fallback_at"`
	RouteFallbackSkipReason    string       `json:"route_fallback_skip_reason"`
	RouteFallbackDropped       []covLang    `json:"route_fallback_dropped"`
	RouteFallbackDroppedCached []covRefused `json:"route_fallback_dropped_cached"`
}

func decodeSkipMeta(t *testing.T, raw string) skipMeta {
	t.Helper()
	var m skipMeta
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	return m
}

// newVideoFallbackHarness seeds an item-scope fallback result for a video
// whose audio extraction the test can break.
func newVideoFallbackHarness(t *testing.T, extractFails *bool) (*fallbackItemHarness, model.Document, []byte) {
	t.Helper()
	h := newFallbackItemHarness(t, "default")
	h.svc.ExtractAudioTrackFunc = func(context.Context, string) ([]byte, error) {
		if *extractFails {
			return nil, errors.New("ffmpeg not found")
		}
		return []byte(strings.Repeat("v", 4000)), nil
	}
	video := model.Document{DocID: 1, RelPath: "talks/kk.mp4", DocType: "video"}
	content := []byte(strings.Repeat("m", 4000))
	if err := h.svc.GenerateTranscriptRepresentation(context.Background(), video, content); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return h, video, content
}

// TestRouteFallbackReview2_ItemRetryNotRunUnderDefault (A): a host that cannot
// extract the audio is not a route failure. Under default the cached fallback
// text stays indexed, no route is asked, route_fallback_at moves to now and
// the skip reason is recorded.
func TestRouteFallbackReview2_ItemRetryNotRunUnderDefault(t *testing.T) {
	extractFails := false
	h, video, content := newVideoFallbackHarness(t, &extractFails)
	extractFails = true
	h.now = h.now.Add(25 * time.Hour)
	requests := h.stt.count()
	h.store.reps, h.store.chunks = nil, nil
	if err := h.svc.GenerateTranscriptRepresentation(context.Background(), video, content); err != nil {
		t.Fatalf("a retry that could not run failed the document: %v", err)
	}
	if h.stt.count() != requests || len(h.store.reps) != 1 {
		t.Fatalf("requests %d, reps %d: want no request and the transcript indexed", h.stt.count()-requests, len(h.store.reps))
	}
	m := decodeSkipMeta(t, h.store.reps[0].MetaJSON)
	if m.RouteFallbackFrom != "ckpt-kk" || m.RouteFallbackAt != "2026-10-19T10:00:00Z" || m.RouteFallbackSkipReason != "audio_extract_failed" {
		t.Errorf("meta = %+v, want the fallback kept, the stamp moved to now and the skip reason", m)
	}
}

// TestRouteFallbackReview2_ItemRetryNotRunUnderFail (A): under fail the spec
// does not allow serving fallback text, so a retry that could not run fails
// the item and retires its text, as a route failure does.
func TestRouteFallbackReview2_ItemRetryNotRunUnderFail(t *testing.T) {
	extractFails := false
	h, video, content := newVideoFallbackHarness(t, &extractFails)
	extractFails = true
	h.svc.SetRouteErrorFallback(false)
	err := h.svc.GenerateTranscriptRepresentation(context.Background(), video, content)
	if err == nil || !errors.Is(err, ingest.ErrTranscriptProviderFailure) {
		t.Fatalf("err = %v, want TRANSCRIBE_FAILED", err)
	}
	if len(h.store.retired) != 1 {
		t.Errorf("retired %v, want the fallback transcript retired", h.store.retired)
	}
}

// TestRouteFallbackReview2_WindowRetryNotRun (A, B): a cut that fails, or a
// recording length beyond the tolerance of the cached one, means the retry
// cannot run. Under default the cached fallback window stays indexed with the
// stamp moved and the reason; under fail it leaves the transcript, recorded as
// a dropped window whose text the cache keeps. No window is decoded.
func TestRouteFallbackReview2_WindowRetryNotRun(t *testing.T) {
	for name, breakHost := range map[string]func(w *windowFallbackHarness){
		"cut fails": func(w *windowFallbackHarness) {
			w.mu.Lock()
			w.failCut = map[int]error{win2StartMS: errors.New("ffmpeg not found")}
			w.mu.Unlock()
		},
		"length beyond the tolerance": func(w *windowFallbackHarness) {
			w.svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 40 * time.Minute, nil }
		},
	} {
		for _, policy := range []string{"default", "fail"} {
			t.Run(name+"/"+policy, func(t *testing.T) {
				w := newWindowFallbackHarness(t)
				before := strings.Join(w.seed(t), "\n")
				w.svc.SetRouteErrorFallback(policy == "default")
				breakHost(w)
				w.now = w.now.Add(25 * time.Hour)
				calls := w.cand.count() + w.def.callCount()
				m, err := w.run(t)
				if err != nil {
					t.Fatalf("a retry that could not run failed the document: %v", err)
				}
				if w.cand.count()+w.def.callCount() != calls {
					t.Errorf("a host error decoded a window")
				}
				if strings.Join(w.cachedLines(t), "\n") != before {
					t.Errorf("a host error changed the cached text")
				}
				assertRetryNotRun(t, w, m, policy)
			})
		}
	}
}

func assertRetryNotRun(t *testing.T, w *windowFallbackHarness, m windowFallbackMeta, policy string) {
	t.Helper()
	sm := decodeSkipMeta(t, w.store.reps[0].MetaJSON)
	if sm.RouteFallbackSkipReason != "cut_failed" && sm.RouteFallbackSkipReason != "schedule_mismatch" {
		t.Errorf("route_fallback_skip_reason = %q, want a short class", sm.RouteFallbackSkipReason)
	}
	text := persistedText(w.windowSTTHarness)
	if policy == "default" {
		if !strings.Contains(text, "decode 2") || m.RouteFallbackAt != "2026-10-19T10:00:00Z" {
			t.Errorf("under default: text %q, stamp %q: want the cached window indexed and the stamp moved", text, m.RouteFallbackAt)
		}
		return
	}
	if strings.Contains(text, "decode 2") || len(sm.RouteFallbackDroppedCached) != 1 || len(sm.RouteFallbackDropped) != 1 {
		t.Errorf("under fail: text %q, meta %+v: want the window dropped and recorded with cached text", text, sm)
	}
}

// TestRouteFallbackReview2_RoundingChangeStillRetries (B): a probed length
// that differs from the cached one by a rounding amount keeps the cached
// schedule, so the retry runs and recovers.
func TestRouteFallbackReview2_RoundingChangeStillRetries(t *testing.T) {
	w := newWindowFallbackHarness(t)
	w.seed(t)
	w.svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) {
		return time.Duration(scopeTotalMS+400) * time.Millisecond, nil
	}
	w.now = w.now.Add(25 * time.Hour)
	m := w.mustRun(t)
	if coverageHasFallbackEntry(m) || m.RouteFallbackAt != "" {
		t.Errorf("meta = %+v, want the window recovered", m)
	}
}

func coverageHasFallbackEntry(m windowFallbackMeta) bool {
	for _, e := range m.Coverage.Languages {
		if e.FallbackFrom != "" {
			return true
		}
	}
	return false
}

// cappedLang is the language-reporting fake with a payload cap, so a window
// is split into pieces.
type cappedLang struct {
	*langWindowTranscriber
	limit int
}

func (c *cappedLang) MaxTranscribePayloadBytes() int { return c.limit }

// TestRouteFallbackReview2_SplitPieceWindowRetries (B): a window the payload
// cap split into two pieces is retried as two pieces and spliced in place.
func TestRouteFallbackReview2_SplitPieceWindowRetries(t *testing.T) {
	w := newWindowFallbackHarness(t)
	w.svc.SetTranscriber(&cappedLang{langWindowTranscriber: w.def, limit: 150_000})
	w.mu.Lock()
	w.oversize = 2
	w.mu.Unlock()
	w.cand.mu.Lock()
	w.cand.failOn = map[int]bool{3: true, 4: true}
	w.cand.mu.Unlock()
	w.mustRun(t)
	before := w.cachedLines(t)
	assertDeriveDropsSplitWindow(t, w)
	w.now = w.now.Add(25 * time.Hour)
	calls := w.cand.count()
	m := w.mustRun(t)
	if w.cand.count() != calls+2 || coverageHasFallbackEntry(m) {
		t.Fatalf("retry made %d candidate calls (meta %+v), want 2 and the window recovered", w.cand.count()-calls, m)
	}
	keepBefore, _ := splitWindow2Lines(before)
	keepAfter, retried := splitWindow2Lines(w.cachedLines(t))
	if strings.Join(keepBefore, "\n") != strings.Join(keepAfter, "\n") {
		t.Errorf("the other windows changed:\nbefore %q\nafter  %q", keepBefore, keepAfter)
	}
	if len(retried) != 4 || !strings.Contains(retried[0], "candidate line") || !strings.Contains(retried[2], "candidate line") {
		t.Errorf("window 2 lines = %q, want both pieces from the candidate", retried)
	}
}

// assertDeriveDropsSplitWindow (F): under fail the derivation pass reads the
// transcript without window 2, cut on the same cores the served splice uses,
// so both of its pieces go.
func assertDeriveDropsSplitWindow(t *testing.T, w *windowFallbackHarness) {
	t.Helper()
	w.svc.SetRouteErrorFallback(false)
	defer w.svc.SetRouteErrorFallback(true)
	derived, err := w.svc.DeriveReadTranscriptForTest(context.Background(), mediaDoc("talks/mixed.m4a"), w.content)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(derived, "[09:50]") || strings.Contains(derived, "[14:50]") || !strings.Contains(derived, "[00:00]") || !strings.Contains(derived, "[19:40]") {
		t.Errorf("the derivation read %q, want every line but window 2's two pieces", derived)
	}
}

// splitWindow2Lines separates the lines of window 2's two pieces from the rest.
func splitWindow2Lines(lines []string) (others, window2 []string) {
	for _, l := range lines {
		in := false
		for _, p := range []string{"[09:50]", "[10:20]", "[14:50]", "[15:20]"} {
			if strings.HasPrefix(l, p) {
				in = true
			}
		}
		if in {
			window2 = append(window2, l)
		} else {
			others = append(others, l)
		}
	}
	return others, window2
}

// TestRouteFallbackReview2_GateUsesTheStoredDocType (E): outside a scan the
// gate asks about a document by the type the store records, so a sniffed
// audio file under an unusual extension is not skipped.
func TestRouteFallbackReview2_GateUsesTheStoredDocType(t *testing.T) {
	ctx := context.Background()
	st := newRealStore(t)
	if err := st.UpsertDocument(ctx, model.Document{RelPath: "rec.bin", DocType: "audio", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	doc, err := st.GetDocumentByPath(ctx, "rec.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertRepresentation(ctx, model.Representation{DocID: doc.DocID, RepType: ingest.RepTypeTranscript, RepHash: "h",
		MetaJSON: `{"source":"stt","route_fallback_from":"ckpt-kk","route_fallback_at":"2026-10-17T08:00:00Z","route_fallback_by":"whisper|large-v3"}`}); err != nil {
		t.Fatal(err)
	}
	svc := mustNewIngestService(t, config.Config{StateDir: testutil.TempDir(t), STTProvider: "off"}, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] x"})
	svc.SetSTTIdentity("whisper", "large-v3")
	svc.SetRouteErrorFallback(true)
	svc.SetRouteRetryAfterForTest(24 * time.Hour)
	svc.SetClockForTest(func() time.Time { return time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC) })
	if !svc.RouteFallbackStaleForTest(ctx, "rec.bin", "audio") {
		t.Fatal("a due fallback on a stored audio document under .bin is not stale")
	}
}

// TestRouteFallbackReview2_ProducerChangeDropIsRetriedWhenDue (G): after a
// producer change, the route and the new default both fail, so the window has
// no valid text left. It is recorded as dropped (in the transcript and the
// cache) and retried on its route when due, then spliced back in.
func TestRouteFallbackReview2_ProducerChangeDropIsRetriedWhenDue(t *testing.T) {
	w := newWindowFallbackHarness(t)
	w.seed(t)
	path := setCoverageCacheProducer(t, w.stateDir, "whisper|old-model")
	w.cand.setBroken(true)
	w.def.mu.Lock()
	w.def.def = langReply{err: errors.New("default down")}
	w.def.mu.Unlock()
	w.now = w.now.Add(time.Hour)
	w.mustRun(t)
	sm := decodeSkipMeta(t, w.store.reps[0].MetaJSON)
	if len(sm.RouteFallbackDropped) != 1 || len(sm.RouteFallbackDroppedCached) != 0 || sm.RouteFallbackAt != "2026-10-18T10:00:00Z" {
		t.Fatalf("meta = %+v, want window 2 dropped with no cached text at the retry time", sm)
	}
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), "route_fallback_dropped") {
		t.Fatalf("the cache does not record the dropped window: %s", raw)
	}
	w.now = w.now.Add(time.Hour)
	calls := w.cand.count()
	w.mustRun(t)
	if w.cand.count() != calls {
		t.Fatalf("a dropped window that is not due was decoded")
	}
	w.cand.setBroken(false)
	w.now = w.now.Add(24 * time.Hour)
	m := w.mustRun(t)
	if w.cand.count() != calls+1 || len(decodeSkipMeta(t, w.store.reps[0].MetaJSON).RouteFallbackDropped) != 0 || m.Coverage.WindowsDecoded != 4 {
		t.Fatalf("meta = %+v: want window 2 recovered on its route", m)
	}
	if !strings.Contains(persistedText(w.windowSTTHarness), "candidate line") {
		t.Errorf("the recovered window text is not indexed")
	}
}

func setCoverageCacheProducer(t *testing.T, state, by string) string {
	t.Helper()
	paths, _ := filepath.Glob(filepath.Join(state, "cache", "transcribe", "*.coverage.json"))
	if len(paths) != 1 {
		t.Fatalf("coverage cache files = %v", paths)
	}
	raw, _ := os.ReadFile(paths[0])
	edited := strings.Replace(string(raw), `"route_fallback_by":"whisper|large-v3"`, `"route_fallback_by":"`+by+`"`, 1)
	if err := os.WriteFile(paths[0], []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	return paths[0]
}

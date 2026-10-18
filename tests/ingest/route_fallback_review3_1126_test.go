package tests

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// SPEC §8.2.4 (spec 0.82.0, dir2mcp #1122): the final review of PR #1126.

// TestRouteFallbackReview3_NoTextDropSurvivesAHostErrorUnderFail (1): a
// producer change leaves window 2 dropped with no text. The policy then flips
// to fail and the host cannot cut the audio. Nothing new is dropped, and the
// record of window 2 must stay, or the gate never retries it.
func TestRouteFallbackReview3_NoTextDropSurvivesAHostErrorUnderFail(t *testing.T) {
	w := newWindowFallbackHarness(t)
	w.seed(t)
	setCoverageCacheProducer(t, w.stateDir, "whisper|old-model")
	w.cand.setBroken(true)
	w.def.mu.Lock()
	w.def.def = langReply{err: errors.New("default down")}
	w.def.mu.Unlock()
	w.now = w.now.Add(time.Hour)
	w.mustRun(t)
	if sm := decodeSkipMeta(t, w.store.reps[0].MetaJSON); len(sm.RouteFallbackDropped) != 1 {
		t.Fatalf("setup: meta %+v, want window 2 dropped with no text", sm)
	}
	w.svc.SetRouteErrorFallback(false)
	w.mu.Lock()
	w.failCut = map[int]error{win2StartMS: errors.New("ffmpeg not found")}
	w.mu.Unlock()
	w.mustRun(t)
	sm := decodeSkipMeta(t, w.store.reps[0].MetaJSON)
	if len(sm.RouteFallbackDropped) != 1 || sm.RouteFallbackDropped[0].StartMS != win2StartMS || sm.RouteFallbackSkipReason != "cut_failed" {
		t.Fatalf("meta %+v: the record of the window with no text was lost", sm)
	}
	if due, why := w.svc.RouteFallbackDue(w.store.reps[0].MetaJSON); !due {
		t.Errorf("the gate does not retry the carried window (%s)", why)
	}
}

// TestRouteFallbackReview3_MixedDropListIsPerWindow (2): a dropped list can
// mix a window whose text the cache keeps and one with no text. The cached
// one is named by range, so the gate processes the transcript on every run
// under default, and only the cached window counts as a fail cost.
func TestRouteFallbackReview3_MixedDropListIsPerWindow(t *testing.T) {
	w := newWindowFallbackHarness(t)
	meta := `{"source":"stt","route_fallback_at":"2026-10-18T08:00:00Z","route_fallback_by":"whisper|large-v3",` +
		`"route_fallback_dropped":[{"start_ms":0,"end_ms":5,"language":"kk","route":"whisper","covered":true,"fallback_from":"kk-ckpt"},` +
		`{"start_ms":10,"end_ms":15,"language":"kk","route":"whisper","covered":true,"fallback_from":"kk-ckpt"}],` +
		`"route_fallback_dropped_cached":[{"start_ms":0,"end_ms":5}]}`
	if due, _ := w.svc.RouteFallbackDue(meta); !due {
		t.Errorf("a list with one cached window is not processed on the next run")
	}
	noText := strings.Replace(meta, `,"route_fallback_dropped_cached":[{"start_ms":0,"end_ms":5}]`, "", 1)
	if due, _ := w.svc.RouteFallbackDue(noText); due {
		t.Errorf("a list with no cached window, one hour old, is due before route_retry_after")
	}
}

// TestRouteFallbackReview3_SkipReasonIsAClass (3): the skip reason stored in
// meta_json is a short class. A long error with a temp path and tool output
// goes to the log only.
func TestRouteFallbackReview3_SkipReasonIsAClass(t *testing.T) {
	h := newFallbackItemHarness(t, "default")
	fails := false
	long := "ffmpeg: /tmp/dir2mcp-vaudio-123456.mp4: Invalid data found when processing input " + strings.Repeat("x", 500)
	h.svc.ExtractAudioTrackFunc = func(context.Context, string) ([]byte, error) {
		if fails {
			return nil, errors.New(long)
		}
		return []byte(strings.Repeat("v", 4000)), nil
	}
	video := mediaDoc("talks/kk.mp4")
	video.DocType = "video"
	content := []byte(strings.Repeat("m", 4000))
	if err := h.svc.GenerateTranscriptRepresentation(context.Background(), video, content); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fails = true
	h.now = h.now.Add(25 * time.Hour)
	h.store.reps, h.store.chunks = nil, nil
	if err := h.svc.GenerateTranscriptRepresentation(context.Background(), video, content); err != nil {
		t.Fatalf("retry: %v", err)
	}
	raw := h.store.reps[0].MetaJSON
	if m := decodeSkipMeta(t, raw); m.RouteFallbackSkipReason != "audio_extract_failed" {
		t.Errorf("skip reason = %q, want the class audio_extract_failed", m.RouteFallbackSkipReason)
	}
	if strings.Contains(raw, "/tmp/") || strings.Contains(readRouteCache(t, h.state), "/tmp/") {
		t.Errorf("the meta or the cache stores the error text: %s", raw)
	}
	if !strings.Contains(h.logs.String(), "dir2mcp-vaudio-123456") {
		t.Errorf("the full error is not in the log")
	}
}

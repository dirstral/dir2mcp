package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §8.2.4 (spec 0.82.0, dir2mcp #1122): the doctor stt_routes check counts
// the transcripts that hold a route-fallback result and names when the next is
// due; with none it says so positively.

const routeFallbackRoutes = `media:
  stt:
    on_route_error: %s
    language_providers:
      kk: ckpt-kk
`

func itemFallbackMeta(at, by string) string {
	return `{"source":"stt","provider":"whisper","model":"base-model","route":"whisper","route_fallback_from":"ckpt-kk","route_fallback_at":"` + at + `","route_fallback_by":"` + by + `"}`
}

func windowFallbackMeta(at, by string) string {
	return `{"source":"stt","provider":"whisper","model":"base-model","coverage":{"windows_attempted":2,"windows_decoded":2,"decoded_ms":10,"duration_ms":10,"ranges":[{"start_ms":0,"end_ms":10}],` +
		`"languages":[{"start_ms":0,"end_ms":10,"language":"kk","route":"whisper","covered":true,"fallback_from":"ckpt-kk"}]},"route_fallback_at":"` + at + `","route_fallback_by":"` + by + `"}`
}

// routeFallbackDoctor writes the route config into dir, seeds the index, and
// returns the stt_routes check.
func routeFallbackDoctor(t *testing.T, policy string, reps ...seedRep) (string, string) {
	t.Helper()
	isolateFromAmbientCredentials(t)
	dir := testutil.TempDir(t)
	body := sttRoutesBase + strings.Replace(routeFallbackRoutes, "%s", policy, 1)
	if err := os.WriteFile(filepath.Join(dir, ".dir2mcp.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if len(reps) > 0 {
		_ = seedTranscripts(t, dir, reps...).Close()
	}
	c, ok := doctorCheckNamed(t, dir, "stt_routes")
	if !ok {
		t.Fatal("doctor produced no stt_routes check")
	}
	return c.Status, c.Detail
}

func TestDoctorRouteFallback_NoneIsReportedPositively(t *testing.T) {
	status, detail := routeFallbackDoctor(t, "default", seedRep{relPath: "a.mp3", metaJSON: `{"source":"stt","route":"ckpt-kk"}`})
	if status != "ok" || !strings.Contains(detail, "no route-fallback transcripts") || !strings.Contains(detail, "route_retry_after=24h0m0s") {
		t.Fatalf("status=%s detail=%q, want a positive no-fallback statement and the interval", status, detail)
	}
}

func TestDoctorRouteFallback_CountsAndNamesTheNextDueTime(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	older, newer := now.Add(-2*time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339)
	_, detail := routeFallbackDoctor(t, "default",
		seedRep{relPath: "a.mp3", metaJSON: itemFallbackMeta(newer, "whisper|base-model")},
		seedRep{relPath: "b.mp3", metaJSON: windowFallbackMeta(older, "whisper|base-model")},
		seedRep{relPath: "c.mp3", metaJSON: windowFallbackMeta(older, "whisper|base-model"), deleted: true},
	)
	want := "2 route-fallback transcript(s), next due " + now.Add(22*time.Hour).Format(time.RFC3339)
	if !strings.Contains(detail, want) {
		t.Fatalf("detail %q: want %q: 2 live fallback transcripts, next due the earliest time plus 24h", detail, want)
	}
}

func TestDoctorRouteFallback_DueOnTheNextRun(t *testing.T) {
	recent := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	future := time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)
	dropped := `{"source":"stt","route_fallback_at":"` + recent + `","route_fallback_by":"whisper|base-model","route_fallback_dropped":[{"start_ms":0,"end_ms":5,"language":"kk","route":"whisper","covered":true,"fallback_from":"ckpt-kk"}]}`
	for name, c := range map[string]struct {
		policy string
		meta   string
	}{
		"no producer":                {"default", itemFallbackMeta(recent, "")},
		"other producer":             {"default", itemFallbackMeta(recent, "whisper|old-model")},
		"no time":                    {"default", itemFallbackMeta("", "whisper|base-model")},
		"future time":                {"default", itemFallbackMeta(future, "whisper|base-model")},
		"policy fail":                {"fail", itemFallbackMeta(recent, "whisper|base-model")},
		"dropped windows under fail": {"fail", dropped},
	} {
		t.Run(name, func(t *testing.T) {
			_, detail := routeFallbackDoctor(t, c.policy, seedRep{relPath: "a.mp3", metaJSON: c.meta})
			if !strings.Contains(detail, "1 route-fallback transcript(s), due on the next run") {
				t.Fatalf("detail %q: want the result due on the next run", detail)
			}
		})
	}
}

// TestDoctorRouteFallback_MixedDropListCountsOnlyCachedWindows: a dropped
// list with one window whose text the cache keeps and one with no text counts
// one window as the fail cost.
func TestDoctorRouteFallback_MixedDropListCountsOnlyCachedWindows(t *testing.T) {
	recent := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	mixed := `{"source":"stt","route_fallback_at":"` + recent + `","route_fallback_by":"whisper|base-model",` +
		`"route_fallback_dropped":[{"start_ms":0,"end_ms":5,"language":"kk","route":"whisper","covered":true,"fallback_from":"ckpt-kk"},` +
		`{"start_ms":10,"end_ms":15,"language":"kk","route":"whisper","covered":true,"fallback_from":"ckpt-kk"}],` +
		`"route_fallback_dropped_cached":[{"start_ms":0,"end_ms":5}]}`
	_, detail := routeFallbackDoctor(t, "fail", seedRep{relPath: "a.mp3", metaJSON: mixed})
	if want := "1 window(s) in 1 transcript(s) dropped under on_route_error=fail"; !strings.Contains(detail, want) {
		t.Fatalf("detail %q lacks %q", detail, want)
	}
}

// TestDoctorRouteFallback_DroppedUnderDefaultIsProcessedNextRun: a window
// dropped under fail whose text the cache keeps is processed on the next run
// under default too (the gate restores the cached text), so doctor says what
// the gate does.
func TestDoctorRouteFallback_DroppedUnderDefaultIsProcessedNextRun(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	meta := `{"source":"stt","route_fallback_at":"` + at.Format(time.RFC3339) + `","route_fallback_by":"whisper|base-model","route_fallback_dropped_cached":[{"start_ms":0,"end_ms":5}],"route_fallback_dropped":[{"start_ms":0,"end_ms":5,"language":"kk","route":"whisper","covered":true,"fallback_from":"ckpt-kk"}]}`
	_, detail := routeFallbackDoctor(t, "default", seedRep{relPath: "a.mp3", metaJSON: meta})
	if want := "1 route-fallback transcript(s), due on the next run"; !strings.Contains(detail, want) {
		t.Fatalf("detail %q: want %q", detail, want)
	}
}

// TestDoctorRouteFallback_CountsRetriesThatCouldNotRunAndTheFailCost: the
// report counts the retries a host problem stopped, and under fail the
// transcripts whose dropped windows are decoded again on every run. With
// none it says so positively.
func TestDoctorRouteFallback_CountsRetriesThatCouldNotRunAndTheFailCost(t *testing.T) {
	recent := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	skipped := `{"source":"stt","route_fallback_from":"ckpt-kk","route_fallback_at":"` + recent + `","route_fallback_by":"whisper|base-model","route_fallback_skip_reason":"audio_extract_failed"}`
	dropped := `{"source":"stt","route_fallback_at":"` + recent + `","route_fallback_by":"whisper|base-model","route_fallback_dropped_cached":[{"start_ms":0,"end_ms":5}],"route_fallback_dropped":[{"start_ms":0,"end_ms":5,"language":"kk","route":"whisper","covered":true,"fallback_from":"ckpt-kk"}]}`
	_, detail := routeFallbackDoctor(t, "fail", seedRep{relPath: "a.mp3", metaJSON: skipped}, seedRep{relPath: "b.mp3", metaJSON: dropped})
	for _, want := range []string{"1 route-fallback retries could not run: audio_extract_failed", "1 window(s) in 1 transcript(s) dropped under on_route_error=fail"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q lacks %q", detail, want)
		}
	}
	_, clean := routeFallbackDoctor(t, "fail", seedRep{relPath: "a.mp3", metaJSON: itemFallbackMeta(recent, "whisper|base-model")})
	for _, want := range []string{"every due route-fallback retry ran", "no transcript holds a window dropped under on_route_error=fail"} {
		if !strings.Contains(clean, want) {
			t.Errorf("detail %q lacks the positive line %q", clean, want)
		}
	}
}

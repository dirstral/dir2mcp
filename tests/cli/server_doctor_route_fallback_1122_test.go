package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	_, detail := routeFallbackDoctor(t, "default",
		seedRep{relPath: "a.mp3", metaJSON: itemFallbackMeta("2099-01-02T00:00:00Z", "whisper|base-model")},
		seedRep{relPath: "b.mp3", metaJSON: windowFallbackMeta("2099-01-01T00:00:00Z", "whisper|base-model")},
		seedRep{relPath: "c.mp3", metaJSON: windowFallbackMeta("2099-01-01T00:00:00Z", "whisper|base-model"), deleted: true},
	)
	if !strings.Contains(detail, "2 route-fallback transcript(s), next due 2099-01-02T00:00:00Z") {
		t.Fatalf("detail %q: want 2 live fallback transcripts, next due the earliest time plus 24h", detail)
	}
}

func TestDoctorRouteFallback_DueOnTheNextRun(t *testing.T) {
	for name, c := range map[string]struct {
		policy string
		meta   string
	}{
		"no producer":    {"default", itemFallbackMeta("2099-01-02T00:00:00Z", "")},
		"other producer": {"default", itemFallbackMeta("2099-01-02T00:00:00Z", "whisper|old-model")},
		"no time":        {"default", itemFallbackMeta("", "whisper|base-model")},
		"policy fail":    {"fail", itemFallbackMeta("2099-01-02T00:00:00Z", "whisper|base-model")},
	} {
		t.Run(name, func(t *testing.T) {
			_, detail := routeFallbackDoctor(t, c.policy, seedRep{relPath: "a.mp3", metaJSON: c.meta})
			if !strings.Contains(detail, "1 route-fallback transcript(s), due on the next run") {
				t.Fatalf("detail %q: want the result due on the next run", detail)
			}
		})
	}
}

package tests

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/quality"
)

// SPEC §8.2.3 (dirstral-spec 0.72.0), dir2mcp #1031. A decoder's own language
// report is not calibrated for a language it covers badly: on a multilingual archive a
// Whisper-class decoder reported Kyrgyz as Macedonian at confidence 1.0. An
// identifier profile outranks that report; a route may list candidates that a
// refused window falls through; validation records decide eligibility.

// TestLanguageIdentifier_OutranksTheDecoderReport: the decoder reports "mk" at
// full confidence on every window, the identifier reports "ky". Every window
// resolves to Kyrgyz and is decoded on the Kyrgyz route.
//
// Mutant killed: resolving on the decoder's report first (every window mk,
// decoded on the default route).
func TestLanguageIdentifier_OutranksTheDecoderReport(t *testing.T) {
	t.Parallel()
	def := &langWindowTranscriber{def: langReply{lang: "mk", conf: 1.0}}
	ky := &langWindowTranscriber{def: langReply{lang: "mk", conf: 1.0, text: "[00:00] бүгүн биз шайлоо жөнүндө сүйлөшөбүз\n[00:30] бул маанилүү маселе"}}
	h, content := newScopedHarness(t, def, scopeTotalMS)
	h.svc.SetLanguageIdentifier(&langWindowTranscriber{def: langReply{lang: "ky", conf: 0.9}}, "lid")
	h.svc.SetRouteTranscriber("ky", ky, "whisper-ky", []string{"ky"})

	meta := runScoped(t, h, "talks/ky.m4a", content)
	assertLangs(t, meta.Coverage.Languages, []covLang{
		{StartMS: 0, EndMS: scopeTotalMS, Language: "ky", LanguageSource: "detected", LanguageConfidence: f64(0.9), Route: "whisper-ky", Covered: true},
	})
	if !strings.Contains(persistedText(h), "шайлоо") {
		t.Errorf("the Kyrgyz route's text is not in the transcript:\n%s", persistedText(h))
	}
	if got := decodeScopedMetaField(t, h, "language_identifier"); got != "lid" {
		t.Errorf("language_identifier = %q, want lid", got)
	}
}

// TestLanguageIdentifier_FailureAndLowConfidenceAreNoSignal: an identifier that
// errors, or reports below the floor, leaves resolution to the decoder's report,
// and never fails the recording.
func TestLanguageIdentifier_FailureAndLowConfidenceAreNoSignal(t *testing.T) {
	t.Parallel()
	for name, id := range map[string]*langWindowTranscriber{
		"error":          {def: langReply{err: errors.New("lid: 503")}},
		"low confidence": {def: langReply{lang: "ky", conf: 0.2}},
		"empty":          {def: langReply{lang: "", conf: 0}},
	} {
		t.Run(name, func(t *testing.T) {
			def := &langWindowTranscriber{def: langReply{lang: "ru", conf: 0.9}}
			h, content := newScopedHarness(t, def, scopeTotalMS)
			h.svc.SetLanguageIdentifier(id, "lid")
			meta := runScoped(t, h, "talks/ru.m4a", content)
			for _, e := range meta.Coverage.Languages {
				if e.Language != "ru" {
					t.Errorf("entry %+v: want the decoder's ru when the identifier gives no signal", e)
				}
			}
		})
	}
}

// TestCandidateRoutes_ARefusedWindowFallsThrough: the first Russian candidate
// decodes a repetition loop, which the per-window gate refuses; the second
// decodes it cleanly. No window is refused, and the record names the second.
//
// Mutant killed: refusing the window on the first candidate's loop.
func TestCandidateRoutes_ARefusedWindowFallsThrough(t *testing.T) {
	t.Parallel()
	def := &langWindowTranscriber{def: langReply{lang: "ru", conf: 0.9, text: ruText}}
	h, content := newScopedHarness(t, def, scopeTotalMS)
	h.svc.SetQualityGate(quality.New(quality.DefaultConfig()))
	h.svc.AddRouteCandidate("ru", &langWindowTranscriber{def: langReply{lang: "ru", conf: 0.9, text: loopText}}, "ru-first", nil)
	h.svc.AddRouteCandidate("ru", &langWindowTranscriber{def: langReply{lang: "ru", conf: 0.9, text: ruText}}, "ru-second", nil)

	meta := runScoped(t, h, "talks/fall.m4a", content)
	if len(meta.Coverage.Refused) != 0 {
		t.Errorf("refused = %+v, want none: the second candidate decoded every window", meta.Coverage.Refused)
	}
	for _, e := range meta.Coverage.Languages {
		if e.Route != "ru-second" {
			t.Errorf("entry %+v: route must name the candidate whose text was kept", e)
		}
	}
	if strings.Contains(persistedText(h), "thank you thank you") {
		t.Errorf("the refused candidate's loop reached the transcript")
	}
	if !strings.Contains(h.logs.String(), "trying candidate \"ru-second\"") {
		t.Errorf("no log line named the fall-through:\n%s", h.logs.String())
	}
}

// TestCandidateRoutes_AllRefusedRefusesTheWindow: every candidate loops, so the
// window is refused with the reason of the last refusal.
func TestCandidateRoutes_AllRefusedRefusesTheWindow(t *testing.T) {
	t.Parallel()
	def := &langWindowTranscriber{def: langReply{lang: "ru", conf: 0.9, text: ruText},
		script: map[int]langReply{2: {lang: "ru", conf: 0.9, text: ruText}}}
	h, content := newScopedHarness(t, def, scopeTotalMS)
	h.svc.SetQualityGate(quality.New(quality.DefaultConfig()))
	loop := langReply{lang: "ru", conf: 0.9, text: loopText}
	h.svc.AddRouteCandidate("ru", &langWindowTranscriber{def: loop}, "ru-first", nil)
	h.svc.AddRouteCandidate("ru", &langWindowTranscriber{def: loop}, "ru-second", nil)

	err := h.svc.GenerateTranscriptRepresentation(t.Context(), mediaDoc("talks/allloop.m4a"), content)
	if err != nil {
		t.Fatalf("every window refused must not be a provider error: %v", err)
	}
	if len(h.store.reps) != 0 {
		t.Fatalf("persisted %d representations, want none: every window was refused", len(h.store.reps))
	}
	if !strings.Contains(h.logs.String(), "quality gate refused window") {
		t.Errorf("no refusal was logged:\n%s", h.logs.String())
	}
}

// TestCandidateRoutes_AFailingCandidateIsAFailedWindow: a candidate that errors
// is a failed window, not a refusal, so the recording is retried rather than
// recorded as refused.
func TestCandidateRoutes_AFailingCandidateIsAFailedWindow(t *testing.T) {
	t.Parallel()
	def := &langWindowTranscriber{def: langReply{lang: "ru", conf: 0.9, text: ruText}}
	h, content := newScopedHarness(t, def, scopeTotalMS)
	h.svc.SetQualityGate(quality.New(quality.DefaultConfig()))
	h.svc.AddRouteCandidate("ru", &langWindowTranscriber{def: langReply{lang: "ru", conf: 0.9, text: loopText}}, "ru-first", nil)
	h.svc.AddRouteCandidate("ru", &langWindowTranscriber{def: langReply{err: errors.New("503")}}, "ru-second", nil)

	err := h.svc.GenerateTranscriptRepresentation(t.Context(), mediaDoc("talks/fail.m4a"), content)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want the candidate's failure", err)
	}
}

// TestCandidateRoutes_AFailedCandidateDoesNotPoisonLaterWindows: the second
// candidate fails on window 1 only. The state must not advance to it, so the
// next windows still decode (on the route that last produced text).
func TestCandidateRoutes_AFailedCandidateDoesNotPoisonLaterWindows(t *testing.T) {
	t.Parallel()
	def := &langWindowTranscriber{def: langReply{lang: "ru", conf: 0.9, text: ruText}}
	h, content := newScopedHarness(t, def, scopeTotalMS)
	h.svc.SetQualityGate(quality.New(quality.DefaultConfig()))
	first := &langWindowTranscriber{def: langReply{lang: "ru", conf: 0.9, text: ruText},
		script: map[int]langReply{1: {lang: "ru", conf: 0.9, text: loopText}}}
	second := &langWindowTranscriber{def: langReply{err: errors.New("503")}}
	h.svc.AddRouteCandidate("ru", first, "ru-first", nil)
	h.svc.AddRouteCandidate("ru", second, "ru-second", nil)

	meta := runScoped(t, h, "talks/poison.m4a", content)
	if meta.Coverage.WindowsDecoded != 3 {
		t.Errorf("windows_decoded = %d, want 3: only the first window's candidates failed", meta.Coverage.WindowsDecoded)
	}
	if second.callCount() != 1 {
		t.Errorf("the failing candidate decoded %d times, want 1: later windows must not start on it", second.callCount())
	}
}

func loadIdentityCfg(t *testing.T, yaml string) config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, yaml)
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	cfg.StateDir = t.TempDir()
	return cfg
}

const candidateProfiles = "" +
	"providers:\n" +
	"  ky-fine:\n" +
	"    kind: whisper\n" +
	"    base_url: http://127.0.0.1:1\n" +
	"    stt_model: whisper-medium-ky\n" +
	"  ky-small:\n" +
	"    kind: whisper\n" +
	"    base_url: http://127.0.0.1:1\n" +
	"    stt_model: whisper-small-ky\n" +
	"    stt_validation:\n" +
	"      - {language: ky, method: cross-model agreement, sample: 6 clips, score: 0.15, date: 2026-07-20}\n" +
	"  lid:\n" +
	"    kind: whisper\n" +
	"    base_url: http://127.0.0.1:1\n" +
	"    stt_model: mms-lid-4017\n" +
	"media:\n" +
	"  stt:\n" +
	"    language_scope: window\n" +
	"    language_identifier: lid\n" +
	"    language_providers:\n" +
	"      ky: [ky-fine, ky-small]\n"

// TestCandidateRoutes_ValidationDecidesEligibilityAndIdentity: under
// require_validation the unvalidated candidate is not eligible, so it leaves the
// route table that joins the identity; the identifier joins it too.
func TestCandidateRoutes_ValidationDecidesEligibilityAndIdentity(t *testing.T) {
	t.Parallel()
	open := loadIdentityCfg(t, candidateProfiles)
	strict := loadIdentityCfg(t, candidateProfiles+"    require_validation: true\n")

	svcOpen := mustNewIngestService(t, open, &fakeIngestStore{})
	svcOpen.SetSTTIdentity("whisper", "large-v3")
	idOpen := svcOpen.ActiveTranscriptIdentity()
	if !strings.Contains(idOpen, "ky=ky-fine|whisper-medium-ky+ky-small|whisper-small-ky") {
		t.Errorf("identity %q must list both candidates in order", idOpen)
	}
	if !strings.Contains(idOpen, "@identifier=lid|mms-lid-4017|probe=30s") {
		t.Errorf("identity %q must name the identifier and its probe length", idOpen)
	}
	probe := loadIdentityCfg(t, candidateProfiles+"    language_probe_sec: 12\n")
	svcProbe := mustNewIngestService(t, probe, &fakeIngestStore{})
	svcProbe.SetSTTIdentity("whisper", "large-v3")
	if idProbe := svcProbe.ActiveTranscriptIdentity(); idProbe == idOpen || !strings.Contains(idProbe, "probe=12s") {
		t.Errorf("changing only language_probe_sec must change the identity: %q vs %q", idProbe, idOpen)
	}

	svcStrict := mustNewIngestService(t, strict, &fakeIngestStore{})
	svcStrict.SetSTTIdentity("whisper", "large-v3")
	idStrict := svcStrict.ActiveTranscriptIdentity()
	// Only the route table is under test. Other identity parts may name the same
	// profile: with stt_provider auto, diarization resolves to the first
	// STT-capable profile, which here is ky-fine.
	routes := routeTableOf(t, idStrict)
	if strings.Contains(routes, "ky-fine") || !strings.Contains(routes, "ky=ky-small|whisper-small-ky") {
		t.Errorf("route table %q (identity %q): under require_validation only the validated candidate is eligible", routes, idStrict)
	}
}

// routeTableOf returns the "routes=..." segment of a transcript identity: from
// "routes=" to the next ";" or "#", or to the end.
func routeTableOf(t *testing.T, identity string) string {
	t.Helper()
	i := strings.Index(identity, "routes=")
	if i < 0 {
		t.Fatalf("identity %q has no route table", identity)
	}
	rest := identity[i:]
	if j := strings.IndexAny(rest, ";#"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// decodeScopedMetaField reads one top-level string field of the persisted
// transcript's meta_json.
func decodeScopedMetaField(t *testing.T, h *windowSTTHarness, field string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(h.store.reps[0].MetaJSON), &m); err != nil {
		t.Fatalf("decode meta_json: %v", err)
	}
	v, _ := m[field].(string)
	return v
}

package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/whisperapi"
)

// routingConfig loads a config whose DEFAULT whisper STT profile is pinned to
// language "ru" and declares coverage [en] only (so "ru" is UNCOVERED on the
// default), plus a route target `whisper-ru` that declares coverage [ru, en] and
// a distinct model/endpoint. routeKey adds `media.stt.language_providers[routeKey]
// = whisper-ru`; "" installs no route table at all.
func routingConfig(t *testing.T, routeKey string) config.Config {
	t.Helper()
	yaml := "" +
		"providers:\n" +
		"  whisper:\n" +
		"    base_url: http://gpu:9001/v1\n" +
		"    stt_model: base\n" +
		"    stt_language: ru\n" +
		"    stt_languages: [en]\n" +
		"  whisper-ru:\n" +
		"    kind: whisper\n" +
		"    base_url: http://gpu:9002/v1\n" +
		"    stt_model: large-v3\n" +
		"    stt_languages: [ru, en]\n"
	if routeKey != "" {
		yaml += "media:\n  stt:\n    language_providers:\n      " + routeKey + ": whisper-ru\n"
	}
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	// The default STT profile is the (overridden) built-in whisper, pinned to ru.
	cfg.STTProvider = "whisper"
	return cfg
}

// TestSTTRouting_PinnedLanguageSelectsMappedProfile pins the routing mechanism
// (SPEC §8.2.1, #566): when the resolved source-language pin matches a
// media.stt.language_providers entry, the mapped profile REPLACES the default STT
// provider on the reported-provider path; a pin with no matching entry keeps the
// default — including when OTHER routes exist.
func TestSTTRouting_PinnedLanguageSelectsMappedProfile(t *testing.T) {
	// Routed: pin "ru" -> whisper-ru replaces the default whisper profile.
	name, model, ok := ResolveSTTProviderModel(routingConfig(t, "ru"))
	if !ok {
		t.Fatal("expected an STT profile to resolve with routing on")
	}
	if name != "whisper-ru" || model != "large-v3" {
		t.Fatalf("routed STT = %q/%q, want whisper-ru/large-v3 (the mapped profile)", name, model)
	}

	// No route table at all -> the default whisper profile is kept.
	assertDefaultKept := func(t *testing.T, cfg config.Config, label string) {
		name, model, ok := ResolveSTTProviderModel(cfg)
		if !ok {
			t.Fatalf("%s: expected an STT profile to resolve", label)
		}
		if name != "whisper" || model != "base" {
			t.Fatalf("%s: STT = %q/%q, want the default whisper/base", label, name, model)
		}
	}
	assertDefaultKept(t, routingConfig(t, ""), "no route table")
	// Routes EXIST but the pin ("ru") matches none of them ("en" only) -> default
	// kept. This proves the miss path, which an empty table does not exercise.
	assertDefaultKept(t, routingConfig(t, "en"), "non-matching route present")
}

// TestSTTRouting_TranscriberBuildUsesRoutedProfile confirms the ACTUAL transcriber
// build path honours the route: the built transcriber must be the ROUTED
// whisper-ru client (model large-v3, endpoint gpu:9002), not the default whisper
// (model base, gpu:9001). Both profiles build a valid *whisperapi.Client, so a
// mere non-nil check would not detect a build path that ignored routing — assert
// the route-specific model + base_url.
func TestSTTRouting_TranscriberBuildUsesRoutedProfile(t *testing.T) {
	tr, err := TranscriberFromConfig(routingConfig(t, "ru"))
	if err != nil {
		t.Fatalf("build routed transcriber: %v", err)
	}
	wc, ok := tr.(*whisperapi.Client)
	if !ok {
		t.Fatalf("expected a *whisperapi.Client, got %T", tr)
	}
	if wc.DefaultModel != "large-v3" {
		t.Fatalf("built transcriber model = %q, want large-v3 (routed whisper-ru); routing did not reach the build path", wc.DefaultModel)
	}
	if wc.BaseURL != "http://gpu:9002/v1" {
		t.Fatalf("built transcriber base_url = %q, want http://gpu:9002/v1 (routed whisper-ru)", wc.BaseURL)
	}
}

// TestSTTRouting_CoveredRouteSuppressesHonestCoverageWarning pins the Slice-A
// floor interaction (SPEC §8.2.1, #566): the default profile declares [en] only,
// so the pinned "ru" is UNCOVERED and the transcript meta records
// language_covered:false. Routing "ru" to whisper-ru (which declares [ru, en])
// makes the ROUTED profile cover the language, so the honest-coverage flag is NOT
// emitted — the coverage check runs against the routed profile, not the default.
func TestSTTRouting_CoveredRouteSuppressesHonestCoverageWarning(t *testing.T) {
	metaFor := func(routeKey string) (prov, lang, meta string, coverage []string) {
		s := &Service{cfg: routingConfig(t, routeKey)}
		s.resolveTranscriptIdentityFields()
		m, err := s.sttTranscriptMetaJSON(nil, "some transcript text", false)
		if err != nil {
			t.Fatalf("sttTranscriptMetaJSON: %v", err)
		}
		return s.sttProvider, s.transcriptLanguage, m, s.sttLanguages
	}

	// Control: no routing -> default whisper (coverage [en]) with pin ru is
	// uncovered, so meta carries language_covered:false.
	prov, lang, m, cov := metaFor("")
	if prov != "whisper" || lang != "ru" {
		t.Fatalf("unrouted identity = provider %q lang %q, want whisper/ru; coverage %v", prov, lang, cov)
	}
	if !strings.Contains(m, `"language_covered":false`) {
		t.Fatalf("unrouted (uncovered) transcript must record language_covered:false, got %s", m)
	}

	// Routed: whisper-ru covers ru, so the pin is covered and the flag is absent.
	prov, lang, m, cov = metaFor("ru")
	if prov != "whisper-ru" || lang != "ru" {
		t.Fatalf("routed identity = provider %q lang %q, want whisper-ru/ru; coverage %v", prov, lang, cov)
	}
	if strings.Contains(m, "language_covered") {
		t.Fatalf("routed+covered transcript must OMIT language_covered, got %s", m)
	}
}

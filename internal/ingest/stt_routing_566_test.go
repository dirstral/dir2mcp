package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// routingConfig loads a config whose DEFAULT whisper STT profile is pinned to
// language "ru" and declares coverage [en] only (so "ru" is UNCOVERED on the
// default), plus a route target `whisper-ru` that declares coverage [ru, en]. The
// caller sets media.stt.language_providers to toggle routing on/off.
func routingConfig(t *testing.T, route bool) config.Config {
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
	if route {
		yaml += "media:\n  stt:\n    language_providers:\n      ru: whisper-ru\n"
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
// provider on the reported-provider path, and a language with no entry keeps the
// default. Routing is pin-based, so the reported provider is the one that will
// actually transcribe.
func TestSTTRouting_PinnedLanguageSelectsMappedProfile(t *testing.T) {
	// Routed: pin "ru" -> whisper-ru replaces the default whisper profile.
	name, model, ok := ResolveSTTProviderModel(routingConfig(t, true))
	if !ok {
		t.Fatal("expected an STT profile to resolve with routing on")
	}
	if name != "whisper-ru" {
		t.Fatalf("routed STT provider = %q, want whisper-ru (the mapped profile)", name)
	}
	if model != "large-v3" {
		t.Fatalf("routed STT model = %q, want large-v3 (the mapped profile's model)", model)
	}

	// Not routed: same pin, no route table -> the default whisper profile is kept.
	name, model, ok = ResolveSTTProviderModel(routingConfig(t, false))
	if !ok {
		t.Fatal("expected an STT profile to resolve with routing off")
	}
	if name != "whisper" {
		t.Fatalf("unrouted STT provider = %q, want the default whisper profile", name)
	}
	if model != "base" {
		t.Fatalf("unrouted STT model = %q, want base (the default profile's model)", model)
	}
}

// TestSTTRouting_TranscriberBuildUsesRoutedProfile confirms the ACTUAL transcriber
// build path also honours the route (both the reported profile and the built
// transcriber share the routing resolution), so a routed config builds a
// transcriber rather than diverging from the reported provider.
func TestSTTRouting_TranscriberBuildUsesRoutedProfile(t *testing.T) {
	tr, err := TranscriberFromConfig(routingConfig(t, true))
	if err != nil {
		t.Fatalf("build routed transcriber: %v", err)
	}
	if tr == nil {
		t.Fatal("expected a non-nil transcriber for a routed whisper profile")
	}
}

// TestSTTRouting_CoveredRouteSuppressesHonestCoverageWarning pins the Slice-A
// floor interaction (SPEC §8.2.1, #566): the default profile declares [en] only,
// so the pinned "ru" is UNCOVERED and the transcript meta records
// language_covered:false. Routing "ru" to whisper-ru (which declares [ru, en])
// makes the ROUTED profile cover the language, so the honest-coverage flag is NOT
// emitted — the coverage check runs against the routed profile, not the default.
func TestSTTRouting_CoveredRouteSuppressesHonestCoverageWarning(t *testing.T) {
	metaFor := func(route bool) (prov, lang, meta string, coverage []string) {
		s := &Service{cfg: routingConfig(t, route)}
		s.resolveTranscriptIdentityFields()
		m, err := s.sttTranscriptMetaJSON(nil, "some transcript text", false)
		if err != nil {
			t.Fatalf("sttTranscriptMetaJSON: %v", err)
		}
		return s.sttProvider, s.transcriptLanguage, m, s.sttLanguages
	}

	// Control: no routing -> default whisper (coverage [en]) with pin ru is
	// uncovered, so meta carries language_covered:false.
	prov, lang, m, cov := metaFor(false)
	if prov != "whisper" || lang != "ru" {
		t.Fatalf("unrouted identity = provider %q lang %q, want whisper/ru; coverage %v", prov, lang, cov)
	}
	if !strings.Contains(m, `"language_covered":false`) {
		t.Fatalf("unrouted (uncovered) transcript must record language_covered:false, got %s", m)
	}

	// Routed: whisper-ru covers ru, so the pin is covered and the flag is absent.
	prov, lang, m, cov = metaFor(true)
	if prov != "whisper-ru" || lang != "ru" {
		t.Fatalf("routed identity = provider %q lang %q, want whisper-ru/ru; coverage %v", prov, lang, cov)
	}
	if strings.Contains(m, "language_covered") {
		t.Fatalf("routed+covered transcript must OMIT language_covered, got %s", m)
	}
}

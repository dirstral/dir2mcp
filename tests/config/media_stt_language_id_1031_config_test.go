package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// SPEC §8.2.3 (dir2mcp #1031): language_providers values may be ordered lists,
// media.stt.language_identifier names an STT-capable profile, and a profile may
// carry stt_validation records.

const whisperProfiles = "" +
	"providers:\n" +
	"  a:\n" +
	"    kind: whisper\n" +
	"    base_url: http://127.0.0.1:1\n" +
	"    stt_model: m-a\n" +
	"    stt_validation:\n" +
	"      - {language: ky, method: agreement, sample: 6 clips, score: 0.15, date: 2026-07-20}\n" +
	"  b:\n" +
	"    kind: whisper\n" +
	"    base_url: http://127.0.0.1:1\n" +
	"    stt_model: m-b\n"

func TestLanguageProviders_ScalarAndListForms(t *testing.T) {
	cfg := loadCfg(t, whisperProfiles+"media:\n  stt:\n    language_providers:\n      ky: [b, a]\n      ru: a\n")
	if got := cfg.MediaSTTLanguageCandidates["ky"]; strings.Join(got, ",") != "b,a" {
		t.Errorf("ky candidates = %v, want [b a] in order", got)
	}
	if cfg.MediaSTTLanguageProviders["ky"] != "b" || cfg.MediaSTTLanguageProviders["ru"] != "a" {
		t.Errorf("first-candidate routes = %v, want ky=b ru=a", cfg.MediaSTTLanguageProviders)
	}
	if got := cfg.MediaSTTLanguageCandidates["ru"]; len(got) != 1 || got[0] != "a" {
		t.Errorf("a single name must be a one-element list, got %v", got)
	}
}

func TestLanguageProviders_BlockStyleList(t *testing.T) {
	cfg := loadCfg(t, whisperProfiles+"media:\n  stt:\n    language_providers:\n      ky:\n        - b\n        - a\n")
	if got := cfg.MediaSTTLanguageCandidates["ky"]; strings.Join(got, ",") != "b,a" {
		t.Errorf("block-style ky candidates = %v, want [b a]", got)
	}
}

// TestRouteSTTProfile_FirstEligibleCandidate: under require_validation an item
// pinned to Kyrgyz decodes on the first candidate WITH a record (a), not the
// first listed (b); with no eligible candidate the default profile stays.
func TestRouteSTTProfile_FirstEligibleCandidate(t *testing.T) {
	cfg := loadCfg(t, whisperProfiles+"media:\n  stt:\n    require_validation: true\n    language_providers:\n      ky: [b, a]\n      ru: [b]\n")
	def := cfg.Providers().ByName()["b"]
	def.Name = "default"
	def.STTLanguage = "ky"
	got, err := cfg.RouteSTTProfile(def)
	if err != nil || got.Name != "a" {
		t.Fatalf("ky route = %q (%v), want the validated candidate a", got.Name, err)
	}
	def.STTLanguage = "ru"
	got, err = cfg.RouteSTTProfile(def)
	if err != nil || got.Name != "default" {
		t.Fatalf("ru route = %q (%v), want the default: no candidate is eligible", got.Name, err)
	}
}

func TestLanguageProviders_EmptyListAndBadCandidateRejected(t *testing.T) {
	for name, yaml := range map[string]string{
		"empty list":        whisperProfiles + "media:\n  stt:\n    language_providers:\n      ky: []\n",
		"unknown candidate": whisperProfiles + "media:\n  stt:\n    language_providers:\n      ky: [a, nope]\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
			writeFile(t, path, yaml)
			cfg, err := config.LoadFile(path)
			if err == nil {
				err = cfg.Validate()
			}
			if err == nil || !strings.Contains(err.Error(), "CONFIG_INVALID") {
				t.Fatalf("err = %v, want CONFIG_INVALID", err)
			}
		})
	}
}

func TestLanguageIdentifier_Validation(t *testing.T) {
	ok := loadCfg(t, whisperProfiles+"media:\n  stt:\n    language_identifier: a\n    language_probe_sec: 20\n    require_validation: true\n")
	if err := ok.Validate(); err != nil {
		t.Fatalf("a whisper identifier must validate: %v", err)
	}
	if ok.MediaSTTLanguageIdentifier != "a" || ok.MediaSTTLanguageProbeSec != 20 || !ok.MediaSTTRequireValidation {
		t.Errorf("parsed identifier=%q probe=%d require=%v", ok.MediaSTTLanguageIdentifier, ok.MediaSTTLanguageProbeSec, ok.MediaSTTRequireValidation)
	}
	if config.Default().MediaSTTLanguageProbeSec != 30 {
		t.Errorf("default probe = %d, want 30", config.Default().MediaSTTLanguageProbeSec)
	}
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, whisperProfiles+"media:\n  stt:\n    language_identifier: nope\n")
	if _, err := config.LoadFile(path); err == nil || !strings.Contains(err.Error(), "language_identifier") {
		t.Fatalf("err = %v, want a language_identifier CONFIG_INVALID at load", err)
	}
	neg := config.Default()
	neg.MediaSTTLanguageProbeSec = -1
	if err := neg.Validate(); err == nil || !strings.Contains(err.Error(), "language_probe_sec") {
		t.Fatalf("err = %v, want a language_probe_sec error", err)
	}
}

func TestSTTValidation_RecordsParse(t *testing.T) {
	cfg := loadCfg(t, whisperProfiles)
	prof := cfg.Providers().ByName()["a"]
	if len(prof.STTValidation) != 1 || prof.STTValidation[0].Language != "ky" || prof.STTValidation[0].Score != "0.15" {
		t.Fatalf("stt_validation = %+v", prof.STTValidation)
	}
	if !prof.ValidatedFor("ky-KG") || prof.ValidatedFor("ru") {
		t.Errorf("ValidatedFor must match the primary subtag only")
	}
	if cfg.Providers().ByName()["b"].ValidatedFor("ky") {
		t.Errorf("a profile with no records is validated for nothing")
	}
}

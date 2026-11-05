package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/provider"
)

// media.language is the corpus-wide source-language pin (SPEC §8.8, spec
// 0.85.0, dir2mcp #1136). Before #1136 neither media.language nor its
// media.stt.language spelling was read: the key was ignored with a warning and
// the language was detected as if no pin was set.

const whisperProfileYAML = "" +
	"providers:\n" +
	"  whisper:\n" +
	"    kind: whisper\n" +
	"    base_url: http://127.0.0.1:9010\n" +
	"    stt_model: large-v3\n" +
	"stt_provider: whisper\n"

// sttProfile resolves the whisper STT profile the way the transcriber build does.
func sttProfile(t *testing.T, cfg config.Config) provider.Profile {
	t.Helper()
	prof, err := cfg.Providers().ResolveExplicit(provider.CapSTT, "whisper", true)
	if err != nil {
		t.Fatalf("resolve whisper STT profile: %v", err)
	}
	return prof
}

func noUnknownKeyWarning(t *testing.T, cfg config.Config) {
	t.Helper()
	for _, w := range cfg.Warnings {
		if strings.Contains(w.Error(), "unrecognized key") {
			t.Errorf("unexpected warning: %v", w)
		}
	}
}

// TestMediaLanguage_PinsSTTProfile: media.language fills the stt_language of
// the STT profile, so the transcription request and every other consumer of the
// profile's language see the pin.
func TestMediaLanguage_PinsSTTProfile(t *testing.T) {
	cfg := loadCfg(t, whisperProfileYAML+"media:\n  language: uk\n")
	if cfg.MediaLanguage != "uk" {
		t.Fatalf("MediaLanguage = %q, want uk", cfg.MediaLanguage)
	}
	if got := sttProfile(t, cfg).STTLanguage; got != "uk" {
		t.Errorf("whisper STTLanguage = %q, want uk (media.language pin)", got)
	}
	noUnknownKeyWarning(t, cfg)
}

// TestMediaLanguage_STTAliasPins is the #1136 regression: the media.stt.language
// spelling an operator wrote is an alias of media.language.
func TestMediaLanguage_STTAliasPins(t *testing.T) {
	cfg := loadCfg(t, whisperProfileYAML+"media:\n  stt:\n    language: uk\n    language_scope: item\n")
	if cfg.MediaLanguage != "uk" {
		t.Fatalf("MediaLanguage = %q, want uk (from media.stt.language)", cfg.MediaLanguage)
	}
	if got := sttProfile(t, cfg).STTLanguage; got != "uk" {
		t.Errorf("whisper STTLanguage = %q, want uk", got)
	}
	if cfg.MediaSTTLanguageScope != "item" {
		t.Errorf("media.stt.language_scope = %q, want item (the alias must not swallow a sibling key)", cfg.MediaSTTLanguageScope)
	}
	noUnknownKeyWarning(t, cfg)
}

// TestMediaLanguage_ProfileSTTLanguageWins: a profile's own stt_language is more
// specific than the corpus pin and is kept.
func TestMediaLanguage_ProfileSTTLanguageWins(t *testing.T) {
	yaml := strings.Replace(whisperProfileYAML, "    stt_model: large-v3\n", "    stt_model: large-v3\n    stt_language: ru\n", 1)
	cfg := loadCfg(t, yaml+"media:\n  language: uk\n")
	if got := sttProfile(t, cfg).STTLanguage; got != "ru" {
		t.Errorf("whisper STTLanguage = %q, want ru (the profile's own stt_language wins)", got)
	}
}

// TestMediaLanguage_UnsetLeavesProfilesUnpinned: no pin, no change.
func TestMediaLanguage_UnsetLeavesProfilesUnpinned(t *testing.T) {
	cfg := loadCfg(t, whisperProfileYAML)
	if got := sttProfile(t, cfg).STTLanguage; got != "" {
		t.Errorf("whisper STTLanguage = %q, want empty (no pin)", got)
	}
}

// TestMediaLanguage_RoutesOnPin: the pin drives media.stt.language_providers
// routing exactly as a profile stt_language does.
func TestMediaLanguage_RoutesOnPin(t *testing.T) {
	yaml := "" +
		"providers:\n" +
		"  whisper:\n" +
		"    kind: whisper\n" +
		"    base_url: http://127.0.0.1:9010\n" +
		"    stt_model: large-v3\n" +
		"  whisper-uk:\n" +
		"    kind: whisper\n" +
		"    base_url: http://127.0.0.1:9011\n" +
		"    stt_model: uk-ckpt\n" +
		"stt_provider: whisper\n" +
		"media:\n" +
		"  language: uk\n" +
		"  stt:\n" +
		"    language_providers:\n" +
		"      uk: whisper-uk\n"
	cfg := loadCfg(t, yaml)
	routed, err := cfg.RouteSTTProfile(sttProfile(t, cfg))
	if err != nil {
		t.Fatalf("RouteSTTProfile: %v", err)
	}
	if routed.Name != "whisper-uk" || routed.STTLanguage != "uk" {
		t.Errorf("routed = %q (lang %q), want whisper-uk with uk", routed.Name, routed.STTLanguage)
	}
}

// TestMediaLanguage_Validation rejects a value that is not a BCP-47 tag and
// accepts a region-qualified one.
func TestMediaLanguage_Validation(t *testing.T) {
	for _, tc := range []struct {
		value   string
		wantErr bool
	}{
		{value: "uk"},
		{value: "pt-BR"},
		{value: "  uk  "},
		{value: "ukrainian", wantErr: true},
		{value: "fa_IR", wantErr: true},
		{value: "u", wantErr: true},
		{value: "uk-", wantErr: true},
		{value: "pt--BR", wantErr: true},
		{value: "zh-Hant-TW"},
		{value: "es-419"},
		{value: "en-u-ca-gregory"},
		{value: "en-x-a"},
		{value: "en-x-private"},
		{value: "en-x", wantErr: true},
		{value: "en-u", wantErr: true},
		{value: "en-u-x", wantErr: true},
		{value: "en-u-a", wantErr: true},
		{value: "en-US-x", wantErr: true},
	} {
		cfg := config.Default()
		cfg.MediaLanguage = tc.value
		err := cfg.Validate()
		if tc.wantErr {
			if err == nil || !strings.Contains(err.Error(), "media.language") {
				t.Errorf("Validate(%q) = %v, want a media.language CONFIG_INVALID", tc.value, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("Validate(%q) rejected a valid tag: %v", tc.value, err)
		}
	}
}

// TestMediaLanguage_InvalidInFileIsConfigInvalid: the load path fails fast.
func TestMediaLanguage_InvalidInFileIsConfigInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, "media:\n  stt:\n    language: ukrainian\n")
	if _, err := config.LoadFile(path); err == nil || !strings.Contains(err.Error(), "CONFIG_INVALID") {
		t.Fatalf("LoadFile = %v, want CONFIG_INVALID", err)
	}
}

// TestMediaLanguage_AliasConflictIsConfigInvalid: media.language and its alias
// media.stt.language with different values would leave the pin to file order,
// so the load fails (SPEC §8.8, spec 0.85.0).
func TestMediaLanguage_AliasConflictIsConfigInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, "media:\n  language: uk\n  stt:\n    language: ru\n")
	if _, err := config.LoadFile(path); err == nil || !strings.Contains(err.Error(), "CONFIG_INVALID") {
		t.Fatalf("LoadFile = %v, want CONFIG_INVALID for media.language uk vs media.stt.language ru", err)
	}
}

// TestMediaLanguage_AliasSameValueIsAccepted: both spellings with the same tag,
// in another case, are one pin.
func TestMediaLanguage_AliasSameValueIsAccepted(t *testing.T) {
	cfg := loadCfg(t, whisperProfileYAML+"media:\n  language: uk\n  stt:\n    language: UK\n")
	if !strings.EqualFold(cfg.MediaLanguage, "uk") {
		t.Fatalf("MediaLanguage = %q, want uk", cfg.MediaLanguage)
	}
	noUnknownKeyWarning(t, cfg)
}

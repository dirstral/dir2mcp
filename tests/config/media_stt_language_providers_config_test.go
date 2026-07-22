package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestMediaSTTLanguageProviders_Default confirms the routing table is empty by
// default, so behaviour is unchanged (single-provider STT) when unset (#566).
func TestMediaSTTLanguageProviders_Default(t *testing.T) {
	if got := config.Default().MediaSTTLanguageProviders; len(got) != 0 {
		t.Fatalf("media.stt.language_providers default = %v, want empty", got)
	}
}

// TestMediaSTTLanguageProviders_ParsesNestedYAML confirms the nested lang->profile
// map decodes and its keys normalize to the BCP-47 primary subtag (SPEC §8.2.1,
// #566): "en-US" collapses to "en" so a "ru-RU" pin matches a "ru" route.
func TestMediaSTTLanguageProviders_ParsesNestedYAML(t *testing.T) {
	yaml := "" +
		"providers:\n" +
		"  whisper-ru:\n" +
		"    kind: whisper\n" +
		"    stt_model: large-v3\n" +
		"    stt_languages: [ru, en]\n" +
		"media:\n" +
		"  stt:\n" +
		"    language_providers:\n" +
		"      ru: whisper-ru\n" +
		"      en-US: whisper-ru\n"

	cfg := loadCfg(t, yaml)
	got := cfg.MediaSTTLanguageProviders
	if len(got) != 2 {
		t.Fatalf("language_providers = %v, want 2 entries", got)
	}
	if got["ru"] != "whisper-ru" {
		t.Errorf("language_providers[ru] = %q, want whisper-ru", got["ru"])
	}
	if got["en"] != "whisper-ru" {
		t.Errorf("language_providers[en] (from en-US) = %q, want whisper-ru", got["en"])
	}
}

// TestMediaSTTLanguageProviders_UnknownProfileRejected pins the CONFIG_INVALID
// static-validation rule (SPEC §8.2.1, #566): a route naming a profile that does
// not exist must fail at startup, not silently at first transcription.
func TestMediaSTTLanguageProviders_UnknownProfileRejected(t *testing.T) {
	yaml := "" +
		"media:\n" +
		"  stt:\n" +
		"    language_providers:\n" +
		"      ru: no-such-profile\n"

	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, yaml)
	_, err := config.LoadFile(path)
	if err == nil {
		t.Fatal("expected CONFIG_INVALID for a route to an unknown provider profile")
	}
	if !strings.Contains(err.Error(), "CONFIG_INVALID") || !strings.Contains(err.Error(), "no-such-profile") {
		t.Fatalf("expected a CONFIG_INVALID unknown-profile error, got: %v", err)
	}
}

// TestMediaSTTLanguageProviders_NonSTTProfileRejected pins the second half of the
// CONFIG_INVALID rule: a route to a profile that exists but is not STT-capable per
// the capability matrix (SPEC 8.1.2) is rejected. A kind:anthropic profile serves
// only chat, so it can never transcribe.
func TestMediaSTTLanguageProviders_NonSTTProfileRejected(t *testing.T) {
	yaml := "" +
		"providers:\n" +
		"  chat-only:\n" +
		"    kind: anthropic\n" +
		"    chat_model: claude-x\n" +
		"media:\n" +
		"  stt:\n" +
		"    language_providers:\n" +
		"      ru: chat-only\n"

	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, yaml)
	_, err := config.LoadFile(path)
	if err == nil {
		t.Fatal("expected CONFIG_INVALID for a route to a non-STT-capable profile")
	}
	if !strings.Contains(err.Error(), "CONFIG_INVALID") || !strings.Contains(err.Error(), "not speech-to-text capable") {
		t.Fatalf("expected a CONFIG_INVALID non-STT-capable error, got: %v", err)
	}
}

// TestMediaSTTLanguageProviders_ConflictingRoutesRejected confirms two keys that
// collapse to the same primary subtag but name DIFFERENT profiles are rejected at
// parse time, so route lookup stays deterministic.
func TestMediaSTTLanguageProviders_ConflictingRoutesRejected(t *testing.T) {
	yaml := "" +
		"providers:\n" +
		"  whisper-a:\n    kind: whisper\n    stt_model: a\n" +
		"  whisper-b:\n    kind: whisper\n    stt_model: b\n" +
		"media:\n" +
		"  stt:\n" +
		"    language_providers:\n" +
		"      ru: whisper-a\n" +
		"      ru-RU: whisper-b\n"

	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, yaml)
	_, err := config.LoadFile(path)
	if err == nil {
		t.Fatal("expected an error for conflicting routes on the same primary subtag")
	}
	if !strings.Contains(err.Error(), "conflicting routes") {
		t.Fatalf("expected a conflicting-routes error, got: %v", err)
	}
}

// TestMediaSTTLanguageProviders_ValidRoutePassesValidation guards against
// over-rejection: a route to an existing STT-capable profile must load cleanly.
func TestMediaSTTLanguageProviders_ValidRoutePassesValidation(t *testing.T) {
	yaml := "" +
		"providers:\n" +
		"  whisper-ru:\n    kind: whisper\n    stt_model: large-v3\n    stt_languages: [ru]\n" +
		"media:\n" +
		"  stt:\n" +
		"    language_providers:\n" +
		"      ru: whisper-ru\n"

	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, yaml)
	if _, err := config.LoadFile(path); err != nil {
		t.Fatalf("valid language_providers route should load, got: %v", err)
	}
}

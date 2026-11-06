package tests

import (
	"reflect"
	"testing"
)

// A ` #` comment after a value is a YAML comment, not part of the value
// (#1142). Before the fix the flat parser kept it, so `key: value  # note`
// failed validation although every YAML tool accepts it.

func TestYAMLTrailingComment_ScalarValue(t *testing.T) {
	cfg := loadCfg(t, "server:\n  name: docs-server   # shown in clients\nmedia:\n  language: uk\t# pin\n")
	if cfg.ServerName != "docs-server" {
		t.Errorf("ServerName = %q, want docs-server", cfg.ServerName)
	}
	if cfg.MediaLanguage != "uk" {
		t.Errorf("MediaLanguage = %q, want uk (tab before #)", cfg.MediaLanguage)
	}
}

func TestYAMLTrailingComment_HashInsideValueIsKept(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"server:\n  name: a#b\n", "a#b"},
		{"server:\n  name: \"a # b\"  # note\n", "a # b"},
		{"server:\n  name: 'a # b'  # note\n", "a # b"},
		{"server:\n  name: \"a \\\" # b\"\n", "a \" # b"},
		{"server:\n  name: it's  # note\n", "it's"},
		{"server:\n  name: beta-'draft # note\n", "beta-'draft"},
		{"server:\n  name: a:'b # note\n", "a:'b"},
	} {
		cfg := loadCfg(t, tc.yaml)
		if cfg.ServerName != tc.want {
			t.Errorf("%q: ServerName = %q, want %q", tc.yaml, cfg.ServerName, tc.want)
		}
	}
}

// TestYAMLTrailingComment_EscapedSingleQuote: ” inside a single-quoted scalar
// is an escaped quote, so the # after it is still inside the scalar.
func TestYAMLTrailingComment_EscapedSingleQuote(t *testing.T) {
	cfg := loadCfg(t, "server:\n  name: 'it''s # x'  # note\n")
	if cfg.ServerName != "it's # x" {
		t.Errorf("ServerName = %q, want %q", cfg.ServerName, "it's # x")
	}
}

func TestYAMLTrailingComment_Lists(t *testing.T) {
	cfg := loadCfg(t, "media:\n  translate:\n    target_langs: [en, de]  # targets\n")
	if want := []string{"en", "de"}; !reflect.DeepEqual(cfg.MediaTranslateTargetLangs, want) {
		t.Errorf("inline list = %q, want %q", cfg.MediaTranslateTargetLangs, want)
	}
	cfg = loadCfg(t, "media:\n  translate:\n    target_langs:\n      - en   # first\n      - de\n")
	if want := []string{"en", "de"}; !reflect.DeepEqual(cfg.MediaTranslateTargetLangs, want) {
		t.Errorf("block list = %q, want %q", cfg.MediaTranslateTargetLangs, want)
	}
}

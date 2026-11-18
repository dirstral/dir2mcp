package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// A key that a yaml.v3 pass reads is not an unrecognized key. The flat parser
// must not warn on it in flow form or in block form. A misspelled sibling key
// must still warn.

const gigaamProfile = "" +
	"providers:\n" +
	"  gigaam:\n" +
	"    kind: whisper\n" +
	"    base_url: http://127.0.0.1:1\n" +
	"    stt_model: gigaam-v2\n" +
	"    stt_languages: [ru]\n"

// unknownKeyWarnings returns the "unrecognized key(s) ignored" warnings.
func unknownKeyWarnings(cfg config.Config) []string {
	var out []string
	for _, w := range cfg.Warnings {
		if strings.Contains(w.Error(), "unrecognized key") {
			out = append(out, w.Error())
		}
	}
	return out
}

func TestStructuredKeyWarning_LanguageProvidersFlowForm(t *testing.T) {
	cfg := loadCfg(t, gigaamProfile+
		"media:\n"+
		"  stt:\n"+
		"    language_identifier: gigaam\n"+
		"    language_providers: {ru: gigaam}\n")
	if got := unknownKeyWarnings(cfg); len(got) != 0 {
		t.Errorf("flow-form language_providers warned: %v", got)
	}
	if got := cfg.MediaSTTLanguageProviders["ru"]; got != "gigaam" {
		t.Errorf("MediaSTTLanguageProviders[ru] = %q, want gigaam (all: %v)", got, cfg.MediaSTTLanguageProviders)
	}
}

func TestStructuredKeyWarning_LanguageProvidersBlockForm(t *testing.T) {
	cfg := loadCfg(t, gigaamProfile+
		"media:\n"+
		"  stt:\n"+
		"    language_providers:\n"+
		"      ru: gigaam\n")
	if got := unknownKeyWarnings(cfg); len(got) != 0 {
		t.Errorf("block-form language_providers warned: %v", got)
	}
	if got := cfg.MediaSTTLanguageProviders["ru"]; got != "gigaam" {
		t.Errorf("MediaSTTLanguageProviders[ru] = %q, want gigaam", got)
	}
}

func TestStructuredKeyWarning_LanguageProvidersBlockCandidateList(t *testing.T) {
	cfg := loadCfg(t, gigaamProfile+
		"media:\n"+
		"  stt:\n"+
		"    language_providers:\n"+
		"      ru:\n"+
		"        - gigaam\n")
	if got := unknownKeyWarnings(cfg); len(got) != 0 {
		t.Errorf("block candidate-list language_providers warned: %v", got)
	}
	if got := cfg.MediaSTTLanguageCandidates["ru"]; len(got) != 1 || got[0] != "gigaam" {
		t.Errorf("MediaSTTLanguageCandidates[ru] = %v, want [gigaam]", got)
	}
}

func TestStructuredKeyWarning_TranslateGlossaryFlowForm(t *testing.T) {
	cfg := loadCfg(t, ""+
		"media:\n"+
		"  translate:\n"+
		"    glossary: {en: {Navalny: Navalny}}\n")
	if got := unknownKeyWarnings(cfg); len(got) != 0 {
		t.Errorf("flow-form translate glossary warned: %v", got)
	}
	if got := cfg.MediaTranslateGlossary["en"]["Navalny"]; got != "Navalny" {
		t.Errorf("MediaTranslateGlossary[en][Navalny] = %q, want Navalny (all: %v)", got, cfg.MediaTranslateGlossary)
	}
}

func TestStructuredKeyWarning_MisspelledSiblingStillWarns(t *testing.T) {
	cfg := loadCfg(t, gigaamProfile+
		"media:\n"+
		"  stt:\n"+
		"    language_providers: {ru: gigaam}\n"+
		"    language_providerz: {ru: gigaam}\n")
	got := unknownKeyWarnings(cfg)
	if len(got) != 1 {
		t.Fatalf("want exactly one unrecognized-key warning, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "1 unrecognized key(s)") || !strings.Contains(got[0], "media.stt.language_providerz") {
		t.Errorf("warning does not name the one misspelled key: %s", got[0])
	}
}

// The yaml.v3 pass reads only the nested `media:` block. A dotted spelling of a
// side-parsed map key is read by no parser, so it must keep the
// unrecognized-key warning instead of a silent drop.
func TestStructuredKeyWarning_DottedSideParsedKeysWarn(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		key  string
	}{
		{
			name: "top-level flow language_providers",
			yaml: gigaamProfile + "media.stt.language_providers: {ru: gigaam}\n",
			key:  "media.stt.language_providers",
		},
		{
			name: "top-level block language_providers",
			yaml: gigaamProfile + "media.stt.language_providers:\n  ru: gigaam\n",
			key:  "media.stt.language_providers.ru",
		},
		{
			name: "top-level child language_providers",
			yaml: gigaamProfile + "media.stt.language_providers.ru: gigaam\n",
			key:  "media.stt.language_providers.ru",
		},
		{
			name: "media-level dotted flow language_providers",
			yaml: gigaamProfile + "media:\n  stt.language_providers: {ru: gigaam}\n",
			key:  "stt.language_providers",
		},
		{
			name: "top-level flow translate glossary",
			yaml: "media.translate.glossary: {en: {Navalny: Navalny}}\n",
			key:  "media.translate.glossary",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadCfg(t, tc.yaml)
			if len(cfg.MediaSTTLanguageProviders) != 0 || len(cfg.MediaTranslateGlossary) != 0 {
				t.Fatalf("dotted spelling was read: providers=%v glossary=%v", cfg.MediaSTTLanguageProviders, cfg.MediaTranslateGlossary)
			}
			got := unknownKeyWarnings(cfg)
			if len(got) != 1 {
				t.Fatalf("want exactly one unrecognized-key warning, got %d: %v", len(got), got)
			}
			if !strings.Contains(got[0], "1 unrecognized key(s)") || !strings.Contains(got[0], tc.key) {
				t.Errorf("warning does not name %s: %s", tc.key, got[0])
			}
		})
	}
}

// A glossary in block form under media: and translate: is read by the yaml.v3
// pass, so its term keys must not warn.
func TestStructuredKeyWarning_TranslateGlossaryBlockForm(t *testing.T) {
	cfg := loadCfg(t, ""+
		"media:\n"+
		"  translate:\n"+
		"    glossary:\n"+
		"      en:\n"+
		"        Navalny: Navalny\n")
	if got := unknownKeyWarnings(cfg); len(got) != 0 {
		t.Errorf("block-form translate glossary warned: %v", got)
	}
	if got := cfg.MediaTranslateGlossary["en"]["Navalny"]; got != "Navalny" {
		t.Errorf("MediaTranslateGlossary[en][Navalny] = %q, want Navalny (all: %v)", got, cfg.MediaTranslateGlossary)
	}
}

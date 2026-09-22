package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// media.stt.language_scope (SPEC §8.2.2, dir2mcp #1029) selects whether the
// source language is resolved, routed and coverage-checked once per recording
// (item, the §8.2.1 behaviour) or once per decode window (window).

// TestMediaSTTLanguageScope_Default pins the shipped default: item, so an
// existing corpus is decoded exactly as before.
func TestMediaSTTLanguageScope_Default(t *testing.T) {
	if got := config.Default().MediaSTTLanguageScope; got != "item" {
		t.Errorf("default media.stt.language_scope = %q, want item", got)
	}
}

// TestMediaSTTLanguageScope_Validation accepts the two values, normalizes empty
// and case, and rejects anything else. A misspelt scope that silently fell back
// to item would leave an operator believing a mixed-language corpus is routed
// per window when it is not.
func TestMediaSTTLanguageScope_Validation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "empty normalizes to item", value: "", want: "item"},
		{name: "item is accepted", value: "item", want: "item"},
		{name: "window is accepted", value: "window", want: "window"},
		{name: "case and whitespace are normalized", value: "  WINDOW ", want: "window"},
		{name: "an unknown scope is rejected", value: "segment", wantErr: true},
		{name: "a boolean is rejected", value: "true", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.MediaSTTLanguageScope = tc.value
			err := cfg.Validate()
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "media.stt.language_scope") {
					t.Fatalf("Validate() = %v, want a media.stt.language_scope error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() rejected %q: %v", tc.value, err)
			}
			if cfg.MediaSTTLanguageScope != tc.want {
				t.Errorf("normalized scope = %q, want %q", cfg.MediaSTTLanguageScope, tc.want)
			}
		})
	}
}

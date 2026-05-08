package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestResolveVersion_PrefersInjectedOverDebugInfo(t *testing.T) {
	info := &debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}}
	got := resolveVersion("0.5.2", info)
	if got != "0.5.2" {
		t.Errorf("injected version should win; got %q want %q", got, "0.5.2")
	}
}

func TestResolveVersion_FallsBackToMainVersion(t *testing.T) {
	info := &debug.BuildInfo{Main: debug.Module{Version: "v0.5.2"}}
	got := resolveVersion(defaultVersion, info)
	if got != "v0.5.2" {
		t.Errorf("Main.Version should win when injected is the default; got %q", got)
	}
}

func TestResolveVersion_SkipsDevelSentinel(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "8869f0aabbcc1122334455"},
		},
	}
	got := resolveVersion(defaultVersion, info)
	if got != "dev-8869f0a" {
		t.Errorf("(devel) Main.Version should yield to vcs.revision; got %q want %q", got, "dev-8869f0a")
	}
}

func TestResolveVersion_FallsBackToVCSRevision(t *testing.T) {
	info := &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef"},
		},
	}
	got := resolveVersion(defaultVersion, info)
	if got != "dev-0123456" {
		t.Errorf("vcs.revision should be surfaced as dev-<sha7>; got %q want %q", got, "dev-0123456")
	}
}

func TestResolveVersion_ShortVCSRevisionIsIgnored(t *testing.T) {
	info := &debug.BuildInfo{
		Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abcd"}},
	}
	got := resolveVersion(defaultVersion, info)
	if got != defaultVersion {
		t.Errorf("vcs.revision shorter than 7 chars should be ignored; got %q", got)
	}
}

func TestResolveVersion_FinalFallbackIsDefault(t *testing.T) {
	got := resolveVersion(defaultVersion, nil)
	if got != defaultVersion {
		t.Errorf("with no debug info and default injected, must return placeholder; got %q", got)
	}
}

func TestResolveVersion_EmptyInjectedFallsThrough(t *testing.T) {
	info := &debug.BuildInfo{Main: debug.Module{Version: "v0.5.2"}}
	got := resolveVersion("", info)
	if got != "v0.5.2" {
		t.Errorf("empty injected (test/edge case) should fall through to debug info; got %q", got)
	}
}

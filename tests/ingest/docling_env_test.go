package tests

import (
	"slices"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
)

// envValue returns the value of key in an os.Environ-style slice, and whether
// it was present.
func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

func countKey(env []string, key string) int {
	n := 0
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && k == key {
			n++
		}
	}
	return n
}

func TestSanitizeDoclingEnv_DropsPythonPathInjectors(t *testing.T) {
	in := []string{
		"PATH=/usr/bin:/bin",
		"PYTHONPATH=/opt/conda/lib/python3.11/site-packages",
		"PYTHONHOME=/opt/conda",
		"HOME=/Users/stas",
	}
	out := ingest.SanitizeDoclingEnv(in)

	if _, ok := envValue(out, "PYTHONPATH"); ok {
		t.Errorf("PYTHONPATH should be removed, got it in: %v", out)
	}
	if _, ok := envValue(out, "PYTHONHOME"); ok {
		t.Errorf("PYTHONHOME should be removed, got it in: %v", out)
	}
	// Unrelated variables are preserved untouched.
	if v, ok := envValue(out, "PATH"); !ok || v != "/usr/bin:/bin" {
		t.Errorf("PATH should be preserved, got %q ok=%v", v, ok)
	}
	if v, ok := envValue(out, "HOME"); !ok || v != "/Users/stas" {
		t.Errorf("HOME should be preserved, got %q ok=%v", v, ok)
	}
}

func TestSanitizeDoclingEnv_ForcesNoUserSite(t *testing.T) {
	// A caller-provided PYTHONNOUSERSITE must be replaced with the canonical
	// value, with no duplicate entry left behind.
	in := []string{"PYTHONNOUSERSITE=0", "FOO=bar"}
	out := ingest.SanitizeDoclingEnv(in)

	if got := countKey(out, "PYTHONNOUSERSITE"); got != 1 {
		t.Fatalf("expected exactly one PYTHONNOUSERSITE entry, got %d in %v", got, out)
	}
	if v, _ := envValue(out, "PYTHONNOUSERSITE"); v != "1" {
		t.Errorf("PYTHONNOUSERSITE should be forced to 1, got %q", v)
	}
}

func TestSanitizeDoclingEnv_AddsNoUserSiteWhenAbsent(t *testing.T) {
	out := ingest.SanitizeDoclingEnv([]string{"PATH=/usr/bin"})
	if !slices.Contains(out, "PYTHONNOUSERSITE=1") {
		t.Errorf("PYTHONNOUSERSITE=1 should be added, got %v", out)
	}
}

func TestSanitizeDoclingEnv_HandlesMalformedEntries(t *testing.T) {
	// Entries without '=' (rare but legal in os.Environ) are passed through
	// rather than dropped or panicking.
	in := []string{"WEIRD", "PATH=/bin"}
	out := ingest.SanitizeDoclingEnv(in)
	if !slices.Contains(out, "WEIRD") {
		t.Errorf("malformed entry should be preserved, got %v", out)
	}
}

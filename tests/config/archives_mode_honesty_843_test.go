package tests

// `ingest.archives.mode` must not promise behavior dir2mcp does not have: #843.
//
// The shipped default is `deep`. That name says an archive nested inside an
// archive is expanded. dir2mcp expands the top level of one container and stops,
// so the default named a behavior the implementation never had, and an operator
// who read the key planned around indexed nested members that do not exist.
//
// Neither the default nor the meaning of a member changes here. Both come from
// the canonical SPEC §16.2 template, which names the members `off|shallow|deep`
// and defines what none of them does, so to move either one is a spec change and
// belongs in dirstral-spec first. What this repository owns is the retraction,
// and these tests pin it at the two places an operator meets the key:
//
//   - a generated config carries the retraction as a comment above the keys;
//   - a hand-written config that sets one gets a warning at load.
//
// tests/ingest/archives_mode_no_runtime_effect_843_test.go pins the other half
// on observable ingest output: every accepted value indexes the same documents.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// configRepoRoot walks up from this file to the directory holding go.mod.
func configRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("failed to locate repository root (go.mod)")
		}
		dir = parent
	}
}

// TestArchivesMode_DefaultMatchesTheREADME pins the default against the document
// that describes it, so the code and the prose cannot drift apart again. #843 is
// exactly that drift: the template said one thing and the runtime did another.
func TestArchivesMode_DefaultMatchesTheREADME(t *testing.T) {
	const want = "deep"
	if got := config.Default().IngestArchivesMode; got != want {
		t.Fatalf("the default ingest.archives.mode must stay %q (SPEC §16.2 template); got %q", want, got)
	}

	readme, err := os.ReadFile(filepath.Join(configRepoRoot(t), "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	text := string(readme)
	if !strings.Contains(text, "mode: "+want+"         # off|shallow|deep") {
		t.Errorf("README.md must show the default ingest.archives.mode as %q", want)
	}
	// The README must retract the promise in the same place it shows the value.
	for _, phrase := range []string{
		"No accepted value changes behavior yet.",
		"`ingest.archives.mode: deep` promises more than dir2mcp does",
		"**no recursion at all**",
		"`skip_reason=archive`",
	} {
		if !strings.Contains(text, phrase) {
			t.Errorf("README.md must still state %q so the default does not read as a promise", phrase)
		}
	}
}

// TestArchivesMode_GeneratedConfigCarriesTheRetraction proves the honesty note
// reaches the file the operator actually opens. Without it the generated config
// says `ingest_archives_mode: deep` and nothing else, and the name is the only
// thing the operator has to go on.
func TestArchivesMode_GeneratedConfigCarriesTheRetraction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".dir2mcp.yaml")
	cfg := config.Default()
	cfg.RootDir = dir
	cfg.StateDir = filepath.Join(dir, ".dir2mcp")
	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	body := string(raw)

	valueLine := "ingest_archives_mode: deep"
	at := strings.Index(body, valueLine)
	if at < 0 {
		t.Fatalf("generated config must still carry %q; got:\n%s", valueLine, body)
	}
	for _, phrase := range []string{
		"VALIDATED but INERT",
		"TOP LEVEL only",
		"skip_reason=archive",
	} {
		idx := strings.Index(body, phrase)
		if idx < 0 {
			t.Errorf("generated config must state %q above the mode keys; got:\n%s", phrase, body)
			continue
		}
		if idx > at {
			t.Errorf("the retraction %q must come BEFORE %q, or the operator reads the value first", phrase, valueLine)
		}
	}

	// The comment must not change what loads back: a generated config still
	// round-trips to the same value.
	reloaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("reload generated config: %v", err)
	}
	if reloaded.IngestArchivesMode != "deep" {
		t.Fatalf("generated config must round-trip ingest.archives.mode; got %q", reloaded.IngestArchivesMode)
	}
}

// loadWarnings returns the warning text of a hand-written config body.
func loadWarnings(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".dir2mcp.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	var lines []string
	for _, w := range cfg.Warnings {
		lines = append(lines, w.Error())
	}
	return strings.Join(lines, "\n")
}

// TestArchivesMode_OffInAHandWrittenConfigWarns covers the case the generated
// file cannot reach: an operator who writes `off` has decided that archive
// contents must not be indexed. dir2mcp expands them anyway, so the belief costs
// privacy, and there is a real action to take instead.
func TestArchivesMode_OffInAHandWrittenConfigWarns(t *testing.T) {
	// `OFF` is included because Validate() lowercases before it compares, so the
	// warning must fold the same way or an upper-case `off` slips past silently.
	for _, written := range []string{"off", "OFF"} {
		got := loadWarnings(t, "root_dir: .\ningest:\n  archives:\n    mode: "+written+"\n")
		if !strings.Contains(got, "ingest.archives.mode") {
			t.Fatalf("ingest.archives.mode=%s must warn; warnings=%q", written, got)
		}
		for _, phrase := range []string{
			"NOT honored",
			"still expanded at the top level",
			"security.path_excludes",
		} {
			if !strings.Contains(got, phrase) {
				t.Errorf("the warning must state %q so the operator knows what to do instead; got %q", phrase, got)
			}
		}
	}
}

// TestArchivesMode_NonOffValuesDoNotWarn is the warning-hygiene guard. `deep` is
// the shipped default and `shallow` withholds nothing, so neither is a decision
// the operator can improve on. A warning that fires on every start over a value
// with no better alternative is one operators learn to skip. Their retraction is
// the generated-config comment and the README instead.
func TestArchivesMode_NonOffValuesDoNotWarn(t *testing.T) {
	for _, value := range []string{"shallow", "deep"} {
		got := loadWarnings(t, "root_dir: .\ningest:\n  archives:\n    mode: "+value+"\n")
		if strings.Contains(got, "ingest.archives.mode") {
			t.Errorf("ingest.archives.mode=%q must load silently; got %q", value, got)
		}
	}
}

// TestArchivesMode_UnsetConfigDoesNotWarn is the false-positive guard. Most
// operators never write the key at all.
func TestArchivesMode_UnsetConfigDoesNotWarn(t *testing.T) {
	if got := loadWarnings(t, "root_dir: .\n"); strings.Contains(got, "ingest.archives.mode") {
		t.Fatalf("a config that never sets the key must not warn about it; got %q", got)
	}
}

// TestIngestModes_GeneratedConfigStillLoadsSilently guards the #628 contract
// this warning had to fit inside: a config dir2mcp itself wrote carries all four
// mode keys at their defaults, and none of those defaults is `off`, so it must
// still load with nothing to say.
func TestIngestModes_GeneratedConfigStillLoadsSilently(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".dir2mcp.yaml")
	cfg := config.Default()
	cfg.RootDir = dir
	cfg.StateDir = filepath.Join(dir, ".dir2mcp")
	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	for _, w := range loaded.Warnings {
		if strings.Contains(w.Error(), "mode") {
			t.Fatalf("a generated config must load silently (#628); got %v", w)
		}
	}
}

// TestArchivesMode_SnapshotDoesNotWarn keeps the warning aimed at operator
// intent. An effective-config snapshot is machine-written and always carries
// every key, so warning on it would fire on every start and say nothing about
// what the operator asked for. Same split as #661.
func TestArchivesMode_SnapshotDoesNotWarn(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.RootDir = dir
	cfg.StateDir = filepath.Join(dir, ".dir2mcp")
	path, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{})
	if err != nil {
		t.Fatalf("SaveEffectiveSnapshot: %v", err)
	}
	loaded, _, err := config.LoadEffectiveSnapshot(path)
	if err != nil {
		t.Fatalf("LoadEffectiveSnapshot: %v", err)
	}
	for _, w := range loaded.Warnings {
		if strings.Contains(w.Error(), "ingest.archives.mode") {
			t.Fatalf("a snapshot load must not warn about a per-format mode key; got %v", w)
		}
	}
}

// TestArchivesMode_InvalidValueNamesTheRealVocabulary keeps the rejection path
// honest too. A retraction that also stopped naming the accepted set would trade
// one unhelpful message for another.
func TestArchivesMode_InvalidValueNamesTheRealVocabulary(t *testing.T) {
	cfg := config.Default()
	cfg.IngestArchivesMode = "recursive"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("ingest.archives.mode=recursive must be rejected")
	}
	if !strings.Contains(err.Error(), "ingest.archives.mode") {
		t.Errorf("the error must name the key; got %v", err)
	}
	for _, accepted := range []string{"off", "shallow", "deep"} {
		if !strings.Contains(err.Error(), accepted) {
			t.Errorf("the error must list the accepted value %q; got %v", accepted, err)
		}
	}
}

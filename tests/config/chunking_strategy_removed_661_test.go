package tests

// chunking.strategy is no longer a recognized key: dir2mcp #661.
//
// The key was parsed, kept in the config struct, and written back to the config
// file and the effective snapshot. No runtime path ever read it, so
// `chunking.strategy: semantic` succeeded and chunking behavior did not change.
// The canonical spec defines no chunking-strategy selector either (it is absent
// from SPEC §16.2 and from bs-011), and the chunker selects its strategy from
// the doc type (SPEC §7.5).
//
// So the key leaves the recognized surface. It is now an unrecognized key like
// any other typo: load names it in a warning (#628) rather than accepting it in
// silence, and no generated file carries it forward. A hard startup error was
// rejected deliberately: #628 fixed exactly this class of problem with a
// warning, on the stated ground that an unknown key must not break an existing
// deployment.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// loadWithChunkingStrategy writes a config carrying the dead key in the given
// spelling and loads it.
func loadWithChunkingStrategy(t *testing.T, body string) config.Config {
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
	return cfg
}

// TestChunkingStrategy_FlatSpellingIsReportedUnrecognized is the headline case.
// Before the fix the key was accepted without a word and did nothing.
func TestChunkingStrategy_FlatSpellingIsReportedUnrecognized(t *testing.T) {
	cfg := loadWithChunkingStrategy(t, "root_dir: .\nchunking_strategy: semantic\n")
	got := warningsText(cfg)
	if !strings.Contains(got, "unrecognized") || !strings.Contains(got, "chunking_strategy") {
		t.Fatalf("a config setting chunking_strategy must be told the key is unrecognized; warnings: %q", got)
	}
}

// TestChunkingStrategy_NestedSpellingIsReportedUnrecognized covers the canonical
// nested spelling, which reaches the same setter table through the `chunking`
// section.
func TestChunkingStrategy_NestedSpellingIsReportedUnrecognized(t *testing.T) {
	cfg := loadWithChunkingStrategy(t, "root_dir: .\nchunking:\n  strategy: semantic\n")
	got := warningsText(cfg)
	if !strings.Contains(got, "unrecognized") || !strings.Contains(got, "strategy") {
		t.Fatalf("the nested chunking.strategy spelling must be reported too; warnings: %q", got)
	}
}

// TestChunkingStrategy_LoadStillSucceeds pins the deliberate choice of a warning
// over an error: an existing deployment that carries the dead key still starts
// (#628), and the live chunking keys next to it still apply.
func TestChunkingStrategy_LoadStillSucceeds(t *testing.T) {
	cfg := loadWithChunkingStrategy(t,
		"root_dir: .\nchunking_strategy: semantic\nchunking_max_tokens: 700\nchunking_overlap_tokens: 70\n")
	if cfg.ChunkingMaxTokens != 700 || cfg.ChunkingOverlapTokens != 70 {
		t.Fatalf("the recognized chunking keys must still apply: max=%d overlap=%d",
			cfg.ChunkingMaxTokens, cfg.ChunkingOverlapTokens)
	}
}

// TestChunkingStrategy_IsNotWrittenBack pins that no generated file carries the
// dead knob forward. Before the fix every saved config and every effective
// snapshot published a `chunking_strategy:` line, which advertised a knob that
// did nothing.
func TestChunkingStrategy_IsNotWrittenBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".dir2mcp.yaml")
	cfg := config.Default()
	cfg.RootDir = dir
	cfg.StateDir = filepath.Join(dir, ".dir2mcp")
	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	if strings.Contains(string(saved), "chunking_strategy") {
		t.Fatalf("a saved config must not publish chunking_strategy:\n%s", string(saved))
	}
}

// TestChunkingStrategy_SnapshotDoesNotCarryIt is the same guarantee for the
// effective-config snapshot, which is the file support bundles and `status`
// report from.
func TestChunkingStrategy_SnapshotDoesNotCarryIt(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	cfg := config.Default()
	cfg.RootDir = dir
	cfg.StateDir = stateDir
	snapshotPath, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{})
	if err != nil {
		t.Fatalf("SaveEffectiveSnapshot: %v", err)
	}
	snapshot, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if strings.Contains(string(snapshot), "chunking_strategy") {
		t.Fatalf("the effective snapshot must not publish chunking_strategy:\n%s", string(snapshot))
	}
}

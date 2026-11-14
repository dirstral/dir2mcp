package tests

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §7.2 (spec 0.93.0): the effective-config snapshot of the previous run
// is the provenance of the previous secret policy for a corpus that an older
// release indexed. SaveEffectiveSnapshot keeps its lists once, before it
// replaces the snapshot.
func TestSecretPolicyProvenance_KeptOnceFromThePreviousSnapshot(t *testing.T) {
	stateDir := testutil.TempDir(t)
	cfg := config.Default()
	cfg.RootDir = testutil.TempDir(t)
	cfg.StateDir = stateDir

	if _, ok, err := config.PreviousSecretPolicy(stateDir); err != nil || ok {
		t.Fatalf("PreviousSecretPolicy on a new state dir = (ok=%v, %v), want no provenance", ok, err)
	}
	// An older release left a snapshot with secret_patterns and no allow list.
	old := "secret_patterns:\n  - 'AKIA[0-9A-Z]{16}'\n  - 'MYCORP-[0-9]{4}'\n"
	if err := os.WriteFile(filepath.Join(stateDir, config.EffectiveConfigSnapshotFile), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{}); err != nil {
		t.Fatalf("SaveEffectiveSnapshot: %v", err)
	}
	prov, ok, err := config.PreviousSecretPolicy(stateDir)
	if err != nil || !ok {
		t.Fatalf("PreviousSecretPolicy = (ok=%v, %v), want the old lists", ok, err)
	}
	if len(prov.Secret) != 2 || prov.Secret[1] != "MYCORP-[0-9]{4}" || len(prov.Allow) != 0 {
		t.Fatalf("provenance = %+v, want the two old patterns and no allow list", prov)
	}

	// A later save keeps the first provenance: the snapshot it replaces now
	// holds this run's lists, not the ones the corpus was indexed under.
	if _, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{}); err != nil {
		t.Fatalf("SaveEffectiveSnapshot: %v", err)
	}
	again, _, err := config.PreviousSecretPolicy(stateDir)
	if err != nil || len(again.Secret) != 2 {
		t.Fatalf("provenance after a second save = %+v (%v), want it unchanged", again, err)
	}
}

func TestSecretPolicyProvenance_NoneFromASnapshotWithoutSecretPatterns(t *testing.T) {
	stateDir := testutil.TempDir(t)
	cfg := config.Default()
	cfg.RootDir = testutil.TempDir(t)
	cfg.StateDir = stateDir
	if err := os.WriteFile(filepath.Join(stateDir, config.EffectiveConfigSnapshotFile), []byte("listen_addr: 127.0.0.1:0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{}); err != nil {
		t.Fatalf("SaveEffectiveSnapshot: %v", err)
	}
	if _, ok, err := config.PreviousSecretPolicy(stateDir); err != nil || ok {
		t.Fatalf("PreviousSecretPolicy = (ok=%v, %v), want no provenance", ok, err)
	}
	// The next save must not take this run's snapshot as the provenance: the
	// previous policy stays unknown.
	if _, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{}); err != nil {
		t.Fatalf("SaveEffectiveSnapshot: %v", err)
	}
	if _, ok, err := config.PreviousSecretPolicy(stateDir); err != nil || ok {
		t.Fatalf("PreviousSecretPolicy after a second save = (ok=%v, %v), want no provenance", ok, err)
	}
}

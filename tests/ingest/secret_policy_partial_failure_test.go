package tests

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Spec 0.93.0 re-evaluation: the cases a check over stored chunks misses, and
// the partial failures that must leave a retryable state (SPEC §7.2).

// execStateDB runs one statement against the state database at dbPath. The
// tests use it to install a trigger that makes one store write fail, which is
// how a partial failure is reproduced against the real store.
func execStateDB(t *testing.T, dbPath, stmt string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(context.Background(), stmt); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}

const (
	blockRetireTrigger = `CREATE TRIGGER block_retire BEFORE UPDATE OF deleted ON representations
		WHEN NEW.deleted = 1 BEGIN SELECT RAISE(ABORT, 'blocked by test'); END`
	unblockRetireTrigger = `DROP TRIGGER block_retire`
)

func blueheronPatterns() []string {
	return append(append([]string(nil), config.Default().SecretPatterns...), `BLUEHERON-[a-z]+-[0-9]{4}`)
}

func writeSidecarMedia(t *testing.T, root string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, "talk.vtt"),
		"WEBVTT\n\n00:00:00.000 --> 00:00:03.000\nThe codename is BLUEHERON-media-2026 for this project.\n")
}

// TestSecretPolicy_BroaderListReachesSourceOnlyMatch covers a match that only
// the source bytes hold. The member's chunk text is trimmed, so the trailing
// newline the new rule needs is in no chunk. A check over stored chunks misses
// it; a fresh read of the member does not.
func TestSecretPolicy_BroaderListReachesSourceOnlyMatch(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	html := "<html><body><p>The quarterly report covers delivery and revenue.</p></body></html>\n"
	data := buildZip(t, map[string]string{"page.html": html})
	if err := os.WriteFile(filepath.Join(root, "bundle.zip"), data, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	st, _ := newPolicyStore(t)

	runPolicyScan(t, policyConfig(root, stateDir, config.Default().SecretPatterns), st)
	requireSearchable(t, st, "bundle.zip/page.html")

	sourceOnly := append(append([]string(nil), config.Default().SecretPatterns...), `</html>\n`)
	runPolicyScan(t, policyConfig(root, stateDir, sourceOnly), st)
	requireWithheldAndRetired(t, st, "bundle.zip/page.html")
}

// TestSecretPolicy_BroaderListReachesMediaSidecarText covers a media file on
// the size-and-mtime fast path whose only text is a subtitle sidecar, which is
// derived text. Neither is re-read on an unchanged scan.
func TestSecretPolicy_BroaderListReachesMediaSidecarText(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeSidecarMedia(t, root)
	st, _ := newPolicyStore(t)

	runPolicyScan(t, policyConfig(root, stateDir, config.Default().SecretPatterns), st)
	requireSearchable(t, st, "talk.mp3")

	runPolicyScan(t, policyConfig(root, stateDir, blueheronPatterns()), st)
	requireWithheldAndRetired(t, st, "talk.mp3")
}

// TestSecretPolicy_RawRetireFailureIsRetried makes the retirement fail when a
// broader rule newly withholds a text document by its raw bytes. The document
// must not be settled as `secret_excluded` over live chunks: the next scan,
// with the store working again, must withhold it and retire its chunks.
func TestSecretPolicy_RawRetireFailureIsRetried(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "memo.md"), "Project codename: BLUEHERON-internal-2026\n")
	st, dbPath := newPolicyStore(t)

	runPolicyScan(t, policyConfig(root, stateDir, config.Default().SecretPatterns), st)
	requireSearchable(t, st, "memo.md")

	execStateDB(t, dbPath, blockRetireTrigger)
	runPolicyScan(t, policyConfig(root, stateDir, blueheronPatterns()), st)
	execStateDB(t, dbPath, unblockRetireTrigger)

	runPolicyScan(t, policyConfig(root, stateDir, blueheronPatterns()), st)
	requireWithheldAndRetired(t, st, "memo.md")
}

// TestSecretPolicy_DerivedRetireFailureIsRetried is the same partial failure
// on the derived-text path: a media file whose sidecar text a broader rule now
// matches. The withheld row must keep a blank done marker, so the fast path
// does not skip it and the next scan retries the retirement.
func TestSecretPolicy_DerivedRetireFailureIsRetried(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeSidecarMedia(t, root)
	st, dbPath := newPolicyStore(t)

	runPolicyScan(t, policyConfig(root, stateDir, config.Default().SecretPatterns), st)
	requireSearchable(t, st, "talk.mp3")

	execStateDB(t, dbPath, blockRetireTrigger)
	runPolicyScan(t, policyConfig(root, stateDir, blueheronPatterns()), st)
	execStateDB(t, dbPath, unblockRetireTrigger)

	runPolicyScan(t, policyConfig(root, stateDir, blueheronPatterns()), st)
	requireWithheldAndRetired(t, st, "talk.mp3")
}

// TestSecretPolicy_ResetFailureFailsTheScan makes the policy reset fail. The
// scan must report the failure and record nothing, so the next scan applies
// the changed policy.
func TestSecretPolicy_ResetFailureFailsTheScan(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "redirect.md"), redirectNote())
	st, dbPath := newPolicyStore(t)

	runPolicyScan(t, policyConfig(root, stateDir, preSpec0930SecretPatterns), st)
	requireStatus(t, st, "redirect.md", model.DocumentStatusSecretExcluded)

	execStateDB(t, dbPath, `CREATE TRIGGER block_policy BEFORE UPDATE ON settings
		WHEN NEW.key = '`+store.SecretPolicyIdentitySetting+`' BEGIN SELECT RAISE(ABORT, 'blocked by test'); END`)
	svc := mustNewIngestService(t, policyConfig(root, stateDir, config.Default().SecretPatterns), st)
	if err := svc.Run(context.Background()); err == nil {
		t.Fatal("Run succeeded although the secret policy reset failed; want an error")
	}
	requireStatus(t, st, "redirect.md", model.DocumentStatusSecretExcluded)
	execStateDB(t, dbPath, `DROP TRIGGER block_policy`)

	runPolicyScan(t, policyConfig(root, stateDir, config.Default().SecretPatterns), st)
	requireSearchable(t, st, "redirect.md")
}

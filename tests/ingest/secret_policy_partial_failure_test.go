package tests

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
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

	runPolicyScan(t, oldBinaryConfig(root, stateDir), st)
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

// TestSecretPolicy_TwoPhaseDerivationRetireFailureIsRetried makes the
// retirement fail in the two-phase derivation pass. The translated transcript
// matches a secret pattern, so that pass withholds the document, but it cannot
// retire the source transcript, which the transcription pass already indexed.
// The done marker stays stamped (blanking it would republish the clean
// transcript), so the fast path does not re-read the file. The next scan's
// derivation pass must retire the representations all the same.
func TestSecretPolicy_TwoPhaseDerivationRetireFailureIsRetried(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	mustWriteFile(t, filepath.Join(root, "audio", "one.mp3"), []byte("fake-audio-one"))
	dbPath := filepath.Join(stateDir, "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Config{
		RootDir:            root,
		StateDir:           stateDir,
		STTProvider:        "off",
		MediaBatchTwoPhase: true,
		// markerTranslator puts a marker into every translated line, so only
		// the translated transcript matches.
		SecretPatterns: []string{`BLUEHERON-[0-9]{4}`},
	}
	scan := func() {
		t.Helper()
		svc := mustNewIngestService(t, cfg, st)
		svc.SetTranscriber(&fakeTranscriber{text: "[00:00] intro line\n[00:02] second line"})
		svc.SetSTTIdentity("whisper", "whisper-large-v3")
		svc.SetTranscriptLanguage("de")
		svc.SetTranslator(markerTranslator{}, "mistral", "mistral-small-2506", []string{"en"})
		_ = svc.Run(context.Background())
	}

	execStateDB(t, dbPath, blockRetireTrigger)
	scan()
	execStateDB(t, dbPath, unblockRetireTrigger)
	doc := requireStatus(t, st, "audio/one.mp3", model.DocumentStatusSecretExcluded)
	if doc.ContentHash == "" {
		t.Fatal("the derivation pass blanked the done marker; the test needs the stamped-marker case")
	}
	reps, err := st.ActiveRepresentations(context.Background(), "audio/one.mp3")
	if err != nil {
		t.Fatalf("ActiveRepresentations: %v", err)
	}
	if len(reps) == 0 {
		t.Fatal("the blocked retirement left no live representation; the test cannot show the retry")
	}

	scan()
	requireWithheldAndRetired(t, st, "audio/one.mp3")
}

// markerTranslator answers the windowed translation contract and puts a
// BLUEHERON marker into every translated line.
type markerTranslator struct{}

func (markerTranslator) Generate(_ context.Context, prompt string) (string, error) {
	return strings.ReplaceAll(numberedEcho(fencedPayload(prompt)), ": translated:", ": BLUEHERON-2026 "), nil
}

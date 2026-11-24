package tests

import (
	"context"
	"database/sql"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/secretmatch"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// security.secret_allow_patterns at ingest (SPEC §7.2, spec 0.99.0): the
// upgrade path releases a document that only the redirect link withheld,
// without a read of every indexed document; the allow list applies to derived
// text; and a corpus with no recorded policy under a custom list is read again
// in full.

// runLoggedPolicyScan runs one scan and returns what it logged.
func runLoggedPolicyScan(t *testing.T, cfg config.Config, st *store.SQLiteStore) string {
	t.Helper()
	var logs syncBuffer
	svc := mustNewIngestService(t, cfg, st)
	svc.SetLogger(log.New(&logs, "", 0))
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return logs.String()
}

// pendingMarks counts the live documents that wait for re-evaluation.
func pendingMarks(t *testing.T, dbPath string) int {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM documents WHERE deleted = 0 AND secret_reeval_pending = 1`).Scan(&n); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	return n
}

const (
	scopeWithheldOnly = "withheld documents (and the archives that hold them)"
	scopeAll          = "withheld and indexed documents"
)

// TestSecretAllow_UpgradeReleasesRedirectAndReadsIndexed is the upgrade path:
// a document withheld only for its redirect link is released by the default
// allow list on the next scan. The store of the older release recorded no
// policy, so the previous policy is unknown: the scan reads every indexed
// document again. After that scan the policy is recorded, and a change that
// can only match less reads only the withheld documents.
func TestSecretAllow_UpgradeReleasesRedirectAndReadsIndexed(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "redirect.md"), redirectNote())
	writeFile(t, filepath.Join(root, "clean.md"), "The quarterly report covers delivery and revenue.\n")
	st, dbPath := newPolicyStore(t)

	runPolicyScan(t, oldBinaryConfig(root, stateDir), st)
	requireStatus(t, st, "redirect.md", model.DocumentStatusSecretExcluded)
	requireSearchable(t, st, "clean.md")
	forgetSecretPolicyIdentity(t, st, dbPath)

	logs := runLoggedPolicyScan(t, policyConfig(root, stateDir, config.Default().SecretPatterns), st)
	requireSearchable(t, st, "redirect.md")
	requireSearchable(t, st, "clean.md")
	if !strings.Contains(logs, scopeAll) {
		t.Fatalf("the upgrade from an unrecorded policy did not re-read the indexed documents; log:\n%s", logs)
	}
	if n := pendingMarks(t, dbPath); n != 0 {
		t.Fatalf("%d document(s) still wait for re-evaluation after the scan", n)
	}

	// Adding an operator allow pattern is also a narrowing change.
	cfg := policyConfig(root, stateDir, config.Default().SecretPatterns)
	cfg.SecretAllowPatterns = append(config.DefaultSecretAllowPatterns(), `feature_token=[A-Za-z0-9]+`)
	logs = runLoggedPolicyScan(t, cfg, st)
	if !strings.Contains(logs, scopeWithheldOnly) || strings.Contains(logs, scopeAll) {
		t.Fatalf("an added allow pattern re-read the indexed documents; log:\n%s", logs)
	}

	// Removing the allow list can match more: every indexed document is read
	// again, and the redirect document is withheld again.
	cfg.SecretAllowPatterns = []string{}
	logs = runLoggedPolicyScan(t, cfg, st)
	if !strings.Contains(logs, scopeAll) {
		t.Fatalf("a removed allow list did not re-read the indexed documents; log:\n%s", logs)
	}
	requireWithheldAndRetired(t, st, "redirect.md")
	requireSearchable(t, st, "clean.md")
	if n := pendingMarks(t, dbPath); n != 0 {
		t.Fatalf("%d document(s) still wait for re-evaluation after the scan", n)
	}
}

// TestSecretAllow_UnrecordedPolicyReadsIndexed covers a corpus that an older
// release indexed, with no recorded policy. The shipped defaults are active now, but that says
// nothing about the list the corpus was indexed under: here it left the AWS
// rule out. So the first scan reads every indexed document again and
// withholds the unchanged key document.
func TestSecretAllow_UnrecordedPolicyReadsIndexed(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "clean.md"), "The quarterly report covers delivery and revenue.\n")
	writeFile(t, filepath.Join(root, "keys.md"), "Deploy key AK"+"IA2ZZZZZZZZZZZZZZQ for the build.\n")
	st, dbPath := newPolicyStore(t)

	// The corpus was indexed under a custom list that left the AWS rule out.
	old := policyConfig(root, stateDir, []string{`BLUEHERON-[0-9]{4}`})
	old.SecretAllowPatterns = []string{}
	runPolicyScan(t, old, st)
	requireSearchable(t, st, "keys.md")
	forgetSecretPolicyIdentity(t, st, dbPath)

	logs := runLoggedPolicyScan(t, policyConfig(root, stateDir, config.Default().SecretPatterns), st)
	if !strings.Contains(logs, scopeAll) {
		t.Fatalf("an unrecorded policy did not re-read the indexed documents; log:\n%s", logs)
	}
	requireSearchable(t, st, "clean.md")
	requireWithheldAndRetired(t, st, "keys.md")
	if n := pendingMarks(t, dbPath); n != 0 {
		t.Fatalf("%d document(s) still wait for re-evaluation after the scan", n)
	}
}

// TestSecretAllow_ConfigOnlyCommandDoesNotSuppressThePass covers the upgrade
// of a corpus that was indexed under a custom list with no AWS rule. Before
// the first scan of this release, a command that only loads the config
// rewrites the effective-config snapshot with the NEW lists. The snapshot then
// holds lists that never evaluated the corpus, so it must not stand in for
// the previous policy: the first scan reads every indexed document again and
// withholds the unchanged key document.
func TestSecretAllow_ConfigOnlyCommandDoesNotSuppressThePass(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "clean.md"), "The quarterly report covers delivery and revenue.\n")
	writeFile(t, filepath.Join(root, "keys.md"), "Deploy key AK"+"IA2ZZZZZZZZZZZZZZQ for the build.\n")
	st, dbPath := newPolicyStore(t)

	old := policyConfig(root, stateDir, []string{`BLUEHERON-[0-9]{4}`})
	old.SecretAllowPatterns = []string{}
	runPolicyScan(t, old, st)
	requireSearchable(t, st, "keys.md")
	forgetSecretPolicyIdentity(t, st, dbPath)

	// The operator edits the config, and a config-only command of the OLDER
	// release writes its snapshot: `secret_patterns` holds the new list, and
	// no scan ran under it.
	next := policyConfig(root, stateDir, config.Default().SecretPatterns)
	var snapshot strings.Builder
	snapshot.WriteString("secret_patterns:\n")
	for _, p := range next.SecretPatterns {
		snapshot.WriteString("  - '" + strings.ReplaceAll(p, "'", "''") + "'\n")
	}
	if err := os.WriteFile(filepath.Join(stateDir, config.EffectiveConfigSnapshotFile), []byte(snapshot.String()), 0o600); err != nil {
		t.Fatalf("write the older release's snapshot: %v", err)
	}
	// Then config-only commands of this release load the config and save the
	// snapshot again, before any scan.
	for i := 0; i < 2; i++ {
		if _, err := config.SaveEffectiveSnapshot(next, config.SecretSourceMetadata{}); err != nil {
			t.Fatalf("SaveEffectiveSnapshot: %v", err)
		}
	}

	logs := runLoggedPolicyScan(t, next, st)
	if !strings.Contains(logs, scopeAll) {
		t.Fatalf("a config-only command before the first scan suppressed the pass; log:\n%s", logs)
	}
	requireSearchable(t, st, "clean.md")
	requireWithheldAndRetired(t, st, "keys.md")
}

// TestSecretAllow_UnrecordedCustomPolicyReadsIndexed covers a corpus indexed
// before spec 0.99.0 under a custom list. That list is not recorded and cannot
// be known, so the first scan reads every indexed document again.
func TestSecretAllow_UnrecordedCustomPolicyReadsIndexed(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "clean.md"), "The quarterly report covers delivery and revenue.\n")
	writeFile(t, filepath.Join(root, "keys.md"), "Deploy key AK"+"IA2ZZZZZZZZZZZZZZQ for the build.\n")
	st, dbPath := newPolicyStore(t)

	custom := []string{`BLUEHERON-[0-9]{4}`}
	old := policyConfig(root, stateDir, custom)
	old.SecretAllowPatterns = []string{}
	runPolicyScan(t, old, st)
	// The old list has no AWS rule, so it indexed the key document.
	requireSearchable(t, st, "keys.md")
	forgetSecretPolicyIdentity(t, st, dbPath)

	logs := runLoggedPolicyScan(t, policyConfig(root, stateDir, append(custom, `AKIA[0-9A-Z]{16}`)), st)
	if !strings.Contains(logs, scopeAll) {
		t.Fatalf("an unrecorded custom policy did not re-read the indexed documents; log:\n%s", logs)
	}
	requireSearchable(t, st, "clean.md")
	// The unchanged key document was read again and withheld.
	requireWithheldAndRetired(t, st, "keys.md")
}

// TestSecretAllow_AppliesToDerivedText pins the allow list on derived text: a
// media file whose subtitle sidecar quotes a redirect link is indexed, and one
// whose sidecar also holds a credential is withheld.
func TestSecretAllow_AppliesToDerivedText(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	tok := "to" + "ken"
	link := "https://www.youtube.com/redirect?event=video_description&redir_" + tok + "=QUFFLUhqbkJ3T2xYbE5fZ3lQdmZ4&q=x"
	writeFile(t, filepath.Join(root, "link.mp3"), "fake-audio-link")
	writeFile(t, filepath.Join(root, "link.vtt"),
		"WEBVTT\n\n00:00:00.000 --> 00:00:03.000\nThe source is "+link+"\n")
	writeFile(t, filepath.Join(root, "cred.mp3"), "fake-audio-cred")
	writeFile(t, filepath.Join(root, "cred.vtt"),
		"WEBVTT\n\n00:00:00.000 --> 00:00:03.000\nThe source is "+link+" and GITHUB_"+strings.ToUpper(tok)+"=Xy7Zq9WvXy7Zq9WvXy7Zq9WvXy7Zq9Wv\n")
	st, _ := newPolicyStore(t)

	runPolicyScan(t, policyConfig(root, stateDir, config.Default().SecretPatterns), st)
	requireSearchable(t, st, "link.mp3")
	requireStatus(t, st, "cred.mp3", model.DocumentStatusSecretExcluded)
}

// TestSecretAllow_ReplacedGenericRuleNeedsThePass pins SPEC §7.2 (spec
// 0.99.0): the test for "can only match less" is on the matches that stay
// after cancellation, not on one list alone. An allow pattern cancels only a
// match of the generic bearer-token default. When a custom token pattern
// replaces that default, also a narrower one, its matches have no allow
// pattern to cancel them. So the change needs the pass: a document with a
// redirect link, which the old set released, is read again and withheld.
func TestSecretAllow_ReplacedGenericRuleNeedsThePass(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "redirect.md"), redirectNote())
	writeFile(t, filepath.Join(root, "clean.md"), "The quarterly report covers delivery and revenue.\n")
	st, dbPath := newPolicyStore(t)

	defaults := config.Default().SecretPatterns
	runPolicyScan(t, policyConfig(root, stateDir, defaults), st)
	requireSearchable(t, st, "redirect.md")

	// Every string the custom pattern matches, the generic default matches too.
	custom := `(?i)_token=[A-Za-z0-9_.-]{20,}`
	replaced := make([]string, 0, len(defaults))
	found := false
	for _, p := range defaults {
		if p == secretmatch.GenericTokenPattern {
			replaced = append(replaced, custom)
			found = true
			continue
		}
		replaced = append(replaced, p)
	}
	if !found {
		t.Fatal("the shipped defaults do not hold the generic token rule")
	}
	old, err := secretmatch.Compile(defaults, config.DefaultSecretAllowPatterns())
	if err != nil {
		t.Fatal(err)
	}
	next, err := secretmatch.Compile(replaced, config.DefaultSecretAllowPatterns())
	if err != nil {
		t.Fatal(err)
	}
	if secretmatch.IsNarrower(old.Policy(), next.Policy()) {
		t.Fatal("a replaced generic token rule must not count as a change that can only match less")
	}

	logs := runLoggedPolicyScan(t, policyConfig(root, stateDir, replaced), st)
	if !strings.Contains(logs, scopeAll) {
		t.Fatalf("the replaced generic rule did not start the pass; log:\n%s", logs)
	}
	requireWithheldAndRetired(t, st, "redirect.md")
	requireSearchable(t, st, "clean.md")
	if n := pendingMarks(t, dbPath); n != 0 {
		t.Fatalf("%d document(s) still wait for re-evaluation after the scan", n)
	}
}

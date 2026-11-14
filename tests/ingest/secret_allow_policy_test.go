package tests

import (
	"context"
	"database/sql"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// security.secret_allow_patterns at ingest (SPEC §7.2, spec 0.93.0): the
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

// TestSecretAllow_UpgradeReleasesRedirectWithoutReadingIndexed is the upgrade
// path: a document withheld only for its redirect link is released by the
// default allow list on the next scan. The change can only match less, so the
// indexed documents are not read again and nothing is hidden.
func TestSecretAllow_UpgradeReleasesRedirectWithoutReadingIndexed(t *testing.T) {
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
	keepOldBinarySnapshot(t, policyConfig(root, stateDir, config.Default().SecretPatterns), preSpec0930SecretPatterns)

	logs := runLoggedPolicyScan(t, policyConfig(root, stateDir, config.Default().SecretPatterns), st)
	requireSearchable(t, st, "redirect.md")
	requireSearchable(t, st, "clean.md")
	if !strings.Contains(logs, scopeWithheldOnly) || strings.Contains(logs, scopeAll) {
		t.Fatalf("the upgrade re-read more than the withheld documents; log:\n%s", logs)
	}
	if n := pendingMarks(t, dbPath); n != 0 {
		t.Fatalf("%d document(s) were hidden by a change that can only match less", n)
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

// TestSecretAllow_UnrecordedPolicyWithoutProvenanceReadsIndexed covers a
// corpus that an older release indexed, with no recorded policy and no
// snapshot provenance. The shipped defaults are active now, but that says
// nothing about the list the corpus was indexed under (it may have left a
// default rule out), so the first scan reads every indexed document again.
func TestSecretAllow_UnrecordedPolicyWithoutProvenanceReadsIndexed(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "clean.md"), "The quarterly report covers delivery and revenue.\n")
	st, dbPath := newPolicyStore(t)

	runPolicyScan(t, oldBinaryConfig(root, stateDir), st)
	forgetSecretPolicyIdentity(t, st, dbPath)

	logs := runLoggedPolicyScan(t, policyConfig(root, stateDir, config.Default().SecretPatterns), st)
	if !strings.Contains(logs, scopeAll) {
		t.Fatalf("an unrecorded policy with no provenance did not re-read the indexed documents; log:\n%s", logs)
	}
	requireSearchable(t, st, "clean.md")
	if n := pendingMarks(t, dbPath); n != 0 {
		t.Fatalf("%d document(s) still wait for re-evaluation after the scan", n)
	}
}

// TestSecretAllow_UnrecordedCustomPolicyReadsIndexed covers a corpus indexed
// before spec 0.93.0 under a custom list. That list is not recorded and cannot
// be known, so the first scan reads every indexed document again.
func TestSecretAllow_UnrecordedCustomPolicyReadsIndexed(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "clean.md"), "The quarterly report covers delivery and revenue.\n")
	st, dbPath := newPolicyStore(t)

	custom := []string{`BLUEHERON-[0-9]{4}`}
	old := policyConfig(root, stateDir, custom)
	old.SecretAllowPatterns = []string{}
	runPolicyScan(t, old, st)
	forgetSecretPolicyIdentity(t, st, dbPath)

	logs := runLoggedPolicyScan(t, policyConfig(root, stateDir, append(custom, `AKIA[0-9A-Z]{16}`)), st)
	if !strings.Contains(logs, scopeAll) {
		t.Fatalf("an unrecorded custom policy did not re-read the indexed documents; log:\n%s", logs)
	}
	requireSearchable(t, st, "clean.md")
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

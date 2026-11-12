package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Upgrade path for the spec 0.93.0 secret-pattern defaults.
//
// A corpus indexed by an older binary holds documents withheld as
// `secret_excluded` under the old, boundary-less defaults. After an upgrade
// (daemon restart, incremental scan, no reindex) those documents must be
// evaluated again under the new defaults. Without the secret policy identity the
// incremental gates never re-read them: carrySecretExclusion keeps the verdict
// on an unchanged content hash, and the cheap-signal fast path skips the read.

// preSpec0930SecretPatterns is the default list an older binary shipped. The
// generic token rule and both key rules have no leading boundary.
var preSpec0930SecretPatterns = []string{
	`AKIA[0-9A-Z]{16}`,
	`(?i)(?:aws(?:[_\s.]{0,20})?secret(?:[_\s.]*(?:access[_\s.]*)?key)?|secret[_\s.]*access[_\s.]*key)\s*[:=]\s*[0-9A-Za-z/+=]{20,}`,
	`(?i)(?:authorization\s*[:=]\s*bearer\s+|(?:access|id|refresh)_token\s*[:=]\s*)[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`,
	`(?i)token\s*[:=]\s*[A-Za-z0-9_.-]{20,}`,
	`sk_[a-z0-9]{32}|api_[A-Za-z0-9]{32}`,
	`sk-[A-Za-z0-9-]{20,}`,
	`[A-Z][A-Z0-9_]*API_KEY\s*[:=]\s*\S{16,}`,
}

// redirectNote is a document that quotes a YouTube redirect link. The old
// generic token rule matched its `redir_token=` value; the new rule does not.
func redirectNote() string {
	return "Source video description link:\n" +
		"https://www.youtube.com/redirect?event=video_description&redir_" + "to" + "ken=" +
		"QUFFLUhqbWRkRThBZVXy7Zq9WvXy7Zq9WvXy7Zq9Wv&q=https%3A%2F%2Fexample.org%2Fpage\n" +
		"The interview covers the regional budget and the new bridge.\n"
}

// slugNote quotes a transliterated URL slug. The old `sk-` rule matched the
// `sk-` inside `krasnoyarsk-`.
func slugNote() string {
	return "Archive link: https://www.newstube.ru/media/b-elcin-i-konstantinova-v-krasnoyar" + "s" + "k-" +
		"podpisanie-dogovorov-o-sotrudnichestve\n"
}

func policyConfig(root, stateDir string, patterns []string) config.Config {
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = stateDir
	cfg.STTProvider = "off"
	cfg.SecretPatterns = append([]string(nil), patterns...)
	return cfg
}

func runPolicyScan(t *testing.T, cfg config.Config, st *store.SQLiteStore) {
	t.Helper()
	svc := mustNewIngestService(t, cfg, st)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func requireStatus(t *testing.T, st *store.SQLiteStore, relPath string, want model.DocumentStatus) model.Document {
	t.Helper()
	doc, err := st.GetDocumentByPath(context.Background(), relPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath(%s): %v", relPath, err)
	}
	if doc.Status != string(want) {
		t.Fatalf("%s status = %q (skip_reason %q), want %q", relPath, doc.Status, doc.SkipReason, want)
	}
	return doc
}

func requireSearchable(t *testing.T, st *store.SQLiteStore, relPath string) {
	t.Helper()
	doc := requireStatus(t, st, relPath, model.DocumentStatusOK)
	if doc.SkipReason != "" {
		t.Errorf("%s skip_reason = %q, want empty", relPath, doc.SkipReason)
	}
	if doc.ContentHash == "" {
		t.Errorf("%s content_hash is empty; the document did not settle", relPath)
	}
	reps, err := st.ActiveRepresentations(context.Background(), relPath)
	if err != nil {
		t.Fatalf("ActiveRepresentations(%s): %v", relPath, err)
	}
	if len(reps) == 0 {
		t.Errorf("%s has no live representation; it is not searchable", relPath)
	}
}

// TestSecretPolicy_UpgradeReevaluatesWithheldDocuments indexes a corpus under
// the pre-0.93.0 defaults, then runs an incremental scan under the shipped
// defaults on the same store. The false positives must come back as indexed.
func TestSecretPolicy_UpgradeReevaluatesWithheldDocuments(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "redirect.md"), redirectNote())
	writeFile(t, filepath.Join(root, "slug.md"), slugNote())
	writeFile(t, filepath.Join(root, "real.md"), "aws_key = AKIA2ZZZZZZZZZZZZZZQ\n")
	st := newRealStore(t)

	// Old binary.
	runPolicyScan(t, policyConfig(root, stateDir, preSpec0930SecretPatterns), st)
	requireStatus(t, st, "redirect.md", model.DocumentStatusSecretExcluded)
	requireStatus(t, st, "slug.md", model.DocumentStatusSecretExcluded)
	requireStatus(t, st, "real.md", model.DocumentStatusSecretExcluded)

	// New binary: incremental scan, no reindex.
	runPolicyScan(t, policyConfig(root, stateDir, config.Default().SecretPatterns), st)
	requireSearchable(t, st, "redirect.md")
	requireSearchable(t, st, "slug.md")
	// A real credential stays withheld under the new defaults.
	real := requireStatus(t, st, "real.md", model.DocumentStatusSecretExcluded)
	if real.ContentHash == "" {
		t.Errorf("real.md content_hash is empty after the re-evaluation; the withheld verdict did not settle")
	}

	recorded, err := st.SecretPolicyIdentity(context.Background())
	if err != nil {
		t.Fatalf("SecretPolicyIdentity: %v", err)
	}
	if recorded == "" {
		t.Fatal("secret policy identity was not recorded after the scan")
	}

	// Steady state: a second scan under the same patterns does not reset the
	// settled withheld row.
	runPolicyScan(t, policyConfig(root, stateDir, config.Default().SecretPatterns), st)
	again, err := st.SecretPolicyIdentity(context.Background())
	if err != nil {
		t.Fatalf("SecretPolicyIdentity: %v", err)
	}
	if again != recorded {
		t.Errorf("identity changed on a steady-state scan: %q -> %q", recorded, again)
	}
	requireStatus(t, st, "real.md", model.DocumentStatusSecretExcluded)
	requireSearchable(t, st, "redirect.md")
}

// TestSecretPolicy_UpgradeReevaluatesWithheldArchiveMember covers a withheld
// archive member: it is re-evaluated only when its container is re-extracted,
// so the reset must reach the container too.
func TestSecretPolicy_UpgradeReevaluatesWithheldArchiveMember(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	data := buildZip(t, map[string]string{"links.txt": redirectNote()})
	if err := os.WriteFile(filepath.Join(root, "bundle.zip"), data, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	st := newRealStore(t)

	runPolicyScan(t, policyConfig(root, stateDir, preSpec0930SecretPatterns), st)
	requireStatus(t, st, "bundle.zip/links.txt", model.DocumentStatusSecretExcluded)

	runPolicyScan(t, policyConfig(root, stateDir, config.Default().SecretPatterns), st)
	requireSearchable(t, st, "bundle.zip/links.txt")
}

// TestSecretPolicy_NarrowedOperatorListReevaluates covers the same mechanism
// for an operator edit rather than a binary upgrade: removing a custom pattern
// lets the document it withheld back into the index without a reindex.
func TestSecretPolicy_NarrowedOperatorListReevaluates(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "memo.md"), "Project codename: BLUEHERON-internal-2026\n")
	st := newRealStore(t)

	custom := append(append([]string(nil), config.Default().SecretPatterns...), `BLUEHERON-[a-z]+-[0-9]{4}`)
	runPolicyScan(t, policyConfig(root, stateDir, custom), st)
	requireStatus(t, st, "memo.md", model.DocumentStatusSecretExcluded)

	runPolicyScan(t, policyConfig(root, stateDir, config.Default().SecretPatterns), st)
	requireSearchable(t, st, "memo.md")
}

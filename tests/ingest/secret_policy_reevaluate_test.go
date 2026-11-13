package tests

import (
	"context"
	"database/sql"
	"errors"
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

// redirectNote is a document that quotes a YouTube redirect link. The generic
// token rule matches its `redir_token=` value. The old binary had no allow list,
// so it withheld the document; the default allow list now cancels that match.
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

// oldBinaryConfig is the policy of a binary before spec 0.93.0: its default
// secret patterns and no allow list.
func oldBinaryConfig(root, stateDir string) config.Config {
	cfg := policyConfig(root, stateDir, preSpec0930SecretPatterns)
	cfg.SecretAllowPatterns = []string{}
	return cfg
}

// newPolicyStore opens a store at a known path, so a test can edit the state
// database the way an older binary left it.
func newPolicyStore(t *testing.T) (*store.SQLiteStore, string) {
	t.Helper()
	dbPath := filepath.Join(testutil.TempDir(t), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, dbPath
}

// forgetSecretPolicyIdentity deletes the recorded identity, which is the state
// of a corpus indexed by a binary that predates it. The old-binary scan in these
// tests runs the current code with the old patterns, so it records an identity;
// deleting it makes the next scan an upgrade from an unrecorded policy, not a
// change between two recorded ones.
func forgetSecretPolicyIdentity(t *testing.T, st *store.SQLiteStore, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	res, err := db.ExecContext(context.Background(), `DELETE FROM settings WHERE key IN (?, ?, ?)`,
		store.SecretPolicyIdentitySetting, store.SecretPolicyPatternsSetting, store.SecretPolicyAllowPatternsSetting)
	if err != nil {
		t.Fatalf("delete secret policy identity: %v", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 3 {
		t.Fatalf("delete secret policy identity: rows=%d err=%v, want the 3 recorded rows", n, err)
	}
	recorded, err := st.SecretPolicyIdentity(context.Background())
	if err != nil {
		t.Fatalf("SecretPolicyIdentity: %v", err)
	}
	if recorded != "" {
		t.Fatalf("SecretPolicyIdentity after delete = %q, want empty", recorded)
	}
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
	st, dbPath := newPolicyStore(t)

	// Old binary.
	runPolicyScan(t, oldBinaryConfig(root, stateDir), st)
	requireStatus(t, st, "redirect.md", model.DocumentStatusSecretExcluded)
	requireStatus(t, st, "slug.md", model.DocumentStatusSecretExcluded)
	requireStatus(t, st, "real.md", model.DocumentStatusSecretExcluded)
	forgetSecretPolicyIdentity(t, st, dbPath)

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
	st, dbPath := newPolicyStore(t)

	runPolicyScan(t, oldBinaryConfig(root, stateDir), st)
	requireStatus(t, st, "bundle.zip/links.txt", model.DocumentStatusSecretExcluded)
	forgetSecretPolicyIdentity(t, st, dbPath)

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

// requireWithheldAndRetired fails unless the document is `secret_excluded` and
// has no live representation left for retrieval to return.
func requireWithheldAndRetired(t *testing.T, st *store.SQLiteStore, relPath string) {
	t.Helper()
	doc := requireStatus(t, st, relPath, model.DocumentStatusSecretExcluded)
	if doc.SkipReason != model.SkipReasonSecretExcluded {
		t.Errorf("%s skip_reason = %q, want %q", relPath, doc.SkipReason, model.SkipReasonSecretExcluded)
	}
	reps, err := st.ActiveRepresentations(context.Background(), relPath)
	if err != nil {
		t.Fatalf("ActiveRepresentations(%s): %v", relPath, err)
	}
	if len(reps) != 0 {
		t.Errorf("%s still has %d live representation(s); its text is searchable", relPath, len(reps))
	}
}

// TestSecretPolicy_BroaderListWithholdsIndexedDocuments is the other direction
// of SPEC §7.2: an operator adds a pattern, and the indexed documents it
// matches are withheld with their representations retired, on an incremental
// scan. The archive member is the case the content gate never re-reads: its
// container is unchanged, so only the post-scan sweep can reach it.
func TestSecretPolicy_BroaderListWithholdsIndexedDocuments(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "memo.md"), "Project codename: BLUEHERON-internal-2026\n")
	writeFile(t, filepath.Join(root, "clean.md"), "The quarterly report covers delivery and revenue.\n")
	data := buildZip(t, map[string]string{"inner.txt": "Codename BLUEHERON-archive-2026 in a member.\n"})
	if err := os.WriteFile(filepath.Join(root, "bundle.zip"), data, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	st, _ := newPolicyStore(t)

	runPolicyScan(t, policyConfig(root, stateDir, config.Default().SecretPatterns), st)
	requireSearchable(t, st, "memo.md")
	requireSearchable(t, st, "bundle.zip/inner.txt")
	before, err := st.SecretPolicyIdentity(context.Background())
	if err != nil {
		t.Fatalf("SecretPolicyIdentity: %v", err)
	}

	custom := append(append([]string(nil), config.Default().SecretPatterns...), `BLUEHERON-[a-z]+-[0-9]{4}`)
	runPolicyScan(t, policyConfig(root, stateDir, custom), st)
	requireWithheldAndRetired(t, st, "memo.md")
	requireWithheldAndRetired(t, st, "bundle.zip/inner.txt")
	requireSearchable(t, st, "clean.md")

	after, err := st.SecretPolicyIdentity(context.Background())
	if err != nil {
		t.Fatalf("SecretPolicyIdentity: %v", err)
	}
	if after == "" || after == before {
		t.Errorf("identity after the broader scan = %q (before %q); want a new recorded value", after, before)
	}

	// The verdict sticks on the next incremental scan under the same list.
	runPolicyScan(t, policyConfig(root, stateDir, custom), st)
	requireWithheldAndRetired(t, st, "memo.md")
	requireWithheldAndRetired(t, st, "bundle.zip/inner.txt")
}

// requireNotSearchable fails when the document has a live representation.
// Its row may be absent, skipped or deleted; only retrieval exposure counts.
func requireNotSearchable(t *testing.T, st *store.SQLiteStore, relPath string) {
	t.Helper()
	doc, err := st.GetDocumentByPath(context.Background(), relPath)
	if err == nil && doc.Status == string(model.DocumentStatusOK) && !doc.Deleted {
		t.Errorf("%s status = ok after the policy change; a gate other than the secret screen should keep it out", relPath)
	}
	reps, err := st.ActiveRepresentations(context.Background(), relPath)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("ActiveRepresentations(%s): %v", relPath, err)
	}
	if len(reps) != 0 {
		t.Errorf("%s has %d live representation(s); a released document skipped the other ingest gates", relPath, len(reps))
	}
}

// TestSecretPolicy_ReleasedDocumentKeepsTheOtherGates pins SPEC §7.2 (spec
// 0.93.0): a document that a policy change releases is treated as a new
// document, so every other §7.1 gate still applies. Three documents are
// withheld under a custom pattern. Then the operator drops the pattern and, in
// the same edit, adds a .gitignore rule, a path exclude and a size limit that
// each cover one of them. None of the three may become searchable. A fourth
// document that no other gate covers is the control.
func TestSecretPolicy_ReleasedDocumentKeepsTheOtherGates(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	memo := "Project codename: BLUEHERON-internal-2026\n"
	writeFile(t, filepath.Join(root, "ignored.md"), memo)
	writeFile(t, filepath.Join(root, "excluded.md"), memo)
	writeFile(t, filepath.Join(root, "control.md"), memo)
	big := make([]byte, 0, 2<<20)
	big = append(big, memo...)
	for len(big) < (3<<20)/2 {
		big = append(big, "The interview covers the regional budget and the new bridge.\n"...)
	}
	if err := os.WriteFile(filepath.Join(root, "big.md"), big, 0o600); err != nil {
		t.Fatalf("write big.md: %v", err)
	}
	st := newRealStore(t)

	custom := append(append([]string(nil), config.Default().SecretPatterns...), `BLUEHERON-[a-z]+-[0-9]{4}`)
	first := policyConfig(root, stateDir, custom)
	first.IngestGitignore = true
	runPolicyScan(t, first, st)
	for _, rel := range []string{"ignored.md", "excluded.md", "big.md", "control.md"} {
		requireStatus(t, st, rel, model.DocumentStatusSecretExcluded)
	}

	writeFile(t, filepath.Join(root, ".gitignore"), "ignored.md\n")
	second := policyConfig(root, stateDir, config.Default().SecretPatterns)
	second.IngestGitignore = true
	second.PathExcludes = append(append([]string(nil), second.PathExcludes...), "excluded.md")
	second.IngestMaxFileMB = 1
	runPolicyScan(t, second, st)

	requireSearchable(t, st, "control.md")
	for _, rel := range []string{"ignored.md", "excluded.md", "big.md"} {
		requireNotSearchable(t, st, rel)
	}
}

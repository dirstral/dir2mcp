package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/secretmatch"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §7.2 (spec 0.97.0): a document that waits for re-evaluation is released
// only by a write that the CURRENT policy evaluated. A scan that started under
// an earlier policy can finish after a broader policy is recorded; its writes
// must not release anything. And a document that an unfinished pass has not
// evaluated stays withheld when a later change arrives, also when that change
// can only match less.

func policyIdentity(t *testing.T, secret, allow []string) (string, secretmatch.Policy) {
	t.Helper()
	m, err := secretmatch.Compile(secret, allow)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	p := m.Policy()
	return p.Identity(), p
}

func isPending(t *testing.T, st interface {
	SecretReevalPendingPaths(context.Context, []string) (map[string]struct{}, error)
}, rel string) bool {
	t.Helper()
	pending, err := st.SecretReevalPendingPaths(context.Background(), []string{rel})
	if err != nil {
		t.Fatalf("SecretReevalPendingPaths: %v", err)
	}
	_, ok := pending[rel]
	return ok
}

// TestSecretPolicy_StaleScanDoesNotReleaseAPendingDocument runs the race in
// order: a broader policy is recorded while a scan under the earlier policy
// still has a document to finish. The late write must leave the document
// marked, with no content hash, so the next scan reads it under the new
// policy.
func TestSecretPolicy_StaleScanDoesNotReleaseAPendingDocument(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "memo.md"), "Project codename: BLUEHERON-internal-2026\n")
	writeFile(t, filepath.Join(root, "late.md"), "Second codename: BLUEHERON-archive-2026\n")
	st, _ := newPolicyStore(t)

	defaults := config.Default()
	earlier := policyConfig(root, stateDir, defaults.SecretPatterns)
	// The earlier scan indexed memo.md. late.md is not indexed yet: remove it
	// for this scan and put it back for the late write.
	lateBody, err := os.ReadFile(filepath.Join(root, "late.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "late.md")); err != nil {
		t.Fatal(err)
	}
	runPolicyScan(t, earlier, st)
	requireSearchable(t, st, "memo.md")
	if err := os.WriteFile(filepath.Join(root, "late.md"), lateBody, 0o644); err != nil {
		t.Fatal(err)
	}

	// A broader policy is recorded: every indexed document is marked.
	broader := append(append([]string(nil), defaults.SecretPatterns...), `BLUEHERON-[a-z]+-[0-9]{4}`)
	identity, policy := policyIdentity(t, broader, defaults.SecretAllowPatterns)
	if _, err := st.ResetForSecretPolicy(ctx, identity, policy.Secret, policy.Allow, true); err != nil {
		t.Fatalf("ResetForSecretPolicy: %v", err)
	}
	if !isPending(t, st, "memo.md") {
		t.Fatal("memo.md is not marked after the broader reset")
	}

	// The scan that started under the earlier policy finishes its documents.
	stale := mustNewIngestService(t, earlier, st)
	earlierMatcher, err := secretmatch.Compile(earlier.SecretPatterns, earlier.SecretAllowPatterns)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"memo.md", "late.md"} {
		info, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		f := ingest.DiscoveredFile{AbsPath: filepath.Join(root, rel), RelPath: rel, SizeBytes: info.Size(), MTimeUnix: info.ModTime().Unix(), Mode: info.Mode()}
		if err := stale.ProcessDocument(ctx, f, earlierMatcher, false); err != nil {
			t.Fatalf("ProcessDocument(%s): %v", rel, err)
		}
		if !isPending(t, st, rel) {
			t.Fatalf("%s was released by a scan that screened with the earlier policy", rel)
		}
		doc, err := st.GetDocumentByPath(ctx, rel)
		if err != nil || doc.ContentHash != "" {
			t.Fatalf("%s kept content hash %q (%v); the next scan would not read it again", rel, doc.ContentHash, err)
		}
	}

	// The scan under the recorded policy decides both documents.
	runPolicyScan(t, policyConfig(root, stateDir, broader), st)
	requireWithheldAndRetired(t, st, "memo.md")
	requireWithheldAndRetired(t, st, "late.md")
}

// TestSecretPolicy_NarrowingChangeKeepsUnevaluatedDocumentsWithheld covers a
// broader change whose pass did not finish, followed by a change that can only
// match less. The documents the pass did not evaluate stay marked through the
// second change, and the next scan evaluates them against the current policy.
func TestSecretPolicy_NarrowingChangeKeepsUnevaluatedDocumentsWithheld(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "clean.md"), "The quarterly report covers delivery and revenue.\n")
	writeFile(t, filepath.Join(root, "keys.md"), "Deploy key AK"+"IA2ZZZZZZZZZZZZZZQ for the build.\n")
	st, dbPath := newPolicyStore(t)

	// Indexed under a custom list with no AWS rule.
	first := policyConfig(root, stateDir, []string{`MYCORP-[0-9]{4}`})
	first.SecretAllowPatterns = []string{}
	runPolicyScan(t, first, st)
	requireSearchable(t, st, "keys.md")

	// A broader change: the pass starts (the reset) and does not finish.
	broaderSecret := []string{`MYCORP-[0-9]{4}`, `AKIA[0-9A-Z]{16}`, `BLUEHERON-[0-9]{4}`}
	identity, policy := policyIdentity(t, broaderSecret, nil)
	if _, err := st.ResetForSecretPolicy(ctx, identity, policy.Secret, policy.Allow, true); err != nil {
		t.Fatalf("ResetForSecretPolicy: %v", err)
	}
	if n := pendingMarks(t, dbPath); n != 2 {
		t.Fatalf("%d document(s) marked after the broader reset, want 2", n)
	}

	// A change that can only match less than the recorded one arrives: one
	// secret pattern is removed. Its reset must keep the marks.
	narrowerSecret := []string{`MYCORP-[0-9]{4}`, `AKIA[0-9A-Z]{16}`}
	narrowIdentity, narrowPolicy := policyIdentity(t, narrowerSecret, nil)
	if !secretmatch.IsNarrower(policy, narrowPolicy) {
		t.Fatal("the second change must count as narrower")
	}
	if _, err := st.ResetForSecretPolicy(ctx, narrowIdentity, narrowPolicy.Secret, narrowPolicy.Allow, false); err != nil {
		t.Fatalf("ResetForSecretPolicy: %v", err)
	}
	if n := pendingMarks(t, dbPath); n != 2 {
		t.Fatalf("%d document(s) marked after the narrowing change, want 2 (it must not release them)", n)
	}
	for _, rel := range []string{"clean.md", "keys.md"} {
		if !isPending(t, st, rel) {
			t.Fatalf("%s was released by a change that can only match less", rel)
		}
	}

	// The same sequence through ingest: the scan under the narrower policy
	// records it, evaluates each marked document against it, and only then
	// releases the clean one. The key document matches the current policy.
	narrow := policyConfig(root, stateDir, narrowerSecret)
	narrow.SecretAllowPatterns = []string{}
	runPolicyScan(t, narrow, st)
	requireSearchable(t, st, "clean.md")
	requireWithheldAndRetired(t, st, "keys.md")
	if n := pendingMarks(t, dbPath); n != 0 {
		t.Fatalf("%d document(s) still marked after the scan", n)
	}
}

package tests

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/secretmatch"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §7.2 (spec 0.93.0): after a configuration change that can match more,
// the next ingest scan marks every indexed document pending. A process can
// serve before that scan starts (up starts its transport before its ingest
// worker; ask serves with no scan at all). Retrieval therefore compares the
// policy the store recorded with the configured one, and withholds every
// indexed document while a broader move is not applied yet.

func policyOf(t *testing.T, secret, allow []string) secretmatch.Policy {
	t.Helper()
	m, err := secretmatch.Compile(secret, allow)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return m.Policy()
}

func TestSecretPolicyTransition_BroaderChangeHidesEverythingBeforeTheScan(t *testing.T) {
	f := newPendingFixture(t)
	ctx := context.Background()
	defaults := config.Default()

	// The corpus was indexed under the shipped defaults, and the store
	// recorded them. No document is pending.
	recorded := policyOf(t, defaults.SecretPatterns, defaults.SecretAllowPatterns)
	if _, err := f.store.ResetForSecretPolicy(ctx, recorded.Identity(), recorded.Secret, recorded.Allow, false); err != nil {
		t.Fatalf("ResetForSecretPolicy: %v", err)
	}
	if err := f.svc.SetSecretPolicy(defaults.SecretPatterns, defaults.SecretAllowPatterns); err != nil {
		t.Fatalf("SetSecretPolicy: %v", err)
	}
	if got := f.searchIDs(t); got != "[1 2 3]" {
		t.Fatalf("hits under the recorded policy = %s, want [1 2 3]", got)
	}

	// The operator adds a pattern. The process serves before its ingest
	// worker has run, so no document carries the pending mark yet.
	broader := append(append([]string(nil), defaults.SecretPatterns...), `BLUEHERON-[a-z]+-[0-9]{4}`)
	if err := f.svc.SetSecretPolicy(broader, defaults.SecretAllowPatterns); err != nil {
		t.Fatalf("SetSecretPolicy: %v", err)
	}
	if got := f.searchIDs(t); got != "[]" {
		t.Fatalf("hits before the scan applied a broader policy = %s, want []", got)
	}
	f.gen.lastPrompt = ""
	if _, err := f.svc.Ask(ctx, "what do the deployment notes say", model.SearchQuery{K: 5, Index: "text"}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	for _, part := range []string{"GITHUB_TO", splitKeyHalf2, "clean deployment notes"} {
		if strings.Contains(f.gen.lastPrompt, part) {
			t.Fatalf("the ask prompt holds %q before the scan applied a broader policy", part)
		}
	}
	for _, rel := range []string{"split.md", "clean.md"} {
		if _, err := f.svc.OpenFile(ctx, rel, model.Span{}, 200); !errors.Is(err, model.ErrForbidden) {
			t.Fatalf("open_file %s before the scan: err = %v, want ErrForbidden", rel, err)
		}
	}

	// The scan starts: it records the new policy and marks every indexed
	// document pending in one transaction. Then it decides clean.md again.
	active := policyOf(t, broader, defaults.SecretAllowPatterns)
	if _, err := f.store.ResetForSecretPolicy(ctx, active.Identity(), active.Secret, active.Allow, true); err != nil {
		t.Fatalf("ResetForSecretPolicy: %v", err)
	}
	if err := f.store.UpsertDocument(ctx, model.Document{RelPath: "clean.md", DocType: "md", Status: "ok", ContentHash: "h2"}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	if got := f.searchIDs(t); got != "[3]" {
		t.Fatalf("hits after the scan decided clean.md = %s, want [3]", got)
	}
}

func TestSecretPolicyTransition_NarrowerChangeHidesNothing(t *testing.T) {
	f := newPendingFixture(t)
	ctx := context.Background()
	defaults := config.Default()
	recorded := policyOf(t, defaults.SecretPatterns, defaults.SecretAllowPatterns)
	if _, err := f.store.ResetForSecretPolicy(ctx, recorded.Identity(), recorded.Secret, recorded.Allow, false); err != nil {
		t.Fatalf("ResetForSecretPolicy: %v", err)
	}
	// An added allow pattern can only match less.
	allow := append(append([]string(nil), defaults.SecretAllowPatterns...), `feature_token=[a-z]+`)
	if err := f.svc.SetSecretPolicy(defaults.SecretPatterns, allow); err != nil {
		t.Fatalf("SetSecretPolicy: %v", err)
	}
	if got := f.searchIDs(t); got != "[1 2 3]" {
		t.Fatalf("hits after a narrowing change = %s, want [1 2 3]", got)
	}
}

// TestSecretPolicyTransition_UnrecordedCorpus covers a corpus that an older
// release indexed: the store records no policy. With no provenance, the
// previous policy is unknown, so every indexed document is withheld until the
// scan marks them. With the provenance that the effective-config snapshot
// kept (the old defaults), the move to the shipped defaults is a narrowing,
// so nothing is hidden.
func TestSecretPolicyTransition_UnrecordedCorpus(t *testing.T) {
	defaults := config.Default()
	t.Run("no provenance", func(t *testing.T) {
		f := newPendingFixture(t)
		f.svc.SetStateDir(testutil.TempDir(t))
		if err := f.svc.SetSecretPolicy(defaults.SecretPatterns, defaults.SecretAllowPatterns); err != nil {
			t.Fatalf("SetSecretPolicy: %v", err)
		}
		if got := f.searchIDs(t); got != "[]" {
			t.Fatalf("hits with an unknown previous policy = %s, want []", got)
		}
	})
	t.Run("provenance of the old defaults", func(t *testing.T) {
		f := newPendingFixture(t)
		stateDir := testutil.TempDir(t)
		old := secretmatch.PreSpec0930DefaultPolicy()
		raw, err := json.Marshal(config.SecretPolicyProvenance{Secret: old.Secret, Allow: []string{}, Source: config.EffectiveConfigSnapshotFile})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, config.SecretPolicyProvenanceFile), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		f.svc.SetStateDir(stateDir)
		if err := f.svc.SetSecretPolicy(defaults.SecretPatterns, defaults.SecretAllowPatterns); err != nil {
			t.Fatalf("SetSecretPolicy: %v", err)
		}
		if got := f.searchIDs(t); got != "[1 2 3]" {
			t.Fatalf("hits after a proven narrowing = %s, want [1 2 3]", got)
		}
	})
}

// TestSecretPolicyTransition_EverySetterUpdatesTheIngestPolicy pins that the
// single-list setters feed the transition check too.
func TestSecretPolicyTransition_EverySetterUpdatesTheIngestPolicy(t *testing.T) {
	ctx := context.Background()
	defaults := config.Default()
	recorded := policyOf(t, defaults.SecretPatterns, defaults.SecretAllowPatterns)

	f := newPendingFixture(t)
	if _, err := f.store.ResetForSecretPolicy(ctx, recorded.Identity(), recorded.Secret, recorded.Allow, false); err != nil {
		t.Fatalf("ResetForSecretPolicy: %v", err)
	}
	broader := append(append([]string(nil), defaults.SecretPatterns...), `BLUEHERON-[a-z]+-[0-9]{4}`)
	if err := f.svc.SetSecretPatterns(broader); err != nil {
		t.Fatalf("SetSecretPatterns: %v", err)
	}
	if got := f.searchIDs(t); got != "[]" {
		t.Fatalf("hits after SetSecretPatterns made the policy broader = %s, want []", got)
	}

	g := newPendingFixture(t)
	if _, err := g.store.ResetForSecretPolicy(ctx, recorded.Identity(), recorded.Secret, recorded.Allow, false); err != nil {
		t.Fatalf("ResetForSecretPolicy: %v", err)
	}
	if err := g.svc.SetSecretAllowPatterns(nil); err != nil {
		t.Fatalf("SetSecretAllowPatterns: %v", err)
	}
	if got := g.searchIDs(t); got != "[]" {
		t.Fatalf("hits after SetSecretAllowPatterns removed the allow list = %s, want []", got)
	}
}

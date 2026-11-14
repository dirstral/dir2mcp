package tests

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/secretmatch"
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

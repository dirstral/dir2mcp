package ingest

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

func mustCompilePolicy(t *testing.T, patterns ...string) string {
	t.Helper()
	compiled, err := CompileSecretPatterns(patterns)
	if err != nil {
		t.Fatalf("CompileSecretPatterns: %v", err)
	}
	return secretPolicyIdentity(compiled)
}

// TestSecretPolicyIdentity pins the fingerprint contract: the order of the
// patterns and blank entries do not change it, and any change to the set does.
func TestSecretPolicyIdentity(t *testing.T) {
	base := mustCompilePolicy(t, `alpha[0-9]+`, `beta`)
	if !strings.HasPrefix(base, "v1:") {
		t.Fatalf("identity %q has no version prefix", base)
	}
	if got := mustCompilePolicy(t, `beta`, "  ", `alpha[0-9]+`); got != base {
		t.Errorf("reordered set identity = %q, want %q", got, base)
	}
	if got := mustCompilePolicy(t, `alpha[0-9]+`); got == base {
		t.Error("removing a pattern did not change the identity")
	}
	if got := mustCompilePolicy(t, `alpha[0-9]+`, `beta`, `gamma`); got == base {
		t.Error("adding a pattern did not change the identity")
	}
	if got := mustCompilePolicy(t, `alpha[0-9]*`, `beta`); got == base {
		t.Error("editing a pattern did not change the identity")
	}
	// One pattern with a literal newline is not the same set as two patterns.
	if mustCompilePolicy(t, "alpha", "beta") == mustCompilePolicy(t, "alpha\nbeta") {
		t.Error("two patterns and one pattern that joins them with a newline share an identity")
	}
}

// TestSecretPolicyNarrowing pins the narrower-set rule that lets an upgrade to
// the spec 0.93.0 defaults skip the indexed-document direction, and the cases
// that must not skip it.
func TestSecretPolicyNarrowing(t *testing.T) {
	defaults := append([]string(nil), config.Default().SecretPatterns...)
	inDefaults := make(map[string]bool, len(defaults))
	for _, p := range defaults {
		inDefaults[p] = true
	}
	inPrevious := make(map[string]bool, len(preSpec0930SecretPatterns))
	for _, p := range preSpec0930SecretPatterns {
		inPrevious[p] = true
	}
	for narrowed, old := range narrowedSecretPatterns {
		if !inDefaults[narrowed] {
			t.Errorf("narrowing %q is not a shipped default", narrowed)
		}
		if !inPrevious[old] {
			t.Errorf("narrowed pattern %q is not a pre-0.93.0 default", old)
		}
	}

	if !secretPolicyIsNarrower(preSpec0930SecretPatterns, defaults) {
		t.Error("the spec 0.93.0 defaults must count as narrower than the pre-0.93.0 defaults")
	}
	if !secretPolicyIsNarrower(defaults, defaults[:len(defaults)-1]) {
		t.Error("removing a pattern must count as narrower")
	}
	if secretPolicyIsNarrower(defaults, append(append([]string(nil), defaults...), `BLUEHERON-[0-9]+`)) {
		t.Error("adding a pattern must not count as narrower")
	}
	if secretPolicyIsNarrower(defaults, preSpec0930SecretPatterns) {
		t.Error("going back to the boundary-less defaults must not count as narrower")
	}
}

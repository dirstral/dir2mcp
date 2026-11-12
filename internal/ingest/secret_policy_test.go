package ingest

import (
	"strings"
	"testing"
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
}

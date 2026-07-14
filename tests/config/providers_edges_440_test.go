package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/provider"
)

// loadFileErr loads a config file and returns the (config, error) pair without
// fataling, so a test can assert on a startup CONFIG_INVALID failure.
func loadFileErr(t *testing.T, yaml string) (config.Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, p, yaml)
	return config.LoadFile(p)
}

// TestProviders_UnknownKindIsConfigInvalid pins issue #440 F7: a provider
// profile declaring an unrecognized/typo `kind:` must fail at startup with a
// clear CONFIG_INVALID that names the offending profile and the bad kind —
// rather than being silently un-selectable in `auto` and surfacing only as a
// generic "no eligible provider" error far from its cause.
func TestProviders_UnknownKindIsConfigInvalid(t *testing.T) {
	_, err := loadFileErr(t, "providers:\n  mytypo:\n    kind: opnai\n    api_key: xk\n")
	if err == nil {
		t.Fatal("unknown provider kind must fail config validation")
	}
	msg := err.Error()
	for _, want := range []string{"CONFIG_INVALID", "mytypo", "opnai"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q must name the profile + bad kind (missing %q)", msg, want)
		}
	}
}

// TestProviders_UnknownKindOverridingBuiltinIsConfigInvalid pins that a typo
// kind on an override of a BUILT-IN profile (not just a brand-new one) is also
// rejected, naming that profile.
func TestProviders_UnknownKindOverridingBuiltinIsConfigInvalid(t *testing.T) {
	_, err := loadFileErr(t, "providers:\n  gemini:\n    kind: gemni\n")
	if err == nil {
		t.Fatal("unknown kind overriding a builtin must fail config validation")
	}
	if msg := err.Error(); !strings.Contains(msg, "gemini") || !strings.Contains(msg, "gemni") {
		t.Fatalf("error %q must name the overridden builtin + bad kind", msg)
	}
}

// TestProviders_KnownKindsLoadCleanly is the negative control: every built-in
// profile carries a recognized kind, so a config with no exotic providers must
// still load without tripping the F7 gate.
func TestProviders_KnownKindsLoadCleanly(t *testing.T) {
	if _, err := loadFileErr(t, "providers:\n  extra:\n    kind: openai\n    base_url: http://x.local/v1\n    api_key: k\n"); err != nil {
		t.Fatalf("a config with only recognized kinds must load: %v", err)
	}
}

// blankBuiltinProviderCreds clears every built-in provider credential (embed + STT)
// so a
// user-only profile is the only eligible embed backend and thus wins auto
// selection — exercising the user-only precedence path.
func blankBuiltinProviderCreds(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"MISTRAL_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY",
		"GEMINI_API_KEY", "COHERE_API_KEY", "ANTHROPIC_API_KEY", "ELEVENLABS_API_KEY",
	} {
		t.Setenv(k, "")
	}
}

// TestProviders_UserOnlyPrecedenceIsDeclaredOrder pins issue #440 F8: user-only
// profiles take precedence in the order they are DECLARED in the YAML
// `providers:` mapping, not alphabetically. Two embed-capable user profiles are
// declared in reverse-alphabetical order (zeta before alpha); auto embed must
// resolve to the first declared (zeta). Alphabetical ordering — the prior
// behavior, a consequence of Go map order being lost on decode — would have
// picked alpha.
func TestProviders_UserOnlyPrecedenceIsDeclaredOrder(t *testing.T) {
	blankBuiltinProviderCreds(t)
	yaml := "providers:\n" +
		"  zeta:\n    kind: openai\n    base_url: http://zeta.local/v1\n    api_key: zk\n" +
		"  alpha:\n    kind: openai\n    base_url: http://alpha.local/v1\n    api_key: ak\n"
	r := loadCfg(t, yaml).Providers()
	p, err := r.Resolve(provider.CapEmbed)
	if err != nil {
		t.Fatalf("auto embed should resolve to a user-only profile: %v", err)
	}
	if p.Name != "zeta" {
		t.Fatalf("auto embed = %q, want first-declared %q (precedence must be declared order, not alphabetical)", p.Name, "zeta")
	}
}

// TestProviders_UserOnlyPrecedenceReversed is the mirror of the above: with the
// declaration order flipped (alpha before zeta), auto embed must resolve to
// alpha. Together the two cases prove the winner tracks declaration order
// rather than a fixed alphabetical sort.
func TestProviders_UserOnlyPrecedenceReversed(t *testing.T) {
	blankBuiltinProviderCreds(t)
	yaml := "providers:\n" +
		"  alpha:\n    kind: openai\n    base_url: http://alpha.local/v1\n    api_key: ak\n" +
		"  zeta:\n    kind: openai\n    base_url: http://zeta.local/v1\n    api_key: zk\n"
	r := loadCfg(t, yaml).Providers()
	p, err := r.Resolve(provider.CapEmbed)
	if err != nil {
		t.Fatalf("auto embed should resolve to a user-only profile: %v", err)
	}
	if p.Name != "alpha" {
		t.Fatalf("auto embed = %q, want first-declared %q", p.Name, "alpha")
	}
}

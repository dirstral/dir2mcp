package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/secrets"
)

// refByName finds a runtime secret ref by env var name.
func refByName(refs []config.RuntimeSecretRef, name string) (config.RuntimeSecretRef, bool) {
	for _, ref := range refs {
		if ref.Name == name {
			return ref, true
		}
	}
	return config.RuntimeSecretRef{}, false
}

// TestRuntimeSecretRefs_OnlyActiveFeatures_722 pins the capability-driven shape
// of the set: a plain local corpus on the default index depends on no
// non-provider runtime secret, so `service install` demands nothing new of an
// existing deployment.
func TestRuntimeSecretRefs_OnlyActiveFeatures_722(t *testing.T) {
	if refs := config.Default().RuntimeSecretRefs(); len(refs) != 0 {
		t.Errorf("a default local config should contribute no runtime secrets, got %+v", refs)
	}
}

// TestRuntimeSecretRefs_S3_722 pins the S3 source credentials, the case from the
// issue: the access key and secret are REQUIRED (validateSourceRuntimeSecrets
// rejects an s3 source without them) and the session token is optional.
func TestRuntimeSecretRefs_S3_722(t *testing.T) {
	cfg := config.Default()
	cfg.Source.Kind = "s3"
	cfg.Source.S3Bucket = "corpus"
	refs := cfg.RuntimeSecretRefs()

	for _, want := range []struct {
		name     string
		required bool
	}{
		{"AWS_ACCESS_KEY_ID", true},
		{"AWS_SECRET_ACCESS_KEY", true},
		{"AWS_SESSION_TOKEN", false},
	} {
		ref, found := refByName(refs, want.name)
		if !found {
			t.Fatalf("%s missing from RuntimeSecretRefs: %+v", want.name, refs)
		}
		if ref.Required != want.required {
			t.Errorf("%s Required = %v, want %v", want.name, ref.Required, want.required)
		}
		if !strings.Contains(ref.Feature, "s3") {
			t.Errorf("%s Feature = %q, should name the s3 source", want.name, ref.Feature)
		}
		if ref.Resolved {
			t.Errorf("%s should be unresolved when the config carries no value", want.name)
		}
	}

	// A resolved credential (from env/keychain/.env.local) is reported as such,
	// which is what lets install tell "only in this shell" from "already
	// persisted" without ever handling the value itself.
	cfg.Source.S3AccessKeyID = "AKIAEXAMPLE"
	ref, _ := refByName(cfg.RuntimeSecretRefs(), "AWS_ACCESS_KEY_ID")
	if !ref.Resolved {
		t.Error("a populated credential should be reported as resolved")
	}
}

// TestRuntimeSecretRefs_IndexBackends_722 pins the Tier C store credentials:
// the pgvector DSN is required (it has no config-file setter and the daemon
// refuses to start without it), the Qdrant key is optional (a local instance may
// be unsecured), and an embedded backend contributes neither.
func TestRuntimeSecretRefs_IndexBackends_722(t *testing.T) {
	for _, tc := range []struct {
		backend  string
		want     string
		required bool
	}{
		{"qdrant", "QDRANT_API_KEY", false},
		{"pgvector", "DIR2MCP_INDEX_PGVECTOR_DSN", true},
	} {
		cfg := config.Default()
		cfg.IndexBackend = tc.backend
		ref, found := refByName(cfg.RuntimeSecretRefs(), tc.want)
		if !found {
			t.Fatalf("backend=%s: %s missing", tc.backend, tc.want)
		}
		if ref.Required != tc.required {
			t.Errorf("backend=%s: %s Required = %v, want %v", tc.backend, tc.want, ref.Required, tc.required)
		}
	}

	cfg := config.Default()
	cfg.IndexBackend = "memory"
	for _, name := range []string{"QDRANT_API_KEY", "DIR2MCP_INDEX_PGVECTOR_DSN"} {
		if _, found := refByName(cfg.RuntimeSecretRefs(), name); found {
			t.Errorf("an embedded backend must not demand %s", name)
		}
	}
}

// TestRuntimeSecretRefs_OptionalFeatures_722 pins the broker URL and the x402
// facilitator token. Both are captured when present but never required: this
// build ships only the in-process brokers (which ignore BrokerURL), and a
// facilitator may accept unauthenticated calls. Requiring either would raise a
// false alarm on a valid deployment.
func TestRuntimeSecretRefs_OptionalFeatures_722(t *testing.T) {
	cfg := config.Default()
	cfg.DistributedEmbed.Enabled = true
	ref, found := refByName(cfg.RuntimeSecretRefs(), "DIR2MCP_DISTRIBUTED_EMBED_BROKER_URL")
	if !found {
		t.Fatal("the broker URL should be captured when distributed embedding is on")
	}
	if ref.Required {
		t.Error("the broker URL must not be required: no shipped broker reads it")
	}
	// Distributed embedding off (the default) contributes nothing.
	if _, found := refByName(config.Default().RuntimeSecretRefs(), "DIR2MCP_DISTRIBUTED_EMBED_BROKER_URL"); found {
		t.Error("distributed_embed.enabled=false must not demand a broker URL")
	}

	x := config.Default()
	x.X402.Mode = "required"
	x.X402.FacilitatorURL = "https://facilitator.example/v1"
	ref, found = refByName(x.RuntimeSecretRefs(), "DIR2MCP_X402_FACILITATOR_TOKEN")
	if !found {
		t.Fatal("the facilitator token should be captured when x402 gating is on")
	}
	if ref.Required {
		t.Error("the facilitator token must not be required")
	}
	// x402 off (the default) contributes nothing.
	if _, found := refByName(config.Default().RuntimeSecretRefs(), "DIR2MCP_X402_FACILITATOR_TOKEN"); found {
		t.Error("x402.mode=off must not demand a facilitator token")
	}
}

// TestRuntimeSecretRefs_CoverKeychainManagedNonProviderVars_722 is the
// anti-drift guard. Every non-provider credential the keychain backend manages
// exists precisely because it cannot live in the config file, which is the same
// rule RuntimeSecretRefs encodes. If a new one is added to secrets.ManagedEnvVars
// without a RuntimeSecretRefs entry, `service install` would silently stop
// persisting it and this test fails.
func TestRuntimeSecretRefs_CoverKeychainManagedNonProviderVars_722(t *testing.T) {
	cfg := config.Default()
	cfg.Source.Kind = "s3"
	cfg.Source.S3Bucket = "corpus"
	cfg.IndexBackend = "pgvector"
	covered := map[string]struct{}{}
	for _, ref := range cfg.RuntimeSecretRefs() {
		covered[ref.Name] = struct{}{}
	}
	// The Qdrant key needs the other backend selected.
	qdrant := config.Default()
	qdrant.IndexBackend = "qdrant"
	for _, ref := range qdrant.RuntimeSecretRefs() {
		covered[ref.Name] = struct{}{}
	}
	providerRefs := map[string]struct{}{}
	for _, name := range config.Default().ProviderEnvVarRefs() {
		providerRefs[name] = struct{}{}
	}

	for _, managed := range secrets.ManagedEnvVars() {
		if _, isProvider := providerRefs[managed]; isProvider {
			continue // already swept via ProviderEnvVarRefs
		}
		if _, ok := covered[managed]; !ok {
			t.Errorf("keychain-managed secret %q is not covered by RuntimeSecretRefs; "+
				"`dir2mcp service install` would not persist or report it", managed)
		}
	}
}

// TestRuntimeSecretNames_722 pins the convenience projection used by the CLI.
func TestRuntimeSecretNames_722(t *testing.T) {
	cfg := config.Default()
	cfg.Source.Kind = "s3"
	cfg.Source.S3Bucket = "corpus"
	names := cfg.RuntimeSecretNames()
	if len(names) != len(cfg.RuntimeSecretRefs()) {
		t.Fatalf("RuntimeSecretNames dropped entries: %v", names)
	}
	if names[0] != "AWS_ACCESS_KEY_ID" {
		t.Errorf("order must be deterministic, got %v", names)
	}
}

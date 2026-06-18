package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestDistributedEmbed_DefaultOff pins that distributed embedding is off by
// default (SPEC §8.7.4): a fresh config validates without any Tier-C requirement,
// so the local-first single-binary in-process loop runs unchanged (§1.2).
func TestDistributedEmbed_DefaultOff(t *testing.T) {
	cfg := config.Default()
	if cfg.DistributedEmbed.Enabled {
		t.Fatal("distributed embedding must default to OFF")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config must validate with distributed mode off: %v", err)
	}
}

// TestDistributedEmbed_RequiresTierC pins SPEC §8.7.4: enabling distributed mode
// with an embedded (single-node) Tier-A/B backend is CONFIG_INVALID — a worker
// pool requires a shared external Tier-C store.
func TestDistributedEmbed_RequiresTierC(t *testing.T) {
	for _, backend := range []string{"memory", "disk"} {
		cfg := config.Default()
		cfg.IndexBackend = backend
		cfg.DistributedEmbed.Enabled = true
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("backend %q: enabling distributed mode on an embedded backend must fail validation", backend)
		}
		if !strings.Contains(err.Error(), "Tier C") {
			t.Fatalf("backend %q: error should mention the Tier C requirement, got: %v", backend, err)
		}
	}
}

// TestDistributedEmbed_TierCAllowed pins that distributed mode validates with an
// external Tier-C backend (qdrant), the one configuration where Tier C becomes a
// prerequisite (SPEC §8.7.4).
func TestDistributedEmbed_TierCAllowed(t *testing.T) {
	cfg := config.Default()
	cfg.IndexBackend = "qdrant"
	cfg.Qdrant.URL = "http://localhost:6334"
	cfg.DistributedEmbed.Enabled = true
	cfg.DistributedEmbed.Broker = "sqlite"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("distributed mode with qdrant must validate: %v", err)
	}
	if cfg.DistributedEmbed.Broker != "sqlite" {
		t.Fatalf("broker normalized to %q, want sqlite", cfg.DistributedEmbed.Broker)
	}
}

// TestDistributedEmbed_UnsupportedBrokerRejected pins that an external broker
// (e.g. nats) is rejected at validation, in sync with buildEmbedBroker which can
// only construct the built-in memory/sqlite brokers — a value it cannot build
// must not validate (even with a broker_url), or startup would fail later.
func TestDistributedEmbed_UnsupportedBrokerRejected(t *testing.T) {
	cfg := config.Default()
	cfg.IndexBackend = "qdrant"
	cfg.Qdrant.URL = "http://localhost:6334"
	cfg.DistributedEmbed.Enabled = true
	cfg.DistributedEmbed.Broker = "nats"
	cfg.DistributedEmbed.BrokerURL = "nats://broker:4222"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("an unsupported (external) broker must fail validation in this build")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("error should explain the broker is unsupported, got: %v", err)
	}
}

// TestDistributedEmbed_FileRoundTrip pins config round-trip: the non-secret knobs
// load from a config file and the broker URL secret is NEVER read from disk
// (SPEC §16.1.1) — it is resolved only from the environment.
func TestDistributedEmbed_FileRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	path := tmp + "/.dir2mcp.yaml"
	writeFile(t, path, ""+
		"root_dir: ./repo\n"+
		"index_backend: qdrant\n"+
		"qdrant_url: http://localhost:6334\n"+
		"distributed_embed_enabled: true\n"+
		"distributed_embed_broker: sqlite\n"+
		"distributed_embed_sqlite_path: /tmp/q.db\n"+
		"distributed_embed_max_attempts: 7\n"+
		"distributed_embed_broker_url: file-supplied-should-be-ignored\n")

	t.Setenv("DIR2MCP_DISABLE_KEYCHAIN", "1")
	t.Setenv("DIR2MCP_DISTRIBUTED_EMBED_BROKER_URL", "nats://secret@broker:4222")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.DistributedEmbed.Enabled {
		t.Fatal("distributed_embed_enabled not loaded from file")
	}
	if cfg.DistributedEmbed.Broker != "sqlite" {
		t.Fatalf("broker = %q, want sqlite", cfg.DistributedEmbed.Broker)
	}
	if cfg.DistributedEmbed.BrokerSQLitePath != "/tmp/q.db" {
		t.Fatalf("sqlite path = %q", cfg.DistributedEmbed.BrokerSQLitePath)
	}
	if cfg.DistributedEmbed.MaxAttempts != 7 {
		t.Fatalf("max attempts = %d, want 7", cfg.DistributedEmbed.MaxAttempts)
	}
	if cfg.DistributedEmbed.BrokerURL != "nats://secret@broker:4222" {
		t.Fatalf("broker url = %q, want env-resolved value (file value must be ignored)", cfg.DistributedEmbed.BrokerURL)
	}
}

// TestDistributedEmbed_NestedYAMLSection pins that the nested YAML form
// (distributed_embed: with child keys) is honored, not just the dotted/flat
// aliases — distributed_embed must be a recognized map section.
func TestDistributedEmbed_NestedYAMLSection(t *testing.T) {
	tmp := t.TempDir()
	path := tmp + "/.dir2mcp.yaml"
	writeFile(t, path, ""+
		"root_dir: ./repo\n"+
		"index_backend: qdrant\n"+
		"qdrant_url: http://localhost:6334\n"+
		"distributed_embed:\n"+
		"  enabled: true\n"+
		"  broker: sqlite\n"+
		"  max_attempts: 9\n")

	t.Setenv("DIR2MCP_DISABLE_KEYCHAIN", "1")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.DistributedEmbed.Enabled {
		t.Fatal("nested distributed_embed.enabled was ignored")
	}
	if cfg.DistributedEmbed.Broker != "sqlite" {
		t.Fatalf("broker = %q, want sqlite", cfg.DistributedEmbed.Broker)
	}
	if cfg.DistributedEmbed.MaxAttempts != 9 {
		t.Fatalf("max_attempts = %d, want 9", cfg.DistributedEmbed.MaxAttempts)
	}
}

// TestDistributedEmbed_MalformedEnvRejected pins that an invalid distributed-embed
// env override is reported, not silently ignored, so automation cannot believe an
// override applied when it did not.
func TestDistributedEmbed_MalformedEnvRejected(t *testing.T) {
	t.Setenv("DIR2MCP_DISABLE_KEYCHAIN", "1")
	t.Run("max_attempts", func(t *testing.T) {
		t.Setenv("DIR2MCP_DISTRIBUTED_EMBED_MAX_ATTEMPTS", "not-a-number")
		if _, err := config.Load(""); err == nil {
			t.Fatal("malformed DIR2MCP_DISTRIBUTED_EMBED_MAX_ATTEMPTS must fail Load")
		}
	})
	t.Run("enabled", func(t *testing.T) {
		t.Setenv("DIR2MCP_DISTRIBUTED_EMBED_ENABLED", "yepyep")
		if _, err := config.Load(""); err == nil {
			t.Fatal("malformed DIR2MCP_DISTRIBUTED_EMBED_ENABLED must fail Load")
		}
	})
}

// TestDistributedEmbed_SnapshotOmitsBrokerURL pins SPEC §16.1.1: the broker URL
// secret is never written to the persisted snapshot, while the non-secret knobs
// are.
func TestDistributedEmbed_SnapshotOmitsBrokerURL(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.IndexBackend = "qdrant"
	cfg.Qdrant.URL = "http://localhost:6334"
	cfg.DistributedEmbed.Enabled = true
	cfg.DistributedEmbed.Broker = "sqlite"
	cfg.DistributedEmbed.BrokerURL = "nats://secret@broker:4222"

	path, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{})
	if err != nil {
		t.Fatalf("SaveEffectiveSnapshot: %v", err)
	}
	raw := readFileString(t, path)
	if strings.Contains(raw, "secret@broker") || strings.Contains(raw, "broker_url") {
		t.Fatalf("snapshot leaked the broker URL secret:\n%s", raw)
	}
	if !strings.Contains(raw, "distributed_embed_enabled: true") {
		t.Fatalf("snapshot missing the enable flag:\n%s", raw)
	}
}

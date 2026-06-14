package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

const pgvectorTestDSN = "postgres://user:secretpw@db.example.com:5432/vectors"

// writePgvectorConfig writes a config file selecting the pgvector backend with
// its non-sensitive invariants (schema/table). The DSN is a runtime-only secret
// (like the Qdrant api_key) and is deliberately NOT written to the config file:
// it is resolved via env/keychain/.env.local at load time.
func writePgvectorConfig(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, ".dir2mcp.yaml")
	writeFile(t, path, ""+
		"root_dir: ./repo\n"+
		"index_backend: pgvector\n"+
		"index_pgvector_schema: knowledge\n"+
		"index_pgvector_table: chunks\n")
	return path
}

// TestIndexBackend_PgvectorFileLoad verifies the non-sensitive pgvector index
// keys load from file, while a file-supplied DSN is ignored (never read from
// disk) and the DSN instead resolves through the env secret precedence
// (DIR2MCP_INDEX_PGVECTOR_DSN) on the env-aware Load path — mirroring how the
// Qdrant api_key is handled.
func TestIndexBackend_PgvectorFileLoad(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, ""+
		"root_dir: ./repo\n"+
		"index_backend: pgvector\n"+
		"index_pgvector_dsn: file-supplied-should-be-ignored\n"+
		"index_pgvector_schema: knowledge\n"+
		"index_pgvector_table: chunks\n")

	t.Setenv("DIR2MCP_DISABLE_KEYCHAIN", "1")
	t.Setenv("DIR2MCP_INDEX_PGVECTOR_DSN", pgvectorTestDSN)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.IndexBackend != "pgvector" {
		t.Fatalf("IndexBackend=%q want pgvector", cfg.IndexBackend)
	}
	if cfg.IndexPgvectorDSN != pgvectorTestDSN {
		t.Fatalf("IndexPgvectorDSN = %q, want env-resolved %q (file value must be ignored)", cfg.IndexPgvectorDSN, pgvectorTestDSN)
	}
	if cfg.IndexPgvectorSchema != "knowledge" || cfg.IndexPgvectorTable != "chunks" {
		t.Fatalf("schema/table not read: schema=%q table=%q", cfg.IndexPgvectorSchema, cfg.IndexPgvectorTable)
	}
}

// TestIndexBackend_DSNNotPersisted is the critical secret-hygiene check: the DSN
// (and its embedded password) is never written to disk, while the non-sensitive
// backend/schema/table keys survive a SaveFile→LoadFile roundtrip. The DSN is
// injected at runtime (env) since it is never read from a config file.
func TestIndexBackend_DSNNotPersisted(t *testing.T) {
	tmp := t.TempDir()
	cfg, err := config.LoadFile(writePgvectorConfig(t, tmp))
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	// Simulate the runtime-resolved secret the way the env path would.
	cfg.IndexPgvectorDSN = pgvectorTestDSN

	out := filepath.Join(tmp, "out.yaml")
	if err := config.SaveFile(out, cfg); err != nil {
		t.Fatalf("SaveFile failed: %v", err)
	}
	text := readFileString(t, out)
	if strings.Contains(text, "secretpw") || strings.Contains(text, "index_pgvector_dsn") {
		t.Fatalf("saved config leaked the DSN secret:\n%s", text)
	}
	if !strings.Contains(text, "index_backend: pgvector") {
		t.Fatalf("saved config missing index_backend:\n%s", text)
	}
	if !strings.Contains(text, "index_pgvector_schema: knowledge") || !strings.Contains(text, "index_pgvector_table: chunks") {
		t.Fatalf("saved config missing schema/table:\n%s", text)
	}

	// Reload the saved (DSN-free) file: backend/schema/table survive, DSN is now
	// empty (it must be re-sourced from env/secret at runtime).
	reloaded, err := config.LoadFile(out)
	if err != nil {
		t.Fatalf("reload LoadFile failed: %v", err)
	}
	if reloaded.IndexBackend != "pgvector" || reloaded.IndexPgvectorSchema != "knowledge" || reloaded.IndexPgvectorTable != "chunks" {
		t.Fatalf("roundtrip lost non-sensitive keys: %+v", reloaded)
	}
	if reloaded.IndexPgvectorDSN != "" {
		t.Fatalf("DSN must not survive persistence, got %q", reloaded.IndexPgvectorDSN)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	return string(raw)
}

// TestIndexBackend_RejectsUnknown verifies Validate rejects an unknown backend
// and normalizes the empty value to "memory".
func TestIndexBackend_RejectsUnknown(t *testing.T) {
	cfg := config.Default()
	cfg.IndexBackend = "redis"
	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected error for unknown backend, got nil")
	}

	cfg = config.Default()
	cfg.IndexBackend = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty backend should validate: %v", err)
	}
	if cfg.IndexBackend != "memory" {
		t.Fatalf("empty backend should normalize to memory, got %q", cfg.IndexBackend)
	}
}

// TestIndexBackend_PgvectorRejectsUnsafeIdentifier verifies an unsafe
// schema/table name is rejected at validation so it can never reach interpolated
// DDL.
func TestIndexBackend_PgvectorRejectsUnsafeIdentifier(t *testing.T) {
	cfg := config.Default()
	cfg.IndexBackend = "pgvector"
	cfg.IndexPgvectorTable = "chunks; DROP TABLE users"
	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected error for unsafe table identifier, got nil")
	}

	cfg = config.Default()
	cfg.IndexBackend = "pgvector"
	cfg.IndexPgvectorSchema = "a.b"
	if err := cfg.Validate(); err == nil {
		t.Fatalf("expected error for unsafe schema identifier, got nil")
	}
}

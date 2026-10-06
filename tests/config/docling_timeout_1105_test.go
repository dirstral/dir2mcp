package tests

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Issue #1105: ingest.docling.timeout_sec (env DIR2MCP_DOCLING_TIMEOUT_SEC)
// sets the time limit for one docling CLI call on one document. The default is
// 900 seconds, the fixed limit from before. The value must be greater than 0.

func TestDoclingTimeout_DefaultIs900(t *testing.T) {
	cfg := config.Default()
	if cfg.IngestDoclingTimeoutSec != 900 {
		t.Fatalf("default IngestDoclingTimeoutSec = %d, want 900", cfg.IngestDoclingTimeoutSec)
	}
	if config.DefaultDoclingTimeoutSec != 900 {
		t.Fatalf("DefaultDoclingTimeoutSec = %d, want 900", config.DefaultDoclingTimeoutSec)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
}

func TestDoclingTimeout_KeyAbsentKeepsDefault(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "root_dir: ./repo\ningest:\n  docling:\n    serve_url: \"\"\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if cfg.IngestDoclingTimeoutSec != config.DefaultDoclingTimeoutSec {
		t.Fatalf("IngestDoclingTimeoutSec = %d, want default %d", cfg.IngestDoclingTimeoutSec, config.DefaultDoclingTimeoutSec)
	}
}

func TestDoclingTimeout_KeySpellingsLoad(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"nested ingest.docling.timeout_sec", "root_dir: ./repo\ningest:\n  docling:\n    timeout_sec: 1800\n"},
		{"nested docling.timeout_sec", "root_dir: ./repo\ndocling:\n  timeout_sec: 1800\n"},
		{"flat ingest_docling_timeout_sec", "root_dir: ./repo\ningest_docling_timeout_sec: 1800\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			path := filepath.Join(tmp, ".dir2mcp.yaml")
			writeFile(t, path, tc.body)

			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatalf("LoadFile failed: %v", err)
			}
			if cfg.IngestDoclingTimeoutSec != 1800 {
				t.Fatalf("IngestDoclingTimeoutSec = %d, want 1800", cfg.IngestDoclingTimeoutSec)
			}
			for _, w := range cfg.Warnings {
				if strings.Contains(w.Error(), "timeout_sec") {
					t.Fatalf("recognized key must not warn, got: %v", w)
				}
			}
		})
	}
}

func TestDoclingTimeout_ZeroOrNegativeIsRejected(t *testing.T) {
	for _, value := range []string{"0", "-5"} {
		t.Run(value, func(t *testing.T) {
			tmp := t.TempDir()
			path := filepath.Join(tmp, ".dir2mcp.yaml")
			writeFile(t, path, "root_dir: ./repo\ningest:\n  docling:\n    timeout_sec: "+value+"\n")

			_, err := config.LoadFile(path)
			if err == nil {
				t.Fatalf("timeout_sec=%s must be rejected", value)
			}
			if !strings.Contains(err.Error(), "ingest.docling.timeout_sec must be greater than 0") {
				t.Fatalf("error must name the key and the rule, got: %v", err)
			}
		})
	}

	cfg := config.Default()
	cfg.IngestDoclingTimeoutSec = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate must reject IngestDoclingTimeoutSec=0")
	}
}

// TestDoclingTimeout_OverflowIsRejected checks the upper bound: a value that
// does not fit in a time.Duration would overflow to a negative limit, so every
// docling call would expire at once.
func TestDoclingTimeout_OverflowIsRejected(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("the overflow value does not fit in a 32-bit int")
	}
	cfg := config.Default()
	cfg.IngestDoclingTimeoutSec = int(config.MaxDoclingTimeoutSec)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the largest value that fits must validate: %v", err)
	}

	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	over := strconv.FormatInt(config.MaxDoclingTimeoutSec+1, 10)
	writeFile(t, path, "root_dir: ./repo\ningest:\n  docling:\n    timeout_sec: "+over+"\n")

	_, err := config.LoadFile(path)
	if err == nil {
		t.Fatalf("timeout_sec=%s must be rejected", over)
	}
	if !strings.Contains(err.Error(), "ingest.docling.timeout_sec must not be greater than") {
		t.Fatalf("error must name the key and the bound, got: %v", err)
	}
}

func TestDoclingTimeout_NonIntegerIsRejected(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "root_dir: ./repo\ningest:\n  docling:\n    timeout_sec: 15m\n")

	_, err := config.LoadFile(path)
	if err == nil {
		t.Fatal("a non-integer timeout_sec must be rejected")
	}
	if !strings.Contains(err.Error(), "ingest.docling.timeout_sec") {
		t.Fatalf("error must name the key, got: %v", err)
	}
}

func TestDoclingTimeout_EnvOverridesYAML(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "root_dir: ./repo\ningest:\n  docling:\n    timeout_sec: 1800\n")

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_DOCLING_TIMEOUT_SEC", " 2700 ")

		cfg, err := config.Load(".dir2mcp.yaml")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.IngestDoclingTimeoutSec != 2700 {
			t.Fatalf("IngestDoclingTimeoutSec = %d, want env value 2700", cfg.IngestDoclingTimeoutSec)
		}
	})
}

func TestDoclingTimeout_EnvNonIntegerWarnsAndKeepsYAML(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "root_dir: ./repo\ningest:\n  docling:\n    timeout_sec: 1800\n")

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_DOCLING_TIMEOUT_SEC", "fifteen")

		cfg, err := config.Load(".dir2mcp.yaml")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.IngestDoclingTimeoutSec != 1800 {
			t.Fatalf("IngestDoclingTimeoutSec = %d, want YAML value 1800 kept", cfg.IngestDoclingTimeoutSec)
		}
		var warned bool
		for _, w := range cfg.Warnings {
			if strings.Contains(w.Error(), "DIR2MCP_DOCLING_TIMEOUT_SEC") {
				warned = true
			}
		}
		if !warned {
			t.Fatalf("a non-integer DIR2MCP_DOCLING_TIMEOUT_SEC must warn, got warnings: %v", cfg.Warnings)
		}
	})
}

func TestDoclingTimeout_EnvZeroIsRejected(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "root_dir: ./repo\n")

	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_DOCLING_TIMEOUT_SEC", "0")

		_, err := config.Load(".dir2mcp.yaml")
		if err == nil {
			t.Fatal("DIR2MCP_DOCLING_TIMEOUT_SEC=0 must be rejected")
		}
		if !strings.Contains(err.Error(), "ingest.docling.timeout_sec must be greater than 0") {
			t.Fatalf("error must name the key and the rule, got: %v", err)
		}
	})
}

func TestDoclingTimeout_SaveFileRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.IngestDoclingTimeoutSec = 1800

	out := filepath.Join(tmp, "out.yaml")
	if err := config.SaveFile(out, cfg); err != nil {
		t.Fatalf("SaveFile failed: %v", err)
	}
	if text := readFileString(t, out); !strings.Contains(text, "ingest_docling_timeout_sec: 1800") {
		t.Fatalf("saved config missing ingest_docling_timeout_sec:\n%s", text)
	}
	reloaded, err := config.LoadFile(out)
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if reloaded.IngestDoclingTimeoutSec != 1800 {
		t.Fatalf("round trip lost the timeout: %d", reloaded.IngestDoclingTimeoutSec)
	}
}

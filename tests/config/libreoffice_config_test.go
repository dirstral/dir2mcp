package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// The libreoffice extraction engine (spec 0.93.0) adds the ingest.extractor
// value `libreoffice` and the knob ingest.libreoffice.timeout_sec (env
// DIR2MCP_LIBREOFFICE_TIMEOUT_SEC): the time limit for one headless LibreOffice
// conversion of one legacy .doc file. Default 300 seconds; it must be greater
// than 0.

func TestLibreOfficeConfig_ExtractorValueAccepted(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "root_dir: ./repo\ningest:\n  extractor: LibreOffice\n")
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("ingest.extractor: libreoffice must load: %v", err)
	}
	if cfg.IngestExtractor != "libreoffice" {
		t.Fatalf("IngestExtractor = %q, want normalized libreoffice", cfg.IngestExtractor)
	}
}

func TestLibreOfficeConfig_TimeoutDefault(t *testing.T) {
	cfg := config.Default()
	if cfg.IngestLibreOfficeTimeoutSec != 300 || config.DefaultLibreOfficeTimeoutSec != 300 {
		t.Fatalf("default timeout = %d (const %d), want 300", cfg.IngestLibreOfficeTimeoutSec, config.DefaultLibreOfficeTimeoutSec)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
}

func TestLibreOfficeConfig_TimeoutKeySpellingsLoad(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"nested ingest.libreoffice.timeout_sec", "root_dir: ./repo\ningest:\n  libreoffice:\n    timeout_sec: 90\n"},
		{"nested libreoffice.timeout_sec", "root_dir: ./repo\nlibreoffice:\n  timeout_sec: 90\n"},
		{"flat ingest_libreoffice_timeout_sec", "root_dir: ./repo\ningest_libreoffice_timeout_sec: 90\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
			writeFile(t, path, tc.body)
			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatalf("LoadFile failed: %v", err)
			}
			if cfg.IngestLibreOfficeTimeoutSec != 90 {
				t.Fatalf("IngestLibreOfficeTimeoutSec = %d, want 90", cfg.IngestLibreOfficeTimeoutSec)
			}
			for _, w := range cfg.Warnings {
				if strings.Contains(w.Error(), "timeout_sec") || strings.Contains(w.Error(), "libreoffice") {
					t.Fatalf("recognized key must not warn, got: %v", w)
				}
			}
		})
	}
}

func TestLibreOfficeConfig_TimeoutZeroIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, "root_dir: ./repo\ningest:\n  libreoffice:\n    timeout_sec: 0\n")
	_, err := config.LoadFile(path)
	if err == nil || !strings.Contains(err.Error(), "ingest.libreoffice.timeout_sec must be greater than 0") {
		t.Fatalf("timeout_sec=0 error = %v, want it to name the key and the rule", err)
	}
}

func TestLibreOfficeConfig_TimeoutEnvOverridesYAML(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, ".dir2mcp.yaml"), "root_dir: ./repo\ningest:\n  libreoffice:\n    timeout_sec: 90\n")
	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_LIBREOFFICE_TIMEOUT_SEC", " 45 ")
		cfg, err := config.Load(".dir2mcp.yaml")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.IngestLibreOfficeTimeoutSec != 45 {
			t.Fatalf("IngestLibreOfficeTimeoutSec = %d, want env value 45", cfg.IngestLibreOfficeTimeoutSec)
		}
	})
}

func TestLibreOfficeConfig_TimeoutEnvNonIntegerWarns(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, ".dir2mcp.yaml"), "root_dir: ./repo\ningest:\n  libreoffice:\n    timeout_sec: 90\n")
	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_LIBREOFFICE_TIMEOUT_SEC", "5m")
		cfg, err := config.Load(".dir2mcp.yaml")
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		if cfg.IngestLibreOfficeTimeoutSec != 90 {
			t.Fatalf("IngestLibreOfficeTimeoutSec = %d, want YAML value 90 kept", cfg.IngestLibreOfficeTimeoutSec)
		}
		warned := false
		for _, w := range cfg.Warnings {
			if strings.Contains(w.Error(), "DIR2MCP_LIBREOFFICE_TIMEOUT_SEC") {
				warned = true
			}
		}
		if !warned {
			t.Fatalf("a non-integer env value must warn; warnings = %v", cfg.Warnings)
		}
	})
}

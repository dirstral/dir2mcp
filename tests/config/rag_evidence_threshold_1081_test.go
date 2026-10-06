package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Issue #1081, SPEC §9.4.3 (spec 0.76.0): rag.evidence_threshold is "auto"
// (default) or a number in (0,1]; env DIR2MCP_RAG_EVIDENCE_THRESHOLD.

func TestEvidenceThreshold_DefaultIsAuto(t *testing.T) {
	cfg := config.Default()
	if cfg.RAGEvidenceThreshold != config.EvidenceThresholdAuto {
		t.Fatalf("default RAGEvidenceThreshold = %q, want auto", cfg.RAGEvidenceThreshold)
	}
	if auto, pinned := cfg.EvidenceThreshold(); !auto || pinned != 0 {
		t.Fatalf("EvidenceThreshold() = %v %v, want auto", auto, pinned)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
}

func TestEvidenceThreshold_KeySpellingsLoad(t *testing.T) {
	cases := []struct{ name, body string }{
		{"nested rag.evidence_threshold", "root_dir: ./repo\nrag:\n  evidence_threshold: 0.55\n"},
		{"flat rag_evidence_threshold", "root_dir: ./repo\nrag_evidence_threshold: 0.55\n"},
		{"flat evidence_threshold", "root_dir: ./repo\nevidence_threshold: 0.55\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
			writeFile(t, path, tc.body)
			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatalf("LoadFile: %v", err)
			}
			if auto, pinned := cfg.EvidenceThreshold(); auto || pinned != 0.55 {
				t.Fatalf("EvidenceThreshold() = %v %v, want pinned 0.55", auto, pinned)
			}
			for _, w := range cfg.Warnings {
				if strings.Contains(w.Error(), "evidence_threshold") {
					t.Fatalf("recognized key must not warn, got: %v", w)
				}
			}
		})
	}
}

func TestEvidenceThreshold_AutoSpellingsAndCase(t *testing.T) {
	for _, raw := range []string{"auto", "AUTO", " Auto ", ""} {
		auto, pinned, err := config.ParseEvidenceThreshold(raw)
		if err != nil || !auto || pinned != 0 {
			t.Fatalf("ParseEvidenceThreshold(%q) = %v %v %v, want auto", raw, auto, pinned, err)
		}
	}
	if auto, pinned, err := config.ParseEvidenceThreshold("1"); err != nil || auto || pinned != 1 {
		t.Fatalf("ParseEvidenceThreshold(1) = %v %v %v, want pinned 1", auto, pinned, err)
	}
}

func TestEvidenceThreshold_InvalidValuesAreRejected(t *testing.T) {
	for _, value := range []string{"0", "-0.2", "1.5", "abc", "NaN", "Inf", "off"} {
		t.Run(value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
			writeFile(t, path, "root_dir: ./repo\nrag:\n  evidence_threshold: "+value+"\n")
			cfg, err := config.LoadFile(path)
			if err == nil {
				err = cfg.Validate()
			}
			if err == nil || !strings.Contains(err.Error(), "rag.evidence_threshold") {
				t.Fatalf("value %q must be rejected naming the key, got: %v", value, err)
			}
		})
	}
}

func TestEvidenceThreshold_EnvOverridesTheFile(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, ".dir2mcp.yaml"), "root_dir: ./repo\nrag:\n  evidence_threshold: 0.9\n")
	testutil.WithWorkingDir(t, tmp, func() {
		t.Setenv("DIR2MCP_RAG_EVIDENCE_THRESHOLD", " 0.42 ")
		cfg, err := config.Load(".dir2mcp.yaml")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if auto, pinned := cfg.EvidenceThreshold(); auto || pinned != 0.42 {
			t.Fatalf("env override: EvidenceThreshold() = %v %v, want pinned 0.42", auto, pinned)
		}

		t.Setenv("DIR2MCP_RAG_EVIDENCE_THRESHOLD", "nope")
		cfg, err = config.Load(".dir2mcp.yaml")
		if err == nil {
			err = cfg.Validate()
		}
		if err == nil || !strings.Contains(err.Error(), "rag.evidence_threshold") {
			t.Fatalf("a bad env value must fail validation, got: %v", err)
		}
	})
}

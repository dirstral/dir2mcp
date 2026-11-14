package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/ingest"
)

// TestExtractionCovered_LibreOffice checks the per-format routing of the
// libreoffice engine (spec 0.93.0) through the exported coverage predicate:
// .doc is covered only when the engine is active and the policy permits it,
// and the libreoffice pin gives no fallback for other formats.
func TestExtractionCovered_LibreOffice(t *testing.T) {
	all := ingest.ActiveEngines{Structured: true, Pandoc: true, LibreOffice: true, FlatOCR: true}
	noLO := ingest.ActiveEngines{Structured: true, Pandoc: true, FlatOCR: true}
	cases := []struct {
		name    string
		policy  string
		engines ingest.ActiveEngines
		ext     string
		want    bool
	}{
		{"auto doc with libreoffice", "auto", all, ".doc", true},
		{"auto doc without libreoffice degrades", "auto", noLO, ".doc", false},
		{"auto docx stays covered", "auto", all, ".docx", true},
		{"auto odt stays covered", "auto", all, ".odt", true},
		{"libreoffice pin reads doc", "libreoffice", all, ".doc", true},
		{"libreoffice pin no fallback for docx", "libreoffice", all, ".docx", false},
		{"libreoffice pin no fallback for pdf", "libreoffice", all, ".pdf", false},
		{"pandoc pin does not read doc", "pandoc", all, ".doc", false},
		{"docling pin does not read doc", "docling", all, ".doc", false},
		{"libreoffice pin inactive engine degrades", "libreoffice", noLO, ".doc", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ingest.ExtractionCovered(tc.policy, tc.engines, tc.ext); got != tc.want {
				t.Fatalf("ExtractionCovered(%q, %+v, %q) = %v, want %v", tc.policy, tc.engines, tc.ext, got, tc.want)
			}
		})
	}
	if !ingest.LibreOfficeSupportsExt(".doc") || ingest.LibreOfficeSupportsExt(".docx") {
		t.Fatal("the libreoffice engine must read exactly .doc")
	}
}

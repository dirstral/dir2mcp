package ingest

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
)

func TestSelectExtractionRoute_LibreOffice(t *testing.T) {
	all := extractionAvailability{structured: true, pandoc: true, libreoffice: true, flatOCR: true}
	noLO := extractionAvailability{structured: true, pandoc: true, flatOCR: true}
	cases := []struct {
		name   string
		policy string
		avail  extractionAvailability
		ext    string
		want   extractionRoute
	}{
		{"auto doc with libreoffice", "auto", all, ".doc", routeLibreOffice},
		{"auto doc without libreoffice degrades", "auto", noLO, ".doc", routeDegrade},
		{"auto docx stays with docling", "auto", all, ".docx", routeStructured},
		{"auto odt stays with pandoc", "auto", all, ".odt", routePandoc},
		{"libreoffice pin reads doc", "libreoffice", all, ".doc", routeLibreOffice},
		{"libreoffice pin no fallback for docx", "libreoffice", all, ".docx", routeDegrade},
		{"libreoffice pin no fallback for pdf", "libreoffice", all, ".pdf", routeDegrade},
		{"libreoffice pin keeps html baseline", "libreoffice", all, ".html", routeRawText},
		{"pandoc pin does not read doc", "pandoc", all, ".doc", routeDegrade},
		{"docling pin does not read doc", "docling", all, ".doc", routeDegrade},
		{"libreoffice pin inactive engine degrades", "libreoffice", noLO, ".doc", routeDegrade},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectExtractionRoute(tc.policy, tc.avail, tc.ext); got != tc.want {
				t.Fatalf("selectExtractionRoute(%q, %+v, %q) = %v, want %v", tc.policy, tc.avail, tc.ext, got, tc.want)
			}
		})
	}
}

func TestLibreOfficeTimeout_DefaultWhenUnset(t *testing.T) {
	if got := NewLibreOfficeExtractor("", "", "", 0).timeout; got != defaultLibreOfficeTimeout {
		t.Fatalf("timeout = %s, want default %s", got, defaultLibreOfficeTimeout)
	}
	if int64(defaultLibreOfficeTimeout/time.Second) != config.DefaultLibreOfficeTimeoutSec {
		t.Fatalf("defaultLibreOfficeTimeout %s drifted from config.DefaultLibreOfficeTimeoutSec %d", defaultLibreOfficeTimeout, config.DefaultLibreOfficeTimeoutSec)
	}
}

func TestFileURL_AbsoluteAndEncoded(t *testing.T) {
	got := fileURL(filepath.Join(string(filepath.Separator)+"tmp", "a b", "profile"))
	if !strings.HasPrefix(got, "file:///") || !strings.Contains(got, "a%20b") {
		t.Fatalf("fileURL = %q, want an absolute, percent-encoded file URL", got)
	}
}

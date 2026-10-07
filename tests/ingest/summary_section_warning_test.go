package tests

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// captureDefaultLog runs fn with the default logger writing to a buffer and
// returns what fn logged. NewService logs the section warning through the
// default logger, because no service logger is set at construction time.
func captureDefaultLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prev)
		log.SetFlags(prevFlags)
	}()
	fn()
	return buf.String()
}

// TestSectionLevelWarning_NamesTheRealOutcome pins that the warning for the
// unimplemented `section` level states what is derived: with `document` also in
// the levels, only document-level summaries; with `section` alone, none. The old
// text claimed document-level summaries in both cases.
func TestSectionLevelWarning_NamesTheRealOutcome(t *testing.T) {
	for _, tc := range []struct {
		name      string
		levels    []string
		want      string
		forbidden string
	}{
		{
			name:      "section alone derives nothing",
			levels:    []string{config.HierarchicalLevelSection},
			want:      "no summaries are derived",
			forbidden: "only document-level summaries are derived",
		},
		{
			name:      "document and section derives document summaries",
			levels:    []string{config.HierarchicalLevelDocument, config.HierarchicalLevelSection},
			want:      "only document-level summaries are derived",
			forbidden: "no summaries are derived",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := hierarchicalConfig(testutil.TempDir(t))
			cfg.RetrievalHierarchicalLevels = tc.levels
			got := captureDefaultLog(t, func() {
				mustNewIngestService(t, cfg, &summaryStore{})
			})
			if !strings.Contains(got, `requests`) || !strings.Contains(got, `"section"`) {
				t.Fatalf("no section-level warning was logged; log:\n%s", got)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("warning does not say %q; log:\n%s", tc.want, got)
			}
			if strings.Contains(got, tc.forbidden) {
				t.Errorf("warning wrongly says %q; log:\n%s", tc.forbidden, got)
			}
		})
	}
}

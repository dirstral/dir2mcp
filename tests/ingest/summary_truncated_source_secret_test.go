package tests

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// The summary step sends a truncated source to the summarizer (SPEC §7.2,
// §9.7). An allow pattern can need context after the cut. The full source then
// passes the check, but the truncated source holds an uncovered token. The
// truncated source must be checked too.

// summaryAllowWithTail is an allow rule whose cover needs the PUBLIC-END tail.
const summaryAllowWithTail = `(?s)token=[A-Z]{20}.*PUBLIC-END`

func runTruncatedSummaryScan(t *testing.T, padding int) (*recordingSummarizer, func(string) bool) {
	t.Helper()
	root := testutil.TempDir(t)
	text := "token=ABCDEFGHIJKLMNOPQRST\n" + strings.Repeat("the quarterly budget report. ", padding/29+1) + "\nPUBLIC-END\n"
	writeFile(t, filepath.Join(root, "notes.txt"), text)
	st := newRealStore(t)
	cfg := hierarchicalConfig(testutil.TempDir(t))
	cfg.RootDir = root
	cfg.STTProvider = "off"
	cfg.SecretPatterns = append([]string(nil), config.Default().SecretPatterns...)
	cfg.SecretAllowPatterns = append(config.DefaultSecretAllowPatterns(), summaryAllowWithTail)
	svc := mustNewIngestService(t, cfg, st)
	sum := &recordingSummarizer{}
	svc.SetSummarizer(sum, "mistral", "mistral-small-latest")
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	sentToken := func(marker string) bool {
		sum.mu.Lock()
		defer sum.mu.Unlock()
		for _, p := range sum.prompts {
			if strings.Contains(p, marker) {
				return true
			}
		}
		return false
	}
	if padding > 24000 {
		requireWithheldAndRetired(t, st, "notes.txt")
	} else {
		requireStatus(t, st, "notes.txt", model.DocumentStatusOK)
	}
	return sum, sentToken
}

// TestSummary_TruncatedSourceLosesAllowContextIsWithheld: the allow context
// sits past the cut, so the truncated source holds an uncovered token. Nothing
// reaches the summarizer and the document is withheld.
func TestSummary_TruncatedSourceLosesAllowContextIsWithheld(t *testing.T) {
	t.Parallel()
	_, sent := runTruncatedSummaryScan(t, 30000)
	if sent("token=ABCDEFGHIJKLMNOPQRST") {
		t.Fatal("the summarizer received a truncated source with an uncovered token")
	}
}

// TestSummary_ShortSourceKeepsAllowContextIsSummarized: the whole source fits
// in the prompt, so the allow context stays and the summary is made.
func TestSummary_ShortSourceKeepsAllowContextIsSummarized(t *testing.T) {
	t.Parallel()
	sum, sent := runTruncatedSummaryScan(t, 1000)
	if !sent("PUBLIC-END") {
		sum.mu.Lock()
		n := len(sum.prompts)
		sum.mu.Unlock()
		t.Fatalf("the summarizer did not receive the clean source (%d prompt(s))", n)
	}
}

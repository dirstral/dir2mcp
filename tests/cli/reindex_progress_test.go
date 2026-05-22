package tests

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
)

// reindexNoopIngestor satisfies model.Ingestor and lets us drive
// runReindex from a test without spinning up the real ingest pipeline.
// Reindex returns immediately so the test exercises the progress
// scaffolding (initial line + done line) without requiring a long-
// running fake.
type reindexNoopIngestor struct{}

func (reindexNoopIngestor) Run(context.Context) error     { return nil }
func (reindexNoopIngestor) Reindex(context.Context) error { return nil }

// TestReindex_PrintsProgressAndDoneLines pins the new reindex UX:
// the command emits at least one `[reindex]` progress line and a
// trailing `[reindex] done:` summary line to stderr. Pre-fix the
// command was dead silent for the whole run, which forced the
// "open a second terminal and poll status" workaround the original
// user complained about.
func TestReindex_PrintsProgressAndDoneLines(t *testing.T) {
	tmp := t.TempDir()
	var stdout, stderr bytes.Buffer

	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewIngestor: func(_ config.Config, _ model.Store) (model.Ingestor, error) {
			return reindexNoopIngestor{}, nil
		},
	})

	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if code := app.RunWithContext(ctx, []string{"reindex"}); code != 0 {
			t.Fatalf("reindex exit=%d stderr=%q", code, stderr.String())
		}
	})

	errOut := stderr.String()
	if !strings.Contains(errOut, "[reindex]") {
		t.Errorf("expected a [reindex] progress line in stderr; got %q", errOut)
	}
	if !strings.Contains(errOut, "[reindex] done:") {
		t.Errorf("expected a [reindex] done: summary line in stderr; got %q", errOut)
	}
}

// TestReindex_QuietSuppressesProgress ensures --quiet keeps the
// stderr stream silent so script-driving callers don't get a wall of
// progress chatter mixed in.
func TestReindex_QuietSuppressesProgress(t *testing.T) {
	tmp := t.TempDir()
	var stdout, stderr bytes.Buffer

	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewIngestor: func(_ config.Config, _ model.Store) (model.Ingestor, error) {
			return reindexNoopIngestor{}, nil
		},
	})

	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if code := app.RunWithContext(ctx, []string{"--quiet", "reindex"}); code != 0 {
			t.Fatalf("reindex exit=%d stderr=%q", code, stderr.String())
		}
	})

	if strings.Contains(stderr.String(), "[reindex]") {
		t.Errorf("--quiet should suppress progress lines; got %q", stderr.String())
	}
}

package tests

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/config"
)

// SPEC §7.7 makes the coverage report a STARTUP diagnostic: "Startup diagnostics
// and `dir2mcp doctor` MUST report...". `dir2mcp up` daemonizes by default on a
// terminal, and before #981 neither process printed it on that path: the child
// skips the banner and never computes the verdicts, so they were not in
// server.log either. The only reachable §7.7 surface was `doctor`, which an
// operator has no reason to run when startup looked clean.

func TestDaemonReady_NamesAPartialTranscriptOnTheDefaultPath(t *testing.T) {
	dir := t.TempDir()
	st := seedTranscripts(t, dir, seedRep{relPath: "archive/interview.mp4",
		metaJSON: coverageMeta(t, "whisper", "large-v3", 8, 1, rfeDecodedMS, rfeDurationMS)})
	_ = st.Close()

	var stdout, stderr strings.Builder
	app := cli.NewAppWithIO(&stdout, &stderr)
	cfg := config.Config{StateDir: filepath.Join(dir, ".dir2mcp")}
	app.RenderDaemonReadyForTest(context.Background(), cfg)

	got := stdout.String()
	if !strings.Contains(got, "Speech coverage") {
		t.Errorf("the daemon banner names no speech coverage:\n%s", got)
	}
	if !strings.Contains(got, "incomplete decode") {
		t.Errorf("the shortfall is not stated:\n%s", got)
	}
	// It belongs ABOVE the ready line, where an operator reads it, not after.
	if i, j := strings.Index(got, "Speech coverage"), strings.Index(got, "Ready for connections"); i < 0 || j < 0 || i > j {
		t.Errorf("the section is not above the ready line (at %d vs %d):\n%s", i, j, got)
	}
}

func TestDaemonReady_ACleanCorpusKeepsTheBannerShort(t *testing.T) {
	// The sections are silent when there is nothing to report, so the default
	// banner does not grow for a healthy corpus.
	dir := t.TempDir()
	st := seedTranscripts(t, dir, seedRep{relPath: "archive/complete.mp4",
		metaJSON: coverageMeta(t, "whisper", "large-v3", 4, 4, 40*minute, 40*minute)})
	_ = st.Close()

	var stdout, stderr strings.Builder
	app := cli.NewAppWithIO(&stdout, &stderr)
	cfg := config.Config{StateDir: filepath.Join(dir, ".dir2mcp")}
	app.RenderDaemonReadyForTest(context.Background(), cfg)

	got := stdout.String()
	if strings.Contains(got, "Speech coverage") {
		t.Errorf("a clean corpus grew a coverage section:\n%s", got)
	}
	if !strings.Contains(got, "Ready for connections") {
		t.Errorf("the banner is missing its ready line:\n%s", got)
	}
}

func TestDaemonReady_TheStillStartingPathReportsNoCoverage(t *testing.T) {
	// A run that returns while the daemon is still starting prints no coverage,
	// and the README says so. Any verdict there would describe a corpus the
	// child has not finished building, and "no uncovered formats" read off a
	// half-built record is the false clean bill §7.7 exists to prevent.
	//
	// Asserted through the banner renderer that path does NOT reach: if the
	// coverage ever moved above the readiness wait, printDaemonReady would no
	// longer be the only place it renders and this pairing would need revisiting.
	dir := t.TempDir()
	st := seedTranscripts(t, dir, seedRep{relPath: "archive/interview.mp4",
		metaJSON: coverageMeta(t, "whisper", "large-v3", 8, 1, rfeDecodedMS, rfeDurationMS)})
	_ = st.Close()

	var stdout, stderr strings.Builder
	app := cli.NewAppWithIO(&stdout, &stderr)
	cfg := config.Config{StateDir: filepath.Join(dir, ".dir2mcp")}

	// The still-starting path prints its own notice and returns; nothing it
	// writes carries a coverage section.
	app.ReportDaemonStillStartingForTest(4242, filepath.Join(cfg.StateDir, "server.log"))
	if got := stdout.String() + stderr.String(); strings.Contains(got, "Speech coverage") {
		t.Errorf("the still-starting notice carried a coverage section:\n%s", got)
	}
}

package test

import (
	"os"
	"path/filepath"
	"testing"

	"dir2mcp/internal/dirstral/app"
	"dir2mcp/internal/dirstral/host"
)

func TestTailLogLinesKeepsLastN(t *testing.T) {
	raw := "line1\nline2\nline3\nline4\n"
	got := app.TailLogLines(raw, 2)
	want := "line3\nline4"
	if got != want {
		t.Fatalf("unexpected tailed logs: got %q want %q", got, want)
	}
}

func TestTailLogLinesHandlesEmptyAndUnlimited(t *testing.T) {
	if got := app.TailLogLines("\n", 100); got != "" {
		t.Fatalf("expected empty output for blank input, got %q", got)
	}

	raw := "a\nb\nc"
	if got := app.TailLogLines(raw, 0); got != raw {
		t.Fatalf("expected maxLines=0 to keep all lines, got %q", got)
	}
}

func TestHostLogPathUsesTempDir(t *testing.T) {
	got := host.LogPath()
	want := filepath.Join(os.TempDir(), "dirstral-mcp-server.log")
	if got != want {
		t.Fatalf("unexpected log path: got %q want %q", got, want)
	}
}

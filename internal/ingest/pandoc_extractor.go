package ingest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// pandocExtractTimeout bounds a single document's pandoc subprocess so one
// pathological file can never wedge the indexer. Born-digital conversion is fast
// (pandoc is a native binary with no model load), so 2 minutes is generous
// headroom while still guaranteeing forward progress. It is a var (not a const)
// only so tests can shorten it; production never reassigns it.
var pandocExtractTimeout = 2 * time.Minute

const (
	// maxPandocOutputBytes caps the Markdown we read back from pandoc. Born-digital
	// office/markup/ebook conversions are far smaller than docling's verbose JSON,
	// so 256 MiB comfortably covers any real document while still bounding a
	// pathological output.
	maxPandocOutputBytes = 256 * 1024 * 1024
	maxPandocStderrBytes = 1 * 1024 * 1024
)

// pandocWaitDelay is a backstop after the context is cancelled (timeout or
// shutdown): os/exec sends the kill signal, and if the child has not exited within
// this grace period the I/O pipes are force-closed so cmd.Run() cannot block
// indefinitely on a wedged subprocess.
const pandocWaitDelay = 10 * time.Second

// pandocExtractor converts born-digital office/markup/ebook formats to Markdown
// via a local pandoc CLI invocation (#393, the §7.4.B.1 T2 tier). It is a flat
// extractor: it implements model.DocumentExtractor (Markdown out) and does NOT
// implement structuredExtractor — pandoc emits no reading-order/section/page
// provenance, so there are no region spans to carry.
type pandocExtractor struct {
	// command is the resolved pandoc command template (the first field is the
	// binary). Empty means the `pandoc` binary is resolved from PATH per call.
	command string
}

// NewPandocExtractor returns a document extractor backed by a local pandoc CLI
// invocation. cmd is the configured ingest.pandoc.command (its first field is the
// binary); a blank cmd resolves `pandoc` from PATH.
func NewPandocExtractor(cmd string) *pandocExtractor {
	return &pandocExtractor{command: strings.TrimSpace(cmd)}
}

// pandocBinary returns the executable to invoke: the first field of the configured
// command, or the bare "pandoc" resolved from PATH.
func (p *pandocExtractor) pandocBinary() string {
	if fields := strings.Fields(p.command); len(fields) > 0 {
		return fields[0]
	}
	return "pandoc"
}

// Extract writes data to a temp file (preserving the extension so pandoc can infer
// the input format), runs `<pandoc> <tmpfile> -t gfm`, and returns the trimmed
// GitHub-Flavored-Markdown pandoc streams to stdout. Output and stderr are capped;
// the subprocess is bounded by pandocExtractTimeout. On failure it returns a
// wrapped error whose message never includes the user-supplied command literally.
func (p *pandocExtractor) Extract(ctx context.Context, relPath string, data []byte) (string, error) {
	ext := filepath.Ext(relPath)
	if ext == "" {
		ext = ".bin"
	}
	tmpFile, err := os.CreateTemp("", "dir2mcp-pandoc-*"+ext)
	if err != nil {
		return "", fmt.Errorf("create temp doc file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return "", fmt.Errorf("write temp doc file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("close temp doc file: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, pandocExtractTimeout)
	defer cancel()
	// pandoc streams Markdown to stdout by default; `-t gfm` selects
	// GitHub-Flavored Markdown as the output format.
	cmd := exec.CommandContext(ctx, p.pandocBinary(), tmpPath, "-t", "gfm")
	cmd.WaitDelay = pandocWaitDelay
	cmd.Env = os.Environ()
	stdout := &limitedBuffer{buf: &bytes.Buffer{}, limit: maxPandocOutputBytes}
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &limitedBuffer{buf: &stderr, limit: maxPandocStderrBytes}
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("pandoc command timed out after %s", pandocExtractTimeout)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("pandoc command failed: %s", msg)
	}
	if stdout.Truncated() {
		return "", fmt.Errorf("pandoc command output exceeded limit")
	}
	out := strings.TrimSpace(stdout.buf.String())
	if out == "" {
		return "", fmt.Errorf("pandoc command produced empty output")
	}
	return out, nil
}

package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dirstral/dir2mcp/internal/ingest/docling"
)

// defaultDoclingCommand requests structured JSON (DoclingDocument) so the
// pipeline can preserve reading order, section hierarchy, and per-element
// page/bbox provenance (spec 0.9.0 §7.4.B). When a custom docling_command
// emits flat Markdown instead, Extract transparently falls back to treating
// the output as Markdown.
const defaultDoclingCommand = "docling --to json --output - {input}"

const (
	maxDoclingStdoutBytes = 100 * 1024 * 1024
	maxDoclingStderrBytes = 1 * 1024 * 1024
)

var errDoclingOutputTooLarge = errors.New("docling output exceeded configured limit")

// SanitizeDoclingEnv returns env (in os.Environ "KEY=VALUE" form) with the
// Python path-injection variables removed and user site-packages disabled, so
// a docling subprocess resolves imports exclusively from its bundled venv.
//
// The docling binary is the venv's own console script, so its interpreter
// already prefers the venv's site-packages — but PYTHONPATH entries are still
// appended to sys.path and PYTHONHOME overrides the prefix entirely, either of
// which can shadow the venv's pinned dependencies with a foreign version (the
// classic "two versions installed" failure). PYTHONNOUSERSITE=1 additionally
// keeps any user-site packages out of the path.
func SanitizeDoclingEnv(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			out = append(out, kv)
			continue
		}
		switch key {
		case "PYTHONPATH", "PYTHONHOME", "PYTHONNOUSERSITE":
			// Drop path injectors; PYTHONNOUSERSITE is re-added canonically below.
			continue
		}
		out = append(out, kv)
	}
	return append(out, "PYTHONNOUSERSITE=1")
}

type doclingExtractor struct {
	commandTemplate string
}

type limitedBuffer struct {
	buf   *bytes.Buffer
	limit int
}

func (w *limitedBuffer) Write(p []byte) (int, error) {
	if w.limit > 0 && w.buf.Len()+len(p) > w.limit {
		return 0, errDoclingOutputTooLarge
	}
	return w.buf.Write(p)
}

// NewDoclingExtractor returns a document extractor backed by a local docling
// CLI invocation. If commandTemplate is blank, a default `docling` command is
// used. The template may include `{input}`; when omitted, input is appended.
func NewDoclingExtractor(commandTemplate string) *doclingExtractor {
	tpl := strings.TrimSpace(commandTemplate)
	if tpl == "" {
		tpl = defaultDoclingCommand
	}
	return &doclingExtractor{commandTemplate: tpl}
}

// StructuredExtraction is the result of structured docling extraction: the
// rendered Markdown persisted as the extracted_markdown representation, the
// ordered blocks (reading order + section breadcrumb + provenance) that drive
// section-aware chunking and region spans, and the document title.
type StructuredExtraction struct {
	Markdown string
	Blocks   []docling.Block
	Title    string
}

// structuredExtractor is implemented by extractors that can emit a structured
// document model. The ingest service type-asserts against it to choose the
// structured path; extractors that only produce flat text (e.g. Mistral OCR)
// do not implement it and use the page-separated path.
type structuredExtractor interface {
	ExtractStructured(ctx context.Context, relPath string, data []byte) (StructuredExtraction, error)
}

// run executes the configured docling command against data and returns the
// trimmed stdout.
func (d *doclingExtractor) run(ctx context.Context, relPath string, data []byte) (string, error) {
	ext := filepath.Ext(relPath)
	if ext == "" {
		ext = ".bin"
	}
	tmpFile, err := os.CreateTemp("", "dir2mcp-docling-*"+ext)
	if err != nil {
		return "", fmt.Errorf("create temp doc file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return "", fmt.Errorf("write temp doc file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("close temp doc file: %w", err)
	}

	args, err := buildDoclingCommandArgs(d.commandTemplate, tmpPath)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	// Run docling with a sanitized environment so it uses only its bundled
	// venv. A stray PYTHONPATH/PYTHONHOME in the caller's shell (e.g. from a
	// conda install) would otherwise be added to the venv interpreter's
	// sys.path and shadow the venv's pinned packages with a different version.
	cmd.Env = SanitizeDoclingEnv(os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedBuffer{buf: &stdout, limit: maxDoclingStdoutBytes}
	cmd.Stderr = &limitedBuffer{buf: &stderr, limit: maxDoclingStderrBytes}
	if err := cmd.Run(); err != nil {
		if errors.Is(err, errDoclingOutputTooLarge) {
			return "", fmt.Errorf("docling command output exceeded limit")
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("docling command failed: %s", msg)
	}

	out := strings.TrimSpace(stdout.String())
	if out == "" {
		return "", fmt.Errorf("docling command produced empty output")
	}
	return out, nil
}

// Extract returns Markdown suitable for chunking/indexing. When the command
// emits a structured DoclingDocument (the default `--to json`), the document is
// linearized to Markdown in reading order; when it emits flat Markdown (a
// custom `--to md` command), the output is returned as-is.
func (d *doclingExtractor) Extract(ctx context.Context, relPath string, data []byte) (string, error) {
	out, err := d.run(ctx, relPath, data)
	if err != nil {
		return "", err
	}
	doc, perr := docling.Parse([]byte(out))
	if perr == nil {
		return docling.RenderMarkdown(doc.Linearize()), nil
	}
	// Output that looks like JSON but failed to parse is a structured-extraction
	// failure (truncated/schema-drifted DoclingDocument); fail fast rather than
	// persisting raw JSON as the representation. Genuinely non-JSON output is a
	// custom flat-Markdown command (--to md), returned as-is.
	if looksLikeJSON(out) {
		return "", fmt.Errorf("parse structured docling output for %s: %w", relPath, perr)
	}
	return out, nil
}

// looksLikeJSON reports whether trimmed output begins with a JSON object or
// array delimiter, used to distinguish a broken DoclingDocument from a
// deliberate flat-Markdown command.
func looksLikeJSON(out string) bool {
	t := strings.TrimSpace(out)
	return strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")
}

// ExtractStructured runs the command and parses the structured DoclingDocument.
// It returns an error when the output is not a parseable DoclingDocument, so
// callers can fall back to the flat path.
func (d *doclingExtractor) ExtractStructured(ctx context.Context, relPath string, data []byte) (StructuredExtraction, error) {
	out, err := d.run(ctx, relPath, data)
	if err != nil {
		return StructuredExtraction{}, err
	}
	doc, err := docling.Parse([]byte(out))
	if err != nil {
		return StructuredExtraction{}, err
	}
	blocks := doc.Linearize()
	return StructuredExtraction{
		Markdown: docling.RenderMarkdown(blocks),
		Blocks:   blocks,
		Title:    doc.Title(),
	}, nil
}

func buildDoclingCommandArgs(template, inputPath string) ([]string, error) {
	parts := strings.Fields(strings.TrimSpace(template))
	if len(parts) == 0 {
		return nil, fmt.Errorf("docling command is empty")
	}
	replaced := false
	for i := range parts {
		if strings.Contains(parts[i], "{input}") {
			parts[i] = strings.ReplaceAll(parts[i], "{input}", inputPath)
			replaced = true
		}
	}
	if !replaced {
		parts = append(parts, inputPath)
	}
	return parts, nil
}

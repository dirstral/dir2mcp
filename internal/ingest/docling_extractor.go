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
//
// `{output}` is substituted with a fresh per-call temp directory and the
// produced file is read back from it. docling does NOT stream to stdout —
// `--output -` writes a file named `-` in the working directory and leaves
// stdout empty (issue #376), which silently failed every extraction. A template
// that omits `{output}` keeps the legacy stdout-reading path (e.g. a custom
// `cat {input}` or a wrapper that genuinely prints to stdout).
const defaultDoclingCommand = "docling --to json --output {output} {input}"

// doclingOutputPlaceholder marks where a per-call temp output directory is
// substituted into the command template.
const doclingOutputPlaceholder = "{output}"

const (
	// maxDoclingOutputBytes caps the docling extraction output we read back (the
	// structured JSON file, the legacy stdout buffer, and the docling-serve
	// response). docling's `--to json` DoclingDocument is verbose — a 920KB,
	// few-hundred-page legal PDF produces ~100MB of JSON — so the old 100MB cap
	// rejected large acts (e.g. the BVI Business Companies Act) with "output
	// exceeded limit" and left them unindexed (issue #381). 1 GiB comfortably
	// covers the largest legal PDFs while still bounding a pathological output.
	// The read is one-time during indexing; query-time paths are unaffected.
	maxDoclingOutputBytes = 1024 * 1024 * 1024
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
// used. The template may include `{input}` and `{output}`; when `{input}` is
// omitted, input is appended.
//
// A bare binary path (a single token, e.g. the dir2mcp-full wrapper's
// DIR2MCP_DOCLING_COMMAND, which is just `…/docling-venv/bin/docling`) is
// underspecified: run as-is it becomes `docling <input>`, which writes Markdown
// into the working directory and nothing to stdout — every extraction then fails
// "empty output" and litters the corpus (issue #381). Expand it to the default
// flags so docling writes structured JSON into the {output} temp dir we read back.
func NewDoclingExtractor(commandTemplate string) *doclingExtractor {
	tpl := strings.TrimSpace(commandTemplate)
	switch {
	case tpl == "":
		tpl = defaultDoclingCommand
	case len(strings.Fields(tpl)) == 1:
		tpl = tpl + " --to json --output " + doclingOutputPlaceholder + " {input}"
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
// trimmed extraction output. When the template contains {output} (the default),
// docling writes into a per-call temp directory and run reads the produced file
// back; otherwise the legacy stdout-reading path is used.
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

	if strings.Contains(d.commandTemplate, doclingOutputPlaceholder) {
		return d.runFileOutput(ctx, tmpPath)
	}
	return d.runStdout(ctx, tmpPath)
}

// runFileOutput runs docling with {output} pointed at a fresh temp directory and
// returns the contents of the single file docling writes there (issue #376).
// The output dir is isolated from the corpus so docling's artifacts can never be
// re-ingested, and is removed when the call returns.
func (d *doclingExtractor) runFileOutput(ctx context.Context, inputPath string) (string, error) {
	outDir, err := os.MkdirTemp("", "dir2mcp-docling-out-*")
	if err != nil {
		return "", fmt.Errorf("create docling output dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(outDir) }()

	args, err := buildDoclingCommandArgs(d.commandTemplate, inputPath, outDir)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = SanitizeDoclingEnv(os.Environ())
	// docling logs progress to stdout/stderr; the real output is the file in
	// outDir, so stdout is discarded and only stderr is captured for diagnostics.
	var stderr bytes.Buffer
	cmd.Stderr = &limitedBuffer{buf: &stderr, limit: maxDoclingStderrBytes}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("docling command failed: %s", msg)
	}

	out, err := readDoclingOutputDir(outDir, inputPath, maxDoclingOutputBytes)
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", fmt.Errorf("docling command produced empty output")
	}
	return out, nil
}

// runStdout is the legacy path for command templates without {output}: it reads
// the extraction from the command's stdout (e.g. a custom `cat {input}` wrapper).
func (d *doclingExtractor) runStdout(ctx context.Context, inputPath string) (string, error) {
	args, err := buildDoclingCommandArgs(d.commandTemplate, inputPath, "")
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
	cmd.Stdout = &limitedBuffer{buf: &stdout, limit: maxDoclingOutputBytes}
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

// readDoclingOutputDir returns the contents of the file docling wrote into
// outDir. docling names the output after the input stem (`<stem>.json`/`.md`),
// so when several files are present the one matching the input stem is preferred;
// a single file is used directly. A file larger than limit is rejected rather
// than partially read.
func readDoclingOutputDir(outDir, inputPath string, limit int) (string, error) {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return "", fmt.Errorf("read docling output dir: %w", err)
	}
	files := make([]os.DirEntry, 0, len(entries))
	for _, e := range entries {
		if e.Type().IsRegular() {
			files = append(files, e)
		}
	}
	if len(files) == 0 {
		return "", nil
	}

	chosen := files[0]
	if len(files) > 1 {
		stem := strings.TrimSuffix(filepath.Base(inputPath), filepath.Ext(inputPath))
		for _, f := range files {
			if strings.TrimSuffix(f.Name(), filepath.Ext(f.Name())) == stem {
				chosen = f
				break
			}
		}
	}

	if info, ierr := chosen.Info(); ierr == nil && limit > 0 && info.Size() > int64(limit) {
		return "", fmt.Errorf("docling command output exceeded limit")
	}
	data, err := os.ReadFile(filepath.Join(outDir, chosen.Name()))
	if err != nil {
		return "", fmt.Errorf("read docling output file: %w", err)
	}
	return string(data), nil
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

func buildDoclingCommandArgs(template, inputPath, outputDir string) ([]string, error) {
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
		if strings.Contains(parts[i], doclingOutputPlaceholder) {
			parts[i] = strings.ReplaceAll(parts[i], doclingOutputPlaceholder, outputDir)
		}
	}
	if !replaced {
		parts = append(parts, inputPath)
	}
	return parts, nil
}

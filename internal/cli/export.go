package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// transcriptStore is the read capability the export command needs from the
// metadata store: list a document's transcript representations (with meta_json
// for language matching) and load a representation's chunk+span rows. The CLI
// type-asserts the configured store against this interface so test fakes can
// supply their own implementation without depending on the sqlite store.
type transcriptStore interface {
	TranscriptRepresentations(ctx context.Context, relPath string) ([]store.TranscriptRepresentation, error)
	TranscriptSpanChunks(ctx context.Context, repID int64) ([]store.TranscriptSpanChunk, error)
}

type exportOptions struct {
	format string
	lang   string
	// secondaryLang selects a second transcript language for bilingual TTML
	// export (SPEC §8.6.10). Empty = monolingual. Only meaningful with
	// --format ttml.
	secondaryLang string
	out           string
	relPath       string
}

// runExport renders a document's stored transcript as a WebVTT or SRT subtitle
// document, writing it to --out (atomically) or to stdout. It resolves the
// transcript representation (matching --lang when given, otherwise the document's
// sole/source transcript) and renders its time-spanned chunks as cues.
func (a *App) runExport(ctx context.Context, global globalOptions, args []string) int {
	opts, err := parseExportOptions(args)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("invalid export flags: %v", err))
		return exitConfigInvalid
	}

	cfg, err := loadConfigWithGlobalOptions(global)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("load config: %v", err))
		return exitConfigInvalid
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		cfg.StateDir = filepath.Join(".", ".dir2mcp")
	}

	st := a.storeForConfig(cfg)
	defer a.closeStoreWithLog(st)
	if initErr := st.Init(ctx); initErr != nil && !errors.Is(initErr, model.ErrNotImplemented) {
		writeCLIError(a.stderr, global.jsonOutput, exitIndexLoadFailure, fmt.Sprintf("initialize metadata store: %v", initErr))
		return exitIndexLoadFailure
	}

	ts, ok := interface{}(st).(transcriptStore)
	if !ok {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, "configured store does not support transcript export")
		return exitGeneric
	}

	// TTML (and its companion SMIL) is the optional broadcast-packaging surface
	// (SPEC §8.6.10), gated by config and OFF by default. It has its own
	// resolve+render+emit path because it MAY select two transcript languages
	// (bilingual) and MAY emit two sidecars (TTML + SMIL).
	if opts.format == "ttml" {
		return a.runTTMLExport(ctx, global, cfg, ts, opts)
	}

	filter := subtitle.NewWordFilter(cfg.MediaFilterWords)
	rendered, code := a.renderTranscriptExport(ctx, global, ts, opts, filter)
	if code != exitSuccess {
		return code
	}

	return a.emitExport(global, opts, rendered)
}

// renderTranscriptExport resolves the transcript representation, builds cues
// from its chunks, and renders them in the requested format. It returns the
// serialized document, or an exit code on error (no transcript, no chunks,
// unknown language, store failure).
func (a *App) renderTranscriptExport(ctx context.Context, global globalOptions, ts transcriptStore, opts exportOptions, filter *subtitle.WordFilter) (string, int) {
	reps, err := ts.TranscriptRepresentations(ctx, opts.relPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("no document found at %q", opts.relPath))
			return "", exitGeneric
		}
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("load transcript representations: %v", err))
		return "", exitGeneric
	}
	if len(reps) == 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("document %q has no transcript representation", opts.relPath))
		return "", exitGeneric
	}

	rep, ok := selectTranscriptRep(reps, opts.lang)
	if !ok {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("document %q has no transcript for language %q", opts.relPath, opts.lang))
		return "", exitGeneric
	}

	rows, err := ts.TranscriptSpanChunks(ctx, rep.RepID)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("load transcript chunks: %v", err))
		return "", exitGeneric
	}

	chunks := make([]subtitle.TranscriptChunk, 0, len(rows))
	for _, r := range rows {
		chunks = append(chunks, subtitle.TranscriptChunk{Text: r.Text, Span: r.Span})
	}
	cues := subtitle.BuildCues(chunks)
	// Apply the configured caption word filter (media.filter_words) on export so
	// exported VTT/SRT never contain the boilerplate/credits/watermark phrases,
	// consistent with how ingest strips them before embedding. Cues empty after
	// filtering are dropped. An empty config leaves cues unchanged.
	cues = subtitle.FilterCues(cues, filter)
	if len(cues) == 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("document %q transcript has no time-coded cues to export", opts.relPath))
		return "", exitGeneric
	}

	switch opts.format {
	case "vtt":
		return subtitle.RenderVTT(cues), exitSuccess
	default: // "srt" (validated in parseExportOptions)
		return subtitle.RenderSRT(cues), exitSuccess
	}
}

// emitExport writes the rendered subtitle document to --out (atomically) or to
// stdout when --out is empty.
func (a *App) emitExport(global globalOptions, opts exportOptions, rendered string) int {
	if strings.TrimSpace(opts.out) == "" {
		writef(a.stdout, "%s", rendered)
		return exitSuccess
	}
	if err := writeFileAtomic(opts.out, []byte(rendered)); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("write %q: %v", opts.out, err))
		return exitGeneric
	}
	if !global.quiet && !global.jsonOutput {
		writef(a.stdout, "wrote %s\n", opts.out)
	}
	return exitSuccess
}

// parseExportOptions parses the export subcommand flags and the trailing
// path argument, validating the required --format and the single positional.
func parseExportOptions(args []string) (exportOptions, error) {
	opts := exportOptions{}
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.format, "format", "", "subtitle format: vtt|srt|ttml (required)")
	fs.StringVar(&opts.lang, "lang", "", "language code selecting which transcript to export (default: the document's transcript)")
	fs.StringVar(&opts.secondaryLang, "secondary-lang", "", "second language code for bilingual TTML export (only with --format ttml)")
	fs.StringVar(&opts.out, "out", "", "output file path (default: stdout). For ttml the companion .smil is written alongside")
	if err := fs.Parse(args); err != nil {
		return exportOptions{}, err
	}

	opts.format = strings.ToLower(strings.TrimSpace(opts.format))
	switch opts.format {
	case "vtt", "srt", "ttml":
	case "":
		return exportOptions{}, errors.New("--format is required (vtt|srt|ttml)")
	default:
		return exportOptions{}, fmt.Errorf("unsupported format %q (want vtt|srt|ttml)", opts.format)
	}
	opts.secondaryLang = strings.TrimSpace(opts.secondaryLang)
	if opts.secondaryLang != "" && opts.format != "ttml" {
		return exportOptions{}, errors.New("--secondary-lang is only valid with --format ttml")
	}

	rest := fs.Args()
	if len(rest) == 0 {
		return exportOptions{}, errors.New("export requires a path-or-relpath argument")
	}
	if len(rest) > 1 {
		return exportOptions{}, fmt.Errorf("export accepts a single path argument: %s", strings.Join(rest, " "))
	}
	opts.relPath = strings.TrimSpace(rest[0])
	if opts.relPath == "" {
		return exportOptions{}, errors.New("export requires a non-empty path argument")
	}
	opts.lang = strings.TrimSpace(opts.lang)
	return opts, nil
}

// selectTranscriptRep chooses the transcript representation to export. With an
// empty lang it returns the first (source/only) transcript — there is no
// hardcoded default language. With a non-empty lang it returns the first
// representation whose meta_json language matches (case-insensitively),
// reporting ok=false when none match.
func selectTranscriptRep(reps []store.TranscriptRepresentation, lang string) (store.TranscriptRepresentation, bool) {
	if len(reps) == 0 {
		return store.TranscriptRepresentation{}, false
	}
	lang = strings.TrimSpace(lang)
	if lang == "" {
		return reps[0], true
	}
	for _, rep := range reps {
		if strings.EqualFold(transcriptRepLanguage(rep.MetaJSON), lang) {
			return rep, true
		}
	}
	return store.TranscriptRepresentation{}, false
}

// transcriptRepLanguage extracts a language code from a transcript
// representation's meta_json, accepting either a "language" or "lang" key.
// Returns "" when the metadata is absent or unparseable, so language-tagged
// sidecar transcripts (#253) match while legacy untagged ones simply do not.
func transcriptRepLanguage(metaJSON string) string {
	trimmed := strings.TrimSpace(metaJSON)
	if trimmed == "" {
		return ""
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(trimmed), &meta); err != nil {
		return ""
	}
	for _, key := range []string{"language", "lang"} {
		if v, ok := meta[key]; ok {
			if s, ok := v.(string); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// writeFileAtomic writes data to path via a sibling temp file + rename so a
// reader never observes a partially written subtitle document. The temp file is
// removed on any failure before the rename completes.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(dir, ".dir2mcp-export-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dirstral/dir2mcp/internal/avutil"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/subexport"
	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// runTTMLExport renders the optional bilingual TTML packaging surface (SPEC
// §8.6.10) and, when enabled, its companion SMIL. Cue resolution, cleaning and
// alignment live in internal/subexport so ingest-time write-back (§8.6.14)
// produces the same bytes; this function owns only the CLI contract: the gate,
// the historical error wording and exit codes, and the file emission.
func (a *App) runTTMLExport(ctx context.Context, global globalOptions, cfg config.Config, ts transcriptStore, opts exportOptions) int {
	if !cfg.MediaSubtitlesTTMLEnabled {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid,
			"TTML export is disabled (set media.subtitles.ttml.enabled to enable, SPEC §8.6.10)")
		return exitConfigInvalid
	}

	renderer, err := subexport.NewRenderer(cfg)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, err.Error())
		return exitConfigInvalid
	}

	ttml, primaryLang, err := renderer.RenderTTML(ctx, ts, opts.relPath, opts.lang, opts.secondaryLang)
	if err != nil {
		// A missing language is INVALID_FIELD on this surface (§8.6.3). The
		// secondary resolution runs after the primary succeeded, so an
		// ErrNoLanguage naming the secondary tag is attributed to it.
		lang := opts.lang
		if opts.secondaryLang != "" && strings.Contains(err.Error(), fmt.Sprintf("lang %q", opts.secondaryLang)) {
			lang = opts.secondaryLang
		}
		code := exitGeneric
		if errors.Is(err, subexport.ErrNoLanguage) {
			code = exitConfigInvalid
		}
		writeCLIError(a.stderr, global.jsonOutput, code, exportErrorMessage(err, opts.relPath, lang, true))
		return code
	}
	return a.emitTTMLExport(ctx, global, cfg, opts, primaryLang, ttml)
}

// emitTTMLExport writes the TTML document and, when SMIL is enabled and an --out
// path is given, the companion SMIL sidecar alongside it. Without --out the TTML
// is written to stdout and SMIL is not produced (a single stream has no sidecar
// path to reference). SMIL fails open: a probe failure omits the SMIL but never
// fails the TTML emission.
//
// lang is the RESOLVED primary language (see resolvedExportLanguage), not the
// raw --lang flag, so the SMIL sidecar tags the same language the TTML does.
func (a *App) emitTTMLExport(ctx context.Context, global globalOptions, cfg config.Config, opts exportOptions, lang, ttml string) int {
	if strings.TrimSpace(opts.out) == "" {
		writef(a.stdout, "%s", ttml)
		return exitSuccess
	}
	if err := writeFileAtomic(opts.out, []byte(ttml)); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("write %q: %v", opts.out, err))
		return exitGeneric
	}
	if !global.quiet && !global.jsonOutput {
		writef(a.stdout, "wrote %s\n", opts.out)
	}

	if cfg.MediaSubtitlesSMILEnabled {
		a.emitSMILSidecar(ctx, global, cfg, opts, lang)
	}
	return exitSuccess
}

// emitSMILSidecar probes the media and writes a SMIL packaging document next to
// the TTML --out path (same base name, .smil extension). The media is resolved
// through the configured CorpusFS (Localize), so a non-local corpus (S3) is
// probed from a temporary local copy rather than from a RootDir path that holds
// no file (issue #736); for a local corpus Localize is the in-root path with a
// no-op cleanup, so local behavior is unchanged. It fails open per SPEC §8.6.10:
// when the media cannot be fetched or probed (ffprobe absent, corrupt input) it
// warns (non-fatal) and produces no SMIL rather than failing the export.
func (a *App) emitSMILSidecar(ctx context.Context, global globalOptions, cfg config.Config, opts exportOptions, lang string) {
	mediaPath, cleanup, ok := a.localizeExportMedia(ctx, cfg, opts.relPath)
	if !ok {
		return
	}
	defer cleanup()

	info, err := avutil.ProbeMediaInfo(ctx, mediaPath)
	if err != nil {
		// Fail open: omit SMIL, keep the TTML already written.
		//
		// The reason is a fixed label and the error is NOT interpolated.
		// ProbeMediaInfo returns `fmt.Errorf("ffprobe %q: %w: %s", path, err,
		// stderr)`, so `%v` would put raw ffprobe stderr and a local filesystem
		// path on the operator's terminal. The previous code did exactly that
		// while carrying a comment saying it must not. `recognize.go` sets the
		// precedent: "Deliberately no body echo: backend errors may include
		// local paths".
		a.warnSMILSkipped("media_metadata_unavailable")
		return
	}

	smilPath := strings.TrimSuffix(opts.out, filepath.Ext(opts.out)) + ".smil"
	// A bilingual TTML carries both languages in one document, so it is referenced
	// once with the RESOLVED primary language tag (issue #730). It used to be the
	// raw --lang flag, so an omitted --lang produced a <textstream> with no
	// systemLanguage even when the transcript recorded one. An empty resolved tag
	// still omits the attribute, which RenderSMIL handles.
	subs := []subtitle.SMILSubtitleRef{{Src: filepath.Base(opts.out), Lang: lang}}
	smil := subtitle.RenderSMIL(subtitle.SMILInput{
		// The media reference is the corpus document's own path, never the
		// localized copy's: a downloaded S3 object lands in a temp file whose
		// name is an implementation detail and does not exist for a player.
		//
		// The full rel_path, not its base name. The base was what the previous
		// local-only code emitted, and it does not identify the document:
		// `videos/game.mp4` and `archive/game.mp4` both became `game.mp4`. It
		// also only resolves for a reader sitting in the media's own directory,
		// whereas rel_path resolves from the corpus root, which is where an
		// export of a corpus is normally unpacked.
		MediaSrc:  filepath.ToSlash(opts.relPath),
		Info:      info,
		Subtitles: subs,
	})
	if err := writeFileAtomic(smilPath, []byte(smil)); err != nil {
		a.warnSMILSkipped("write_failed")
		return
	}
	if !global.quiet && !global.jsonOutput {
		writef(a.stdout, "wrote %s\n", smilPath)
	}
}

// localizeExportMedia resolves the corpus document at relPath to a real local
// filesystem path for probing, returning it with a cleanup that releases any
// temporary copy. It builds the CorpusFS the same way the server does
// (buildCorpusFS), so the configured source kind — including S3, which has no
// local file at RootDir/rel_path — is honored.
//
// It is called only from the SMIL path, which runs only when
// media.subtitles.smil.enabled is set and an --out path was given, so an export
// that needs no probe never fetches media.
//
// Both failure modes fail open (ok=false) but are reported distinctly: an
// operator must be able to tell "the media could not be fetched" from "the media
// was fetched and could not be probed".
func (a *App) localizeExportMedia(ctx context.Context, cfg config.Config, relPath string) (string, func(), bool) {
	fsys, err := buildCorpusFS(ctx, cfg)
	if err != nil {
		a.warnSMILSkipped("corpus_source_unavailable")
		return "", nil, false
	}
	localPath, cleanup, err := fsys.Localize(ctx, relPath)
	if err != nil {
		a.warnSMILSkipped("media_fetch_failed")
		return "", nil, false
	}
	if cleanup == nil {
		cleanup = func() {}
	}
	return localPath, cleanup, true
}

// warnSMILSkipped reports a non-fatal reason the SMIL companion was not written.
// It goes to stderr as a `warning:` line, the CLI's convention for non-fatal
// problems, and is NOT suppressed by --quiet/--json: silently omitting an
// explicitly enabled artifact and saying nothing about it is what made issue
// #736 invisible to S3 operators. stdout stays machine-safe.
func (a *App) warnSMILSkipped(reason string) {
	writef(a.stderr, "warning: skipping SMIL (%s)\n", reason)
}

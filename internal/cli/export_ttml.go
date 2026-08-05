package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/dirstral/dir2mcp/internal/avutil"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// runTTMLExport renders a document's transcript(s) as the optional broadcast
// TTML packaging surface (SPEC §8.6.10) and, when enabled, a companion SMIL
// document. It is reached only for --format ttml. The surface is gated by config
// (media.subtitles.ttml.enabled) and OFF by default, so a disabled config never
// reaches the render path. With one language it emits monolingual TTML; with a
// --secondary-lang whose transcript exists it emits bilingual TTML aligned
// within the configured tolerance. A requested language with no transcript is
// reported as INVALID_FIELD. SMIL fails open: when probe metadata is
// unavailable the SMIL is omitted but the TTML is still emitted.
func (a *App) runTTMLExport(ctx context.Context, global globalOptions, cfg config.Config, ts transcriptStore, opts exportOptions) int {
	if !cfg.MediaSubtitlesTTMLEnabled {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid,
			"TTML export is disabled (set media.subtitles.ttml.enabled to enable, SPEC §8.6.10)")
		return exitConfigInvalid
	}

	filter := subtitle.NewWordFilter(cfg.MediaFilterWords)

	primaryCues, code := a.resolveCues(ctx, global, ts, opts.relPath, opts.lang, filter)
	if code != exitSuccess {
		return code
	}

	var bilingual []subtitle.BilingualCue
	if opts.secondaryLang != "" {
		secondaryCues, scode := a.resolveCues(ctx, global, ts, opts.relPath, opts.secondaryLang, filter)
		if scode != exitSuccess {
			return scode
		}
		bilingual = subtitle.AlignBilingual(primaryCues, secondaryCues,
			opts.lang, opts.secondaryLang, cfg.MediaSubtitlesTTMLAlignToleranceMS)
	} else {
		bilingual = subtitle.MonolingualBilingualCues(primaryCues, opts.lang)
	}
	if len(bilingual) == 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric,
			fmt.Sprintf("document %q transcript has no time-coded cues to export", opts.relPath))
		return exitGeneric
	}

	ttml := subtitle.RenderTTML(bilingual, opts.lang)
	return a.emitTTMLExport(ctx, global, cfg, opts, ttml)
}

// resolveCues loads the transcript representation for lang, builds its cues, and
// applies the configured word filter. A non-empty lang with no matching
// transcript is reported as INVALID_FIELD (SPEC §8.6.10/§8.6.3). It returns the
// cues, or an exit code on error.
func (a *App) resolveCues(ctx context.Context, global globalOptions, ts transcriptStore, relPath, lang string, filter *subtitle.WordFilter) ([]subtitle.Cue, int) {
	reps, err := ts.TranscriptRepresentations(ctx, relPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("no document found at %q", relPath))
			return nil, exitGeneric
		}
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("load transcript representations: %v", err))
		return nil, exitGeneric
	}
	if len(reps) == 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric,
			fmt.Sprintf("document %q has no transcript representation", relPath))
		return nil, exitGeneric
	}

	rep, ok := selectTranscriptRep(reps, lang)
	if !ok {
		// A requested language with no transcript is a client field error, not a
		// server failure (SPEC §8.6.10: "Requesting an export for a language with
		// no transcript is INVALID_FIELD").
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid,
			fmt.Sprintf("INVALID_FIELD: document %q has no transcript for language %q", relPath, lang))
		return nil, exitConfigInvalid
	}

	rows, err := ts.TranscriptSpanChunks(ctx, rep.RepID)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("load transcript chunks: %v", err))
		return nil, exitGeneric
	}
	chunks := make([]subtitle.TranscriptChunk, 0, len(rows))
	for _, r := range rows {
		chunks = append(chunks, subtitle.TranscriptChunk{Text: r.Text, Span: r.Span})
	}
	cues := subtitle.FilterCues(subtitle.BuildCues(chunks), filter)
	return cues, exitSuccess
}

// emitTTMLExport writes the TTML document and, when SMIL is enabled and an --out
// path is given, the companion SMIL sidecar alongside it. Without --out the TTML
// is written to stdout and SMIL is not produced (a single stream has no sidecar
// path to reference). SMIL fails open: a probe failure omits the SMIL but never
// fails the TTML emission.
func (a *App) emitTTMLExport(ctx context.Context, global globalOptions, cfg config.Config, opts exportOptions, ttml string) int {
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
		a.emitSMILSidecar(ctx, global, cfg, opts)
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
func (a *App) emitSMILSidecar(ctx context.Context, global globalOptions, cfg config.Config, opts exportOptions) {
	mediaPath, cleanup, ok := a.localizeExportMedia(ctx, cfg, opts.relPath)
	if !ok {
		return
	}
	defer cleanup()

	info, err := avutil.ProbeMediaInfo(ctx, mediaPath)
	if err != nil {
		// Fail open: omit SMIL, keep the TTML already written. Never echo raw
		// ffprobe stderr at the user beyond a short note.
		a.warnSMILSkipped("media metadata unavailable: %v", err)
		return
	}

	smilPath := strings.TrimSuffix(opts.out, filepath.Ext(opts.out)) + ".smil"
	// A bilingual TTML carries both languages in one document, so it is referenced
	// once with the primary language tag.
	subs := []subtitle.SMILSubtitleRef{{Src: filepath.Base(opts.out), Lang: opts.lang}}
	smil := subtitle.RenderSMIL(subtitle.SMILInput{
		// The media reference is the corpus document's own name, never the
		// localized copy's: a downloaded S3 object lands in a temp file whose
		// name is an implementation detail and does not exist for a player.
		MediaSrc:  path.Base(filepath.ToSlash(opts.relPath)),
		Info:      info,
		Subtitles: subs,
	})
	if err := writeFileAtomic(smilPath, []byte(smil)); err != nil {
		a.warnSMILSkipped("write %q: %v", smilPath, err)
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
		a.warnSMILSkipped("corpus source unavailable: %v", err)
		return "", nil, false
	}
	localPath, cleanup, err := fsys.Localize(ctx, relPath)
	if err != nil {
		a.warnSMILSkipped("cannot read media %q from the corpus source: %v", relPath, err)
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
func (a *App) warnSMILSkipped(format string, args ...interface{}) {
	writef(a.stderr, "warning: skipping SMIL ("+format+")\n", args...)
}

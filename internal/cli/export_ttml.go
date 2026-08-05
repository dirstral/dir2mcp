package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dirstral/dir2mcp/internal/avutil"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/store"
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

	// The same cue-preparation pipeline VTT/SRT render through (issue #729). It is
	// built after the enabled gate so a disabled surface still reports "TTML export
	// is disabled" rather than an unrelated config error.
	pipe, err := newCuePipeline(cfg)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, err.Error())
		return exitConfigInvalid
	}

	primaryCues, primaryLang, code := a.resolveCues(ctx, global, ts, opts.relPath, opts.lang, pipe)
	if code != exitSuccess {
		return code
	}

	var bilingual []subtitle.BilingualCue
	if opts.secondaryLang != "" {
		secondaryCues, secondaryLang, scode := a.resolveCues(ctx, global, ts, opts.relPath, opts.secondaryLang, pipe)
		if scode != exitSuccess {
			return scode
		}
		// Both languages are cleaned by resolveCues BEFORE alignment, so the cue
		// set that is aligned is exactly the cue set that is rendered. Aligning
		// pre-clean cues would pair (and time-region-merge) cues that cleaning then
		// drops, which is both non-deterministic w.r.t. config and breaks the
		// §8.6.10 guarantee that both runs map back to the same segment span.
		bilingual = subtitle.AlignBilingual(primaryCues, secondaryCues,
			primaryLang, secondaryLang, cfg.MediaSubtitlesTTMLAlignToleranceMS)
	} else {
		bilingual = subtitle.MonolingualBilingualCues(primaryCues, primaryLang)
	}
	if len(bilingual) == 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric,
			fmt.Sprintf("document %q transcript has no time-coded cues to export", opts.relPath))
		return exitGeneric
	}

	ttml := subtitle.RenderTTML(bilingual, primaryLang)
	return a.emitTTMLExport(ctx, global, cfg, opts, primaryLang, ttml)
}

// resolveCues loads the transcript representation for lang, builds its cues, and
// runs them through the shared cue pipeline (word filter + editorial cleaning).
// A non-empty lang with no matching transcript is reported as INVALID_FIELD
// (SPEC §8.6.10/§8.6.3).
//
// It returns the cues AND the resolved language tag, or an exit code on error.
// Returning the tag is the fix for issue #730: `--lang` is a SELECTOR, not the
// value to emit. With `--lang` omitted the selector is empty but the selected
// representation still knows its own language, and passing the raw flag through
// produced `xml:lang=""` on a transcript that records `en`.
func (a *App) resolveCues(ctx context.Context, global globalOptions, ts transcriptStore, relPath, lang string, pipe cuePipeline) ([]subtitle.Cue, string, int) {
	reps, err := ts.TranscriptRepresentations(ctx, relPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("no document found at %q", relPath))
			return nil, "", exitGeneric
		}
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("load transcript representations: %v", err))
		return nil, "", exitGeneric
	}
	if len(reps) == 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric,
			fmt.Sprintf("document %q has no transcript representation", relPath))
		return nil, "", exitGeneric
	}

	rep, ok := selectTranscriptRep(reps, lang)
	if !ok {
		// A requested language with no transcript is a client field error, not a
		// server failure (SPEC §8.6.10: "Requesting an export for a language with
		// no transcript is INVALID_FIELD").
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid,
			fmt.Sprintf("INVALID_FIELD: document %q has no transcript for language %q", relPath, lang))
		return nil, "", exitConfigInvalid
	}

	rows, err := ts.TranscriptSpanChunks(ctx, rep.RepID)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("load transcript chunks: %v", err))
		return nil, "", exitGeneric
	}
	chunks := make([]subtitle.TranscriptChunk, 0, len(rows))
	for _, r := range rows {
		chunks = append(chunks, subtitle.TranscriptChunk{Text: r.Text, Span: r.Span})
	}
	return pipe.apply(subtitle.BuildCues(chunks)), resolvedExportLanguage(rep), exitSuccess
}

// resolvedExportLanguage reports the language tag to emit for a selected
// transcript representation (issue #730).
//
// The representation's OWN recorded tag wins whenever it has one. That is the
// authoritative record of what language the cues are actually in, and it is what
// makes an omitted --lang work: selectTranscriptRep returns the first/source
// transcript, and its meta_json language is the tag that describes it.
// Preferring the record also canonicalizes the spelling, since --lang matching
// is case-insensitive: `--lang EN` against a transcript recording `en` emits
// `en`, not the caller's `EN`.
//
// When the representation records no language at all, the tag stays EMPTY, and
// the caller's --lang is deliberately NOT echoed in its place. That case is
// reachable only with an omitted --lang anyway (a non-empty --lang can only
// match a representation that recorded a language, so there is nothing to echo),
// i.e. a legacy transcript indexed before language tagging. There is no
// fallback by design: an empty xml:lang means "no language information is
// available" per XML 1.0 §2.12 and is what TTML1's own examples use, whereas
// substituting a guessed or configured default would put a plausible-but-wrong
// BCP-47 tag into a broadcast subtitle file. Players act on that tag (track
// selection, font/shaping), so a wrong tag misroutes where an absent one merely
// degrades. Downstream, RenderTTML omits per-run xml:lang and RenderSMIL omits
// systemLanguage for an empty tag.
func resolvedExportLanguage(rep store.TranscriptRepresentation) string {
	return transcriptRepLanguage(rep.MetaJSON)
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

package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dirstral/dir2mcp/internal/model"
)

// readOrComputeTranslation returns the source transcript translated into
// targetLang, caching the result under <state>/cache/translate keyed by the
// SOURCE content hash plus the target-language suffix (TranscriptLangSuffix), so
// re-ingesting an unchanged document reuses the cached translation instead of
// re-calling the chat provider (SPEC §8.6.2 caching parity with the transcript
// cache). The cache is keyed by source content, not target text, because the
// same source media always yields the same translation for a given target.
func (s *Service) readOrComputeTranslation(ctx context.Context, content []byte, sourceText, targetLang string) (string, error) {
	cacheDir := filepath.Join(s.cfg.StateDir, "cache", "translate")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("create translate cache dir: %w", err)
	}

	// Key the cache on the canonical translate derivation identity, not just the
	// source media bytes (SPEC §8.6.7): the same media re-translated after the
	// source transcript text, the translate provider, the model, or the TARGET
	// LANGUAGE changes must NOT return stale text (the rep's meta_json records the
	// new provider/model, so a stale body would be mislabeled). translateCacheKey
	// folds {content, source-text, capability=translate|provider|model|target-lang}
	// via the same derivationIdentity scheme as transcriptCacheKey/ocrCacheKey, so
	// the same source in two corpora derives once (cross-corpus reuse) while an
	// identity change misses without reading another derivation's bytes. The
	// TranscriptLangSuffix is retained on the filename so cache files stay
	// human-identifiable by language.
	base := s.translateCacheKey(content, sourceText, targetLang) + TranscriptLangSuffix(targetLang)
	cachePath := filepath.Join(cacheDir, base+".txt")
	if cached, err := os.ReadFile(cachePath); err == nil {
		return string(cached), nil
	}

	translated, err := s.translateTranscriptText(ctx, sourceText, targetLang)
	if err != nil {
		return "", err
	}

	out := strings.ReplaceAll(strings.ReplaceAll(translated, "\r\n", "\n"), "\r", "\n")
	if err := os.WriteFile(cachePath, []byte(out), 0o644); err != nil {
		return "", fmt.Errorf("write translate cache: %w", err)
	}
	return out, nil
}

// translateTranscriptText translates a segment-formatted transcript into
// targetLang while keeping it time-aligned to the source (SPEC §8.6.2): each
// source line's leading [mm:ss] / mm:ss timestamp marker is preserved VERBATIM
// and only the spoken text is translated, so chunkTranscriptByTime produces the
// same time spans for the translated transcript as for the source. Lines are
// translated segment-by-segment via the chat Generator; a line with no timestamp
// marker is translated as-is (its leading-marker, if any, is empty).
func (s *Service) translateTranscriptText(ctx context.Context, sourceText, targetLang string) (string, error) {
	lines := strings.Split(sourceText, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			out = append(out, "")
			continue
		}
		marker, body := splitTimestampMarker(line)
		translatedBody, err := s.translateLine(ctx, body, targetLang)
		if err != nil {
			return "", err
		}
		// Collapse any internal newlines/whitespace runs the chat provider may
		// have returned into single spaces, so one source segment stays exactly
		// one output line — otherwise an extra line would desync the translated
		// transcript's time spans from the source (chunkTranscriptByTime keys on
		// per-line [mm:ss] markers).
		translatedBody = strings.Join(strings.Fields(translatedBody), " ")
		switch {
		case marker == "":
			out = append(out, translatedBody)
		case translatedBody == "":
			out = append(out, marker)
		default:
			out = append(out, marker+" "+translatedBody)
		}
	}
	return strings.Join(out, "\n"), nil
}

// translateLineMaxTokens caps a single translated transcript line. One source
// segment maps to one short output line, so a tight bound is plenty here; it
// keeps the #500 runaway path tight on chat backends that respect max_tokens
// WITHOUT lowering the generous default that ask/annotate rely on. It is applied
// only when the translator implements model.BoundedGenerator; otherwise the call
// falls back to Generate (the provider's own default cap still bounds it).
const translateLineMaxTokens = 512

// translateLine translates a single line of transcript text into targetLang via
// the chat Generator. Empty/whitespace input short-circuits to empty so the chat
// provider is never called for a marker-only line. The prompt asks for the
// translation only (no preamble), which the trim downstream normalizes.
func (s *Service) translateLine(ctx context.Context, text, targetLang string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", nil
	}
	prompt := buildTranslatePrompt(text, targetLang)
	if bg, ok := s.translator.(model.BoundedGenerator); ok {
		return bg.GenerateWithMaxTokens(ctx, prompt, translateLineMaxTokens)
	}
	translated, err := s.translator.Generate(ctx, prompt)
	if err != nil {
		return "", err
	}
	return translated, nil
}

// buildTranslatePrompt builds a deterministic, general-purpose translation
// prompt. It pins the TARGET language only (the source language is auto-detected
// by the model, matching SPEC §8.6.2's auto-detection default) and instructs the
// model to return the translation alone so the output needs no post-parsing.
func buildTranslatePrompt(text, targetLang string) string {
	var b strings.Builder
	b.WriteString("Translate the following text into ")
	b.WriteString(targetLang)
	b.WriteString(". Preserve meaning faithfully. Return only the translated text, ")
	b.WriteString("with no preamble, quotes, or explanation.\n\n")
	b.WriteString(text)
	return b.String()
}

// splitTimestampMarker splits a transcript line into its leading timestamp
// marker (verbatim, e.g. "[00:12]" or "00:12") and the remaining text. When the
// line has no recognized leading timestamp the marker is empty and the whole
// trimmed line is returned as the text. Keeping the marker byte-for-byte ensures
// the translated transcript's time spans line up with the source.
func splitTimestampMarker(line string) (marker, text string) {
	trimmed := strings.TrimRight(strings.TrimLeft(line, " \t"), " \t")
	if trimmed == "" {
		return "", ""
	}
	startMS, body, ok := parseTranscriptTimestamp(trimmed)
	if !ok {
		return "", trimmed
	}
	// Recover the verbatim marker as the prefix of the trimmed line preceding the
	// parsed body, so the original bracket/format is preserved exactly.
	if body == "" {
		return trimmed, ""
	}
	idx := strings.LastIndex(trimmed, body)
	if idx <= 0 {
		// Fall back to a normalized [mm:ss] marker when the body could not be
		// located as a suffix (should not happen for parsed lines).
		return formatTimestampMarker(startMS), body
	}
	return strings.TrimRight(trimmed[:idx], " \t"), body
}

// formatTimestampMarker renders a millisecond offset as a bracketed [mm:ss] /
// [hh:mm:ss] marker, or the [.mmm] sub-second form when the offset is not on a
// whole second, matching the transcript timestamp format (issue #431). It is
// only a fallback for the "body not locatable" branch of splitTimestampMarker;
// the common path preserves the source marker verbatim.
func formatTimestampMarker(ms int) string {
	if ms < 0 {
		ms = 0
	}
	totalSec := ms / 1000
	frac := ms % 1000
	h := totalSec / 3600
	m := (totalSec % 3600) / 60
	sec := totalSec % 60
	var base string
	if h > 0 {
		base = fmt.Sprintf("%02d:%02d:%02d", h, m, sec)
	} else {
		base = fmt.Sprintf("%02d:%02d", m, sec)
	}
	if frac != 0 {
		return fmt.Sprintf("[%s.%03d]", base, frac)
	}
	return "[" + base + "]"
}

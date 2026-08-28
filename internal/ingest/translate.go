package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/promptfence"
	"github.com/dirstral/dir2mcp/internal/statefs"
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
	if err := statefs.MkdirAll(cacheDir); err != nil {
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
	if err := statefs.WriteFile(cachePath, []byte(out)); err != nil {
		return "", fmt.Errorf("write translate cache: %w", err)
	}
	return out, nil
}

// translateCell is one source transcript line decomposed into its verbatim
// leading timestamp marker and spoken body, plus the translated body once
// resolved. A cell is translatable when its body has non-whitespace text; empty
// lines and marker-only lines pass through unchanged so the output keeps exactly
// one line per source line (the anti-desync invariant chunkTranscriptByTime
// relies on).
type translateCell struct {
	marker       string
	body         string
	translated   string
	translatable bool
}

// translateTranscriptText translates a segment-formatted transcript into
// targetLang while keeping it time-aligned to the source (SPEC §8.6.2): each
// source line's leading [mm:ss] / mm:ss timestamp marker is preserved VERBATIM
// and only the spoken text is translated, so chunkTranscriptByTime produces the
// same time spans for the translated transcript as for the source.
//
// Cues are translated in WINDOWS of translateWindowLines() consecutive cues per
// chat call, each window carrying a read-only margin of translateContextLines()
// surrounding cues, so the model sees neighbouring cues and can resolve
// referents/agreement, keep split sentences coherent, and hold terminology
// consistent (issue #573). A strict numbered 1:1 request/response contract lets
// the batch be verified — exactly one output line per input cue, in order. On any
// count/format mismatch the window safe-degrades to per-line translation, so a
// malformed model response can never drop/merge/reorder cues and desync subtitle
// timing. The historical per-line path is used directly when the effective window
// is <=1 with no margin (an explicit opt-out that also keeps pre-windowing
// translate caches valid — see translateWindowShape).
func (s *Service) translateTranscriptText(ctx context.Context, sourceText, targetLang string) (string, error) {
	lines := strings.Split(sourceText, "\n")
	cells := make([]translateCell, len(lines))
	var order []int // indices into cells that carry translatable text, in order
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue // zero-value cell: empty line passes through as ""
		}
		marker, body := splitTimestampMarker(line)
		cells[i] = translateCell{marker: marker, body: body}
		if strings.TrimSpace(body) != "" {
			cells[i].translatable = true
			order = append(order, i)
		}
	}

	windowN := s.translateWindowLines()
	marginM := s.translateContextLines()
	if windowN <= 1 && marginM == 0 {
		// Explicit opt-out: translate each cue in isolation (historical behaviour).
		if err := s.translateCellsPerLine(ctx, cells, order, targetLang); err != nil {
			return "", err
		}
	} else {
		for start := 0; start < len(order); start += windowN {
			end := min(start+windowN, len(order))
			if err := s.translateWindow(ctx, cells, order, start, end, marginM, targetLang); err != nil {
				return "", err
			}
		}
	}

	out := make([]string, len(cells))
	for i := range cells {
		if !cells[i].translatable {
			// Empty line -> "" (zero-value marker); marker-only line -> marker.
			out[i] = cells[i].marker
			continue
		}
		out[i] = assembleTranslatedLine(cells[i].marker, cells[i].translated)
	}
	return strings.Join(out, "\n"), nil
}

// translateWindowLines resolves the effective chat-translate window size from
// config: 0 (unset) means the built-in default; <1 is clamped to 1 (per-line).
func (s *Service) translateWindowLines() int {
	n := s.cfg.MediaTranslateWindowLines
	switch {
	case n == 0:
		return config.DefaultMediaTranslateWindowLines
	case n < 1:
		return 1
	default:
		return n
	}
}

// translateContextLines resolves the effective read-only context margin (cues on
// each side of a window) from config; negative is clamped to 0.
func (s *Service) translateContextLines() int {
	if m := s.cfg.MediaTranslateContextLines; m > 0 {
		return m
	}
	return 0
}

// translateWindowShape is the identity token folded into the translate derivation
// identity (activeTranslateIdentity) so a change to the window/margin — which
// materially changes the cross-line context the model sees and therefore its
// output — invalidates cached translations (§8.6.7). The historical per-line path
// (window<=1, margin==0) folds NOTHING, so pre-windowing caches stay valid for
// that explicit opt-out. It is chat-engine only: the whisper engine keys its own
// cache on the STT identity (readOrComputeWhisperTranslation) and never consults
// this identity, so its cache is untouched.
// translatePromptFenceTag joins the translate derivation identity so the #888
// fence change misses stale caches. It is a version marker, not a knob: unlike
// the contextual and summary prompts, translate has no prompt_version config
// surface, and inventing one here would be adding an opt-out for a security
// fix rather than preserving an existing choice.
const translatePromptFenceTag = "f2"

func (s *Service) translateWindowShape() string {
	if s.translateEngine == "whisper" {
		// Whisper translates audio directly and never sees these prompts, so
		// the fence tag below must not reach it: folding it in would re-derive
		// every whisper translation for a prompt change that cannot affect it.
		return ""
	}
	shape := ""
	if w, m := s.translateWindowLines(), s.translateContextLines(); w > 1 || m != 0 {
		shape = fmt.Sprintf("cw%dc%d", w, m)
	}
	// The chat prompts are fenced since #888, which changes what the model is
	// asked and therefore what it returns. A stale cached translation would be
	// mislabeled by the rep's recorded provider/model, exactly as the window
	// shape comment above reasons, so the fence joins the identity and the
	// change misses the cache.
	return shape + translatePromptFenceTag
}

// translateWindow translates order[start:end] in a single numbered batch, giving
// the model up to marginM read-only context cues on each side. It verifies the
// model returned exactly one line per target in order; on any mismatch it falls
// back to per-line translation for this window so timing can never desync.
func (s *Service) translateWindow(ctx context.Context, cells []translateCell, order []int, start, end, marginM int, targetLang string) error {
	targets := order[start:end]
	before := order[max(0, start-marginM):start]
	after := order[end:min(len(order), end+marginM)]

	prompt := buildWindowTranslatePrompt(cells, before, targets, after, targetLang, s.translateGlossaryFor(targetLang))
	raw, err := s.generateBounded(ctx, prompt, translateLineMaxTokens*len(targets))
	if err != nil {
		// A provider/transport failure is NOT a format mismatch: the provider is
		// down, so fanning out into per-line retries would just multiply the same
		// error. Surface it and let translation abort as the per-line path did.
		return err
	}
	parsed, ok := parseNumberedTranslations(raw, len(targets))
	if !ok {
		s.getLogger().Printf("transcript translation: windowed batch response did not return %d numbered lines 1:1; "+
			"falling back to per-line translation for this window (subtitle timing preserved)", len(targets))
		return s.translateCellsPerLine(ctx, cells, targets, targetLang)
	}
	for i, idx := range targets {
		cells[idx].translated = collapseTranslatedLine(parsed[i])
	}
	return nil
}

// translateCellsPerLine translates each cell named by idxs one at a time (the
// historical isolated path). It is also the safe-degrade fallback for a window
// whose batch response was malformed. collapseTranslatedLine keeps one source cue
// exactly one output line.
func (s *Service) translateCellsPerLine(ctx context.Context, cells []translateCell, idxs []int, targetLang string) error {
	for _, idx := range idxs {
		translatedBody, err := s.translateLine(ctx, cells[idx].body, targetLang)
		if err != nil {
			return err
		}
		cells[idx].translated = collapseTranslatedLine(translatedBody)
	}
	return nil
}

// assembleTranslatedLine reattaches the verbatim timestamp marker to a translated
// body, preserving the exact source formatting rules.
func assembleTranslatedLine(marker, translatedBody string) string {
	switch {
	case marker == "":
		return translatedBody
	case translatedBody == "":
		return marker
	default:
		return marker + " " + translatedBody
	}
}

// collapseTranslatedLine collapses any internal newlines/whitespace runs a chat
// provider may have returned into single spaces, so one source segment stays
// exactly one output line — otherwise an extra line would desync the translated
// transcript's time spans from the source (chunkTranscriptByTime keys on per-line
// [mm:ss] markers).
func collapseTranslatedLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// translateLineMaxTokens caps a single translated transcript line. One source
// segment maps to one short output line, so a tight bound is plenty here; it
// keeps the #500 runaway path tight on chat backends that respect max_tokens
// WITHOUT lowering the generous default that ask/annotate rely on. For a windowed
// batch the cap is scaled by the number of target cues. It is applied only when
// the translator implements model.BoundedGenerator; otherwise the call falls back
// to Generate (the provider's own default cap still bounds it).
const translateLineMaxTokens = 512

// generateBounded runs the translator with a per-call max-tokens cap when it
// implements model.BoundedGenerator, else falls back to the plain Generator. A
// non-positive maxTokens also falls back to Generate — mirroring the
// model.BoundedGenerator contract ("<= 0 means use default") and keeping this
// aligned with retrieval.boundedGenerate, so a future zero-cap opt-out behaves
// identically on both paths rather than silently passing 0 here.
func (s *Service) generateBounded(ctx context.Context, prompt string, maxTokens int) (string, error) {
	if maxTokens > 0 {
		if bg, ok := s.translator.(model.BoundedGenerator); ok {
			return bg.GenerateWithMaxTokens(ctx, prompt, maxTokens)
		}
	}
	return s.translator.Generate(ctx, prompt)
}

// translateLine translates a single line of transcript text into targetLang via
// the chat Generator. Empty/whitespace input short-circuits to empty so the chat
// provider is never called for a marker-only line. The prompt asks for the
// translation only (no preamble), which the trim downstream normalizes.
func (s *Service) translateLine(ctx context.Context, text, targetLang string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", nil
	}
	return s.generateBounded(ctx, buildTranslatePrompt(text, targetLang, s.translateGlossaryFor(targetLang)), translateLineMaxTokens)
}

// translateGlossaryFor returns the terminology-guidance entries (source term →
// preferred rendering) configured for targetLang, or nil when no glossary applies
// (SPEC §8.6.2, issue #574). Language tags are matched case-insensitively against
// the normalized (lower-cased) glossary keys.
func (s *Service) translateGlossaryFor(targetLang string) map[string]string {
	if len(s.cfg.MediaTranslateGlossary) == 0 {
		return nil
	}
	return s.cfg.MediaTranslateGlossary[strings.ToLower(strings.TrimSpace(targetLang))]
}

// writeGlossaryGuidance appends a deterministic terminology-guidance line for the
// current target language's glossary entries (SPEC §8.6.2, issue #574). It is
// GUIDANCE, not a hard constraint — the model is asked to PREFER these renderings
// but adapt them for grammar. Entries are emitted in sorted source-term order so
// the prompt is byte-for-byte deterministic (Go map iteration order is
// randomized). No-op when glossary is empty, preserving today's prompt exactly.
func writeGlossaryGuidance(b *strings.Builder, glossary map[string]string) {
	if len(glossary) == 0 {
		return
	}
	terms := make([]string, 0, len(glossary))
	for src := range glossary {
		terms = append(terms, src)
	}
	sort.Strings(terms)
	b.WriteString("Prefer these renderings for the terms below, adapting only as grammar requires ")
	b.WriteString("(case, inflection, agreement): ")
	for i, src := range terms {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(src)
		b.WriteString(" => ")
		b.WriteString(glossary[src])
	}
	b.WriteString(".\n")
}

// buildTranslatePrompt builds a deterministic, general-purpose translation
// prompt. It pins the TARGET language only (the source language is auto-detected
// by the model, matching SPEC §8.6.2's auto-detection default) and instructs the
// model to return the translation alone so the output needs no post-parsing. When
// a non-empty glossary is supplied it injects per-target terminology guidance
// (§8.6.2); an empty glossary reproduces the historical prompt verbatim.
func buildTranslatePrompt(text, targetLang string, glossary map[string]string) string {
	var b strings.Builder
	b.WriteString("Translate the following text into ")
	b.WriteString(targetLang)
	b.WriteString(". Preserve meaning faithfully. Return only the translated text, ")
	b.WriteString("with no preamble, quotes, or explanation.\n")
	writeGlossaryGuidance(&b, glossary)
	// The cue is untrusted DATA (#888): a subtitle line saying "ignore the
	// above" is text an attacker may have written, and the translation is
	// stored and indexed.
	b.WriteString("\n")
	b.WriteString(promptfence.Guard("translate"))
	b.WriteString("\n")
	b.WriteString(promptfence.Wrap("", text))
	// Restated after the data, per #892: the nearest instruction wins.
	b.WriteString("\nReturn only the translated text.")
	return b.String()
}

// buildWindowTranslatePrompt builds the numbered, general-purpose batch prompt
// for a window of consecutive cues (issue #573). The `targets` cues are numbered
// 1..N and are the ONLY lines the model translates and returns; the `before` /
// `after` cues are supplied as read-only context so the model can resolve
// referents, agreement, split sentences, and terminology WITHOUT re-translating
// them. The strict "N: <translation>" response contract lets the caller verify a
// 1:1 mapping and safe-degrade on any mismatch. It is language-agnostic (the
// target language is pinned; the source is auto-detected). A non-empty glossary
// injects the same per-target terminology guidance as the per-line prompt
// (§8.6.2, issue #574); an empty glossary reproduces the historical prompt.
func buildWindowTranslatePrompt(cells []translateCell, before, targets, after []int, targetLang string, glossary map[string]string) string {
	var b strings.Builder
	b.WriteString("Translate each NUMBERED line below into ")
	b.WriteString(targetLang)
	b.WriteString(". These are consecutive subtitle cues from one recording, so use the ")
	b.WriteString("surrounding context lines only to resolve pronouns, gender/number agreement, ")
	b.WriteString("sentences split across cues, and to keep terminology and named entities consistent. ")
	b.WriteString("Preserve meaning faithfully.\n")
	b.WriteString("Return EXACTLY one translated line per numbered input, in the form \"N: <translation>\", ")
	b.WriteString("with the SAME count and the SAME numbers in the SAME order. Do NOT add, drop, split, ")
	b.WriteString("merge, renumber, or reorder lines, and never output the context lines. ")
	b.WriteString("No preamble, quotes, or explanation.\n")
	writeGlossaryGuidance(&b, glossary)
	b.WriteString("\n")
	b.WriteString(promptfence.Guard("translate"))
	b.WriteString("\n")

	// ONE fence around the whole payload region, not one per line (#888). The
	// numbered structure has to survive intact inside it: the response contract
	// is positional ("N: <translation>", verified 1:1 by
	// parseNumberedTranslations, which safe-degrades on any mismatch), so
	// per-line markers would put marker text between the number and the cue and
	// risk silently dropping a whole window's translations.
	var payload strings.Builder
	writeContext := func(heading string, idxs []int) {
		if len(idxs) == 0 {
			return
		}
		payload.WriteString("\n")
		payload.WriteString(heading)
		payload.WriteString("\n")
		for _, idx := range idxs {
			payload.WriteString("- ")
			payload.WriteString(cells[idx].body)
			payload.WriteString("\n")
		}
	}

	writeContext("Context before (do NOT translate or return):", before)
	payload.WriteString("\nLines to translate:\n")
	for n, idx := range targets {
		payload.WriteString(strconv.Itoa(n + 1))
		payload.WriteString(": ")
		payload.WriteString(cells[idx].body)
		payload.WriteString("\n")
	}
	writeContext("Context after (do NOT translate or return):", after)

	b.WriteString(promptfence.Wrap("", payload.String()))
	// Restated after the data, per #892: the nearest instruction wins, and the
	// numbering contract is the one that must survive.
	b.WriteString("\nReturn EXACTLY one translated line per numbered input, in the form ")
	b.WriteString("\"N: <translation>\", and never output the context lines.")
	return b.String()
}

// numberedTranslationRE matches a "N: <text>" / "N. <text>" / "N) <text>"
// response line, capturing the 1-based index and the translated text.
var numberedTranslationRE = regexp.MustCompile(`^\s*(\d+)\s*[:.)]\s?(.*)$`)

// stripFenceEcho removes fence markers a model echoed back into a translation.
// The prompt now carries them (#888), and a model that repeats one would
// otherwise write it into a stored, indexed subtitle: the parser treats an
// unnumbered line as a continuation and would append it to the translation
// above. Defensive, cheap, and it cannot damage real subtitle text, which does
// not contain these literals.
func stripFenceEcho(s string) string {
	for _, marker := range []string{
		promptfence.OpenMarker + promptfence.OpenMarkerEnd,
		promptfence.CloseMarker,
		promptfence.OpenMarker,
	} {
		s = strings.ReplaceAll(s, marker, "")
	}
	return strings.TrimSpace(s)
}

// parseNumberedTranslations parses a windowed batch response into exactly n
// translations, indexed 0..n-1 by the model's 1-based numbering. It enforces the
// strict 1:1 contract: every number 1..n must appear exactly once (no gaps, no
// duplicates, no out-of-range numbers), or it returns ok=false so the caller
// safe-degrades to per-line translation. Continuation lines (a wrapped
// translation with no leading number) are appended to the current entry so a
// model that hard-wraps a long line is tolerated without breaking the mapping.
func parseNumberedTranslations(raw string, n int) ([]string, bool) {
	if n <= 0 {
		return nil, false
	}
	out := make([]string, n)
	seen := make([]bool, n)
	filled := 0
	cur := -1
	for _, line := range strings.Split(raw, "\n") {
		if m := numberedTranslationRE.FindStringSubmatch(line); m != nil {
			num, err := strconv.Atoi(m[1])
			if err != nil || num < 1 || num > n {
				return nil, false
			}
			idx := num - 1
			if seen[idx] {
				return nil, false // duplicate number: cannot trust the 1:1 mapping
			}
			seen[idx] = true
			out[idx] = m[2]
			filled++
			cur = idx
			continue
		}
		if cur >= 0 && strings.TrimSpace(line) != "" {
			out[cur] += " " + line // continuation of a wrapped translation
		}
	}
	if filled != n {
		return nil, false
	}
	// Drop any fence marker the model echoed back (#888) before the text is
	// stored and indexed.
	for i := range out {
		out[i] = stripFenceEcho(out[i])
	}
	return out, true
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

// Test seams for the tests/ tree (issue #888): the fence must be asserted on
// the exact strings the model receives and on the identity that guards the
// cache, not on reconstructions of them.

// BuildTranslatePromptForTest exposes the per-line prompt.
func BuildTranslatePromptForTest(text, targetLang string, glossary map[string]string) string {
	return buildTranslatePrompt(text, targetLang, glossary)
}

// BuildWindowTranslatePromptForTest exposes the windowed prompt, taking cue
// bodies directly so a test does not have to build translateCell values.
func BuildWindowTranslatePromptForTest(before, targets, after []string, targetLang string) string {
	cells := make([]translateCell, 0, len(before)+len(targets)+len(after))
	idx := func(bodies []string) []int {
		out := make([]int, 0, len(bodies))
		for _, body := range bodies {
			cells = append(cells, translateCell{body: body})
			out = append(out, len(cells)-1)
		}
		return out
	}
	beforeIdx, targetIdx, afterIdx := idx(before), idx(targets), idx(after)
	return buildWindowTranslatePrompt(cells, beforeIdx, targetIdx, afterIdx, targetLang, nil)
}

// ParseNumberedTranslationsForTest exposes the 1:1 batch parser, including its
// defensive stripping of echoed fence markers.
func ParseNumberedTranslationsForTest(raw string, n int) ([]string, bool) {
	return parseNumberedTranslations(raw, n)
}

// TranslateWindowShapeForTest exposes the shape folded into the translate
// derivation identity.
func (s *Service) TranslateWindowShapeForTest() string { return s.translateWindowShape() }

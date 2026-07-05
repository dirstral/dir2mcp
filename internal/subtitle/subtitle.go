// Package subtitle renders WebVTT and SubRip (SRT) subtitle documents from a
// document's stored transcript chunks. dir2mcp already persists transcript
// representations as chunk text plus a time span (model.Span{Kind:"time",
// StartMS, EndMS}); this package is a pure read+render layer over that data,
// turning ordered chunks into cues and serializing them in either format.
package subtitle

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dirstral/dir2mcp/internal/model"
)

// blankLineRE matches an interior run of blank lines (a newline, optional
// whitespace, and one or more further newlines). Such a run terminates a cue
// block early in both WebVTT and SRT, so renderers collapse it to a single
// newline to keep cue boundaries intact.
var blankLineRE = regexp.MustCompile(`\n[ \t]*\n+`)

// collapseBlankLines replaces any interior run of blank lines with a single
// newline so ordinary transcript text (which may contain paragraph breaks)
// cannot prematurely terminate a cue block.
func collapseBlankLines(text string) string {
	return blankLineRE.ReplaceAllString(text, "\n")
}

// vttCueTextReplacer escapes the characters that are special in WebVTT cue
// payloads. Escaping '>' also neutralises the "-->" timing arrow (it becomes
// "--&gt;"), so a cue line can never be misread as a timing line.
var vttCueTextReplacer = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
)

// escapeVTTText makes arbitrary transcript text safe as a WebVTT cue payload:
// it collapses interior blank lines, escapes '&'/'<'/'>', and (via the '>'
// escape) neutralises any "-->" so the text cannot break the cue structure.
func escapeVTTText(text string) string {
	return vttCueTextReplacer.Replace(collapseBlankLines(text))
}

// srtCueTextReplacer neutralises the "-->" timing arrow in SRT cue text. SRT
// has no markup escaping, so a literal "-->" on its own line could be parsed as
// a timing line; replacing it with "-→" keeps the text readable and unambiguous.
var srtCueTextReplacer = strings.NewReplacer("-->", "-→")

// neutralizeSRTText makes arbitrary transcript text safe as an SRT cue payload:
// it collapses interior blank lines (which would end the cue block early) and
// neutralises any "-->" timing arrow.
func neutralizeSRTText(text string) string {
	return srtCueTextReplacer.Replace(collapseBlankLines(text))
}

// Cue is a single subtitle entry: a [StartMS, EndMS] time window and the text
// shown during it. Index is the 1-based sequence number used by SRT (WebVTT
// does not require numeric identifiers and omits them).
type Cue struct {
	Index   int
	StartMS int
	EndMS   int
	Text    string
	// Speaker is the human-readable voice/speaker name parsed from a WebVTT
	// <v Name> voice tag on the cue (SPEC §8.6.8). It is metadata only: it never
	// appears in Text (voice tags are stripped) and is empty for cues with no
	// voice markup, so a transcript without <v> tags is unchanged. A sidecar
	// that carries voice tags can populate speaker attribution WITHOUT a
	// diarization model.
	Speaker string
}

// TranscriptChunk pairs a transcript chunk's text with its time span. It is the
// builder's input shape so callers (CLI/store) need not depend on the full
// model.Chunk row layout, only the two fields that matter for subtitles.
type TranscriptChunk struct {
	Text string
	Span model.Span
}

// BuildCues turns transcript chunks into ordered, 1-indexed cues. Chunks are
// sorted by span start (then end) so out-of-order storage still renders in
// playback order. Only spans of kind "time" contribute timing; chunks whose
// text is empty after trimming are skipped (an empty cue has nothing to show).
// A chunk whose span is non-time is dropped, since subtitles require timing.
func BuildCues(chunks []TranscriptChunk) []Cue {
	type timed struct {
		start   int
		end     int
		text    string
		speaker string
	}
	timedChunks := make([]timed, 0, len(chunks))
	for _, ch := range chunks {
		if !strings.EqualFold(strings.TrimSpace(ch.Span.Kind), "time") {
			continue
		}
		text := strings.TrimSpace(ch.Text)
		if text == "" {
			continue
		}
		start := ch.Span.StartMS
		end := ch.Span.EndMS
		if start < 0 {
			start = 0
		}
		if end < start {
			end = start
		}
		// Prefer the human-readable label, falling back to the stable id, so a
		// diarized transcript exports voice markup (SPEC §8.6.3); empty for a
		// non-diarized transcript so the export is unchanged.
		speaker := strings.TrimSpace(ch.Span.SpeakerLabel)
		if speaker == "" {
			speaker = strings.TrimSpace(ch.Span.Speaker)
		}
		timedChunks = append(timedChunks, timed{start: start, end: end, text: text, speaker: speaker})
	}

	sort.SliceStable(timedChunks, func(i, j int) bool {
		if timedChunks[i].start != timedChunks[j].start {
			return timedChunks[i].start < timedChunks[j].start
		}
		return timedChunks[i].end < timedChunks[j].end
	})

	cues := make([]Cue, 0, len(timedChunks))
	for i, tc := range timedChunks {
		cues = append(cues, Cue{
			Index:   i + 1,
			StartMS: tc.start,
			EndMS:   tc.end,
			Text:    tc.text,
			Speaker: tc.speaker,
		})
	}
	return cues
}

// Broadcast segmentation norms. BuildCues emits one cue per stored transcript
// chunk (a whisper segment), which can run up to ~30 s / 150+ characters — fine
// as a transcript, but not broadcast-legible. BuildBroadcastCues re-segments
// from per-word timings into cues that obey these standard subtitle norms.
// Values are in milliseconds unless noted; they mirror the pilot prototype that
// validated on the RFE/RL corpus.
const (
	bcMaxDurMS   = 6000 // hard cap on cue length
	bcMinDurMS   = 1200 // minimum on-screen time
	bcMaxChars   = 84   // two lines x 42 characters
	bcMaxLine    = 42   // characters per line
	bcTargetCPS  = 16.0 // aim below the 17 CPS ceiling so rounding stays in-spec
	bcLeadMS     = 400  // max lead-in: a cue may appear this early into preceding silence
	bcGapMinMS   = 50   // minimum gap kept between adjacent cues (never overlap)
	bcPauseMS    = 600  // inter-word gap that justifies a break
	bcSentMinLen = 30   // min characters before a sentence-end may end a cue early
)

// bcNoSpaceBefore are runes that attach to the preceding token (no space
// inserted before them), i.e. closing punctuation. bcNoSpaceAfter are runes
// that attach to the following token (no space after), i.e. opening brackets and
// guillemets. Because whisperapi trims each word token (client.go), the raw
// leading spaces whisper emits are gone, so text must be re-spaced on rebuild;
// this detokenization keeps punctuation flush with its word. Ambiguous straight
// quotes are left to default spacing (over-spacing is safer than mis-attaching).
const (
	bcNoSpaceBefore = ".,!?;:…»)]}%”’"
	bcNoSpaceAfter  = "«([{“‘"
)

// broadcastWord is one per-word timing collapsed from a WordSpan: absolute start
// and end in ms, the trimmed token text, and the diarized speaker of its span
// (empty for a non-diarized transcript) so the segmenter never merges two
// speakers into one cue.
type broadcastWord struct {
	start   int
	end     int
	text    string
	speaker string
}

// BuildBroadcastCues re-segments a transcript's per-word timings (model.Span.Words,
// spec §8.6.1) into broadcast-legible cues: <= bcMaxDurMS / <= bcMaxChars and
// >= bcMinDurMS, breaking at sentence ends and speech pauses, then buying display
// time from surrounding silence (end-extend, then a small lead-in) without ever
// overlapping a neighbour. The cue TEXT is the verbatim word tokens re-joined —
// only cue boundaries and timing are computed, never the words. It returns nil
// when no span carries word timings, so callers fall back to BuildCues (the
// chunk-per-cue path) and a words-absent transcript is unchanged.
//
// Broadcast re-segmentation currently supports only space-delimited scripts
// (Latin, Cyrillic, Greek, Hangul, …). It rejoins trimmed word tokens with an
// inserted ASCII space (appendBroadcastToken) and wraps/measures lines on that
// space, which is correct for scripts that separate words with spaces but would
// FABRICATE spaces inside a spaceless script (Han/Kana/Thai), where Whisper emits
// one token per character/syllable — turning "你好世界" into "你 好 世 界" and
// violating the "text is never modified" contract. When such a script dominates
// the word tokens, this returns nil so the caller falls back to BuildCues, which
// reproduces the stored chunk text verbatim (no space injection, no corruption).
func BuildBroadcastCues(chunks []TranscriptChunk) []Cue {
	words := collectBroadcastWords(chunks)
	if len(words) == 0 {
		return nil
	}
	if !wordsNeedSpaceJoining(words) {
		// Spaceless script (Han/Kana/Thai): the broadcast path's space-joining
		// would corrupt the text. Fall back to the verbatim chunk-per-cue builder.
		return nil
	}
	return relaxBroadcastTiming(segmentBroadcastWords(words))
}

// spacelessScriptPercent is the share of alphabetic runes that must belong to a
// spaceless script (Han/Kana/Thai) before a transcript is treated as spaceless
// and routed away from the space-joining broadcast path. It is deliberately low:
// a genuinely space-delimited transcript (Latin/Cyrillic) has ~0% such runes, so
// even a modest presence is a reliable signal without misclassifying the odd
// borrowed ideograph in an otherwise Latin caption.
const spacelessScriptPercent = 30

// isSpacelessRune reports whether r belongs to a script written without
// inter-word spaces, where per-character ASR tokens must NOT be space-joined:
// Han (Chinese, incl. CJK extensions and Japanese kanji), Hiragana, Katakana,
// and Thai. Hangul is intentionally excluded — Korean IS space-delimited, so it
// stays on the broadcast path.
func isSpacelessRune(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Thai, r)
}

// wordsNeedSpaceJoining reports whether a transcript's word tokens come from a
// space-delimited script and can therefore be safely rejoined with inserted
// spaces (the broadcast path). It inspects only alphabetic runes; a transcript
// where at least spacelessScriptPercent of them are in a spaceless script
// (Han/Kana/Thai) returns false so the caller falls back to verbatim chunk cues.
// A token set with no alphabetic runes (pure punctuation/digits) has no space
// convention to violate, so it stays on the broadcast path.
func wordsNeedSpaceJoining(words []broadcastWord) bool {
	var letters, spaceless int
	for _, w := range words {
		for _, r := range w.text {
			if !unicode.IsLetter(r) {
				continue
			}
			letters++
			if isSpacelessRune(r) {
				spaceless++
			}
		}
	}
	if letters == 0 {
		return true
	}
	return spaceless*100/letters < spacelessScriptPercent
}

// broadcastSeg is one cue's word-derived span before timing relaxation: the
// spoken [start,end] window, the rebuilt (trimmed) text, and the diarized
// speaker carried through to the emitted cue's voice markup.
type broadcastSeg struct {
	start   int
	end     int
	text    string
	speaker string
}

// segmentBroadcastWords groups ordered words into segments, starting a new
// segment whenever the speaker changes or shouldBreakBroadcast fires. Text is
// rebuilt incrementally via appendBroadcastToken so break decisions see the exact
// rendered length.
func segmentBroadcastWords(words []broadcastWord) []broadcastSeg {
	var segs []broadcastSeg
	haveCur := false
	var curStart, curLastEnd int
	var curText, curSpeaker string

	flush := func() {
		if !haveCur {
			return
		}
		if t := strings.TrimSpace(curText); t != "" {
			segs = append(segs, broadcastSeg{start: curStart, end: curLastEnd, text: t, speaker: curSpeaker})
		}
		haveCur = false
		curText = ""
		curSpeaker = ""
	}

	for _, w := range words {
		// A speaker change is a hard cue boundary: BuildCues keeps one cue per
		// (single-speaker) span, so the broadcast path must never merge two
		// speakers' words into one cue when re-segmenting from word timings.
		if haveCur && (w.speaker != curSpeaker || shouldBreakBroadcast(curText, curStart, curLastEnd, w)) {
			flush()
		}
		if !haveCur {
			curStart = w.start
			curText = w.text
			curSpeaker = w.speaker
			haveCur = true
		} else {
			curText = appendBroadcastToken(curText, w.text)
		}
		curLastEnd = w.end
	}
	flush()
	return segs
}

// shouldBreakBroadcast reports whether word w must start a new cue rather than
// extend the current one: it would overflow the char or duration cap, or a
// speech pause / sentence end offers a natural boundary. The minimum on-screen
// time (bcMinDurMS) is deliberately NOT gated here — it is a display constraint
// enforced later by relaxBroadcastTiming, which buys the time from surrounding
// silence. Gating a natural break on spoken duration would merge a short but
// complete utterance (e.g. a 300 ms "Yes." before a 2 s pause) into the next cue.
func shouldBreakBroadcast(curText string, curStart, curLastEnd int, w broadcastWord) bool {
	gap := w.start - curLastEnd
	switch {
	case utf8.RuneCountInString(appendBroadcastToken(curText, w.text)) > bcMaxChars:
		return true
	case w.end-curStart > bcMaxDurMS:
		return true
	case gap > bcPauseMS:
		return true
	case endsBroadcastSentence(curText) && utf8.RuneCountInString(curText) >= bcSentMinLen:
		return true
	default:
		return false
	}
}

// relaxBroadcastTiming turns segments into final cues, enforcing MIN_DUR and
// relaxing reading speed toward bcTargetCPS by buying display time from the
// surrounding silence, never overlapping a neighbour: first extend the END into
// the following gap; if still too dense, pull the START back into the preceding
// gap (up to bcLeadMS). Silence that isn't there can't be borrowed, so a cue over
// a fast talker stays dense — a property of the speech, not the segmentation.
func relaxBroadcastTiming(segs []broadcastSeg) []Cue {
	cues := make([]Cue, 0, len(segs))
	for i := range segs {
		prevEnd := 0
		if i > 0 {
			prevEnd = segs[i-1].end
		}
		next := segs[i].end + 2000
		if i+1 < len(segs) {
			next = segs[i+1].start
		}
		need := int(math.Round(float64(utf8.RuneCountInString(segs[i].text)) * 1000.0 / bcTargetCPS))
		if need < bcMinDurMS {
			need = bcMinDurMS
		}
		if segs[i].end-segs[i].start < need {
			end := segs[i].start + need
			if end > next-bcGapMinMS {
				end = next - bcGapMinMS
			}
			// Never pull the end before the spoken end: words are sorted by start
			// only, so overlapping ASR timings can place the next cue's start before
			// this cue's spoken end, and the overlap cap above must not shrink the
			// cue below where speech actually ends (which would truncate it or, when
			// next <= start, invert it to a zero/negative duration).
			if end < segs[i].end {
				end = segs[i].end
			}
			segs[i].end = end
		}
		if segs[i].end-segs[i].start < need {
			room := segs[i].start - (prevEnd + bcGapMinMS)
			if room > bcLeadMS {
				room = bcLeadMS
			}
			if room > 0 {
				segs[i].start -= room
			}
		}
		// Hard-cap the on-screen duration at bcMaxDurMS. Segmentation bounds the
		// SPOKEN span, but a single word whisper assigns a huge duration (or a
		// short cue that swallowed a long inter-word silence) can still leave a
		// segment whose end is many seconds past its start; the "never truncate
		// below spoken end" rule above would otherwise preserve a 20s+ cue.
		// Capping trims only display time (the tail is silence/over-held), never
		// inverts duration, and cannot introduce overlap since it only shortens.
		if segs[i].end-segs[i].start > bcMaxDurMS {
			segs[i].end = segs[i].start + bcMaxDurMS
		}
		cues = append(cues, Cue{
			Index:   i + 1,
			StartMS: segs[i].start,
			EndMS:   segs[i].end,
			Text:    wrapBroadcastLines(segs[i].text),
			Speaker: segs[i].speaker,
		})
	}
	return cues
}

// collectBroadcastWords flattens the per-word timings of every "time" span into
// playback order. A word's end is start+duration (WordSpan.D is a duration, not
// an end); negative starts/durations are clamped so timing is always valid.
// Empty tokens are skipped. Each word carries its span's diarized speaker (label
// preferred, stable id fallback — matching BuildCues, SPEC §8.6.3) so the
// segmenter can preserve voice markup and break on a speaker change.
func collectBroadcastWords(chunks []TranscriptChunk) []broadcastWord {
	var words []broadcastWord
	for _, ch := range chunks {
		if !strings.EqualFold(strings.TrimSpace(ch.Span.Kind), "time") {
			continue
		}
		speaker := strings.TrimSpace(ch.Span.SpeakerLabel)
		if speaker == "" {
			speaker = strings.TrimSpace(ch.Span.Speaker)
		}
		for _, w := range ch.Span.Words {
			tok := strings.TrimSpace(w.W)
			if tok == "" {
				continue
			}
			start := w.T
			if start < 0 {
				start = 0
			}
			dur := w.D
			if dur < 0 {
				dur = 0
			}
			words = append(words, broadcastWord{start: start, end: start + dur, text: tok, speaker: speaker})
		}
	}
	sort.SliceStable(words, func(i, j int) bool {
		if words[i].start != words[j].start {
			return words[i].start < words[j].start
		}
		return words[i].end < words[j].end
	})
	return words
}

// appendBroadcastToken joins tok onto text, inserting a single space unless the
// punctuation should stay flush (closing punctuation attaches to the preceding
// word; an opening bracket/guillemet attaches to the following word).
func appendBroadcastToken(text, tok string) string {
	if text == "" {
		return tok
	}
	firstTok, _ := utf8.DecodeRuneInString(tok)
	lastText, _ := utf8.DecodeLastRuneInString(text)
	// A token beginning with an ASCII hyphen-minus is the tail of a hyphenated
	// compound: whisper trims word tokens, so "so-called" arrives as "so" then
	// "-called". Attach it flush to render "so-called", not "so -called" — but
	// only after an alphanumeric, so a genuine leading dash (dialogue/range) is
	// not glued onto the previous word. Cyrillic "что-то", "все-таки" etc. are
	// covered because IsLetter is Unicode-aware.
	if firstTok == '-' && (unicode.IsLetter(lastText) || unicode.IsDigit(lastText)) {
		return text + tok
	}
	if strings.ContainsRune(bcNoSpaceBefore, firstTok) || strings.ContainsRune(bcNoSpaceAfter, lastText) {
		return text + tok
	}
	return text + " " + tok
}

// endsBroadcastSentence reports whether the trimmed text ends in sentence-final
// punctuation, a natural place to end a cue.
func endsBroadcastSentence(text string) bool {
	r, _ := utf8.DecodeLastRuneInString(strings.TrimSpace(text))
	return r == '.' || r == '!' || r == '?' || r == '…'
}

// wrapBroadcastLines splits text into two lines (a single embedded newline) when
// it exceeds one line, choosing the space break that MINIMIZES the longer line.
// Minimizing the max keeps both lines <= bcMaxLine whenever the text admits such
// a split (breaking nearest-the-middle did not: the middle space could still
// leave one line a few characters over the 42-char cap). Text that fits on one
// line, or has no interior space to break at, is returned unchanged. Rune-aware
// so multibyte scripts wrap by character count, not byte count.
func wrapBroadcastLines(text string) string {
	r := []rune(text)
	if len(r) <= bcMaxLine {
		return text
	}
	best := -1
	bestMax := len(r) + 1
	for i, ch := range r {
		if ch != ' ' {
			continue
		}
		longer := i // first line length (runes before the space)
		if second := len(r) - i - 1; second > longer {
			longer = second
		}
		if longer < bestMax {
			bestMax = longer
			best = i
		}
	}
	if best == -1 {
		return text
	}
	return string(r[:best]) + "\n" + string(r[best+1:])
}

// RenderVTT serializes cues as a WebVTT document. The output is the literal
// "WEBVTT" header, a blank line, then each non-empty cue as a
// "HH:MM:SS.mmm --> HH:MM:SS.mmm" timing line followed by its text and a
// trailing blank line. Cues with empty text are skipped. Cue indices are not
// emitted (they are optional in WebVTT).
func RenderVTT(cues []Cue) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for _, cue := range cues {
		text := strings.TrimSpace(cue.Text)
		if text == "" {
			continue
		}
		b.WriteString(formatTimestampVTT(cue.StartMS))
		b.WriteString(" --> ")
		b.WriteString(formatTimestampVTT(cue.EndMS))
		b.WriteByte('\n')
		// Carry speaker as a WebVTT <v Name> voice tag when present (SPEC §8.6.3);
		// a cue with no speaker renders exactly as before. The name is sanitised
		// so it can never break out of the tag.
		if speaker := sanitizeVoiceName(cue.Speaker); speaker != "" {
			b.WriteString("<v ")
			b.WriteString(speaker)
			b.WriteByte('>')
		}
		b.WriteString(escapeVTTText(text))
		b.WriteString("\n\n")
	}
	return b.String()
}

// voiceNameReplacer makes a speaker name safe to embed in a WebVTT <v Name>
// tag: it strips '<'/'>' (which would terminate the tag or inject markup),
// escapes '&' (a bare ampersand is an invalid entity in the tag), and replaces
// CR/LF with a space (a newline would leave the <v ...> tag unterminated).
var voiceNameReplacer = strings.NewReplacer(
	"<", "",
	">", "",
	"&", "&amp;",
	"\r", " ",
	"\n", " ",
)

// sanitizeVoiceName makes a speaker name safe to embed in a WebVTT <v Name> tag.
// Returns "" for an empty/blank name so the caller omits the voice tag entirely.
func sanitizeVoiceName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return strings.TrimSpace(voiceNameReplacer.Replace(name))
}

// RenderSRT serializes cues as a SubRip (SRT) document: a 1-based index line,
// a "HH:MM:SS,mmm --> HH:MM:SS,mmm" timing line (comma decimal separator), the
// text, and a blank separator line between cues. Empty-text cues are skipped,
// and indices are renumbered contiguously over the surviving cues so the output
// is always a valid, gap-free SRT regardless of the Index values on input.
func RenderSRT(cues []Cue) string {
	var b strings.Builder
	idx := 0
	for _, cue := range cues {
		text := strings.TrimSpace(cue.Text)
		if text == "" {
			continue
		}
		idx++
		fmt.Fprintf(&b, "%d\n", idx)
		b.WriteString(formatTimestampSRT(cue.StartMS))
		b.WriteString(" --> ")
		b.WriteString(formatTimestampSRT(cue.EndMS))
		b.WriteByte('\n')
		b.WriteString(neutralizeSRTText(text))
		b.WriteString("\n\n")
	}
	return b.String()
}

// splitTimestamp decomposes a non-negative millisecond value into zero-padded
// hours, minutes, seconds, and milliseconds components. Negative inputs are
// clamped to zero so a malformed span can never produce a negative timestamp.
func splitTimestamp(ms int) (hours, minutes, seconds, millis int) {
	if ms < 0 {
		ms = 0
	}
	millis = ms % 1000
	totalSeconds := ms / 1000
	seconds = totalSeconds % 60
	totalMinutes := totalSeconds / 60
	minutes = totalMinutes % 60
	hours = totalMinutes / 60
	return hours, minutes, seconds, millis
}

// formatTimestampVTT renders ms as a WebVTT timestamp "HH:MM:SS.mmm" with a dot
// before the millisecond field.
func formatTimestampVTT(ms int) string {
	h, m, s, msec := splitTimestamp(ms)
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, msec)
}

// formatTimestampSRT renders ms as an SRT timestamp "HH:MM:SS,mmm" with a comma
// before the millisecond field.
func formatTimestampSRT(ms int) string {
	h, m, s, msec := splitTimestamp(ms)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, msec)
}

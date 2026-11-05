package subtitle

import (
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

// ReflowTranslatedCues is ReflowChunkCues for a translated transcript that has
// no word timings of its own, timed against the speech of its source transcript
// (SPEC §8.6.3, "Translated cues follow the source speech", 0.87.0).
//
// A translated segment carries the start_ms of the source segment it translates
// (§8.6.2). When that source segment has word timings, it is split at each gap
// of at least pauseMS between the end of one source word and the start of the
// next one. Each part is a spoken run. The translated tokens go to the runs in
// order, in proportion to the source words of each run, and each share is spread
// over its own run only. So no translated token falls inside a source pause.
// Display-time relaxation can extend a cue into the pause that follows it, but
// never past the start of the next spoken run, and a cue that opens a run never
// leads in to the pause before it.
//
// pauseMS <= 0 turns the rule off. A translated segment whose source segment has
// no word timings, or no pause, is spread per segment as ReflowChunkCues does.
// So with no source, or with the rule off, the result equals ReflowChunkCues.
func ReflowTranslatedCues(cues []Cue, source []TranscriptChunk, pauseMS int) []Cue {
	if pauseMS <= 0 || len(source) == 0 {
		return ReflowChunkCues(cues)
	}
	speech := newSourceSpeech(source)
	if len(speech.words) == 0 {
		return ReflowChunkCues(cues)
	}
	var words []broadcastWord
	for _, c := range cues {
		words = append(words, speech.timeCue(c, pauseMS)...)
	}
	if len(words) == 0 {
		return cues
	}
	return relaxBroadcastTiming(segmentBroadcastWords(words))
}

// sourceSpeech indexes the source transcript: the start of each source segment
// mapped to the start of the next one (the end of its marker window, §8.6.2),
// and every source word in playback order.
type sourceSpeech struct {
	windowEnd map[int]int
	words     []broadcastWord
}

// newSourceSpeech builds the index. A merged source chunk is split back into
// its segments (BuildCues), and the words of all chunks are collected in start
// order. The window of the last segment is open (math.MaxInt).
func newSourceSpeech(source []TranscriptChunk) sourceSpeech {
	segs := BuildCues(source)
	windowEnd := make(map[int]int, len(segs))
	for i, s := range segs {
		if _, seen := windowEnd[s.StartMS]; seen {
			continue
		}
		windowEnd[s.StartMS] = nextSegmentStart(segs, i)
	}
	return sourceSpeech{windowEnd: windowEnd, words: collectBroadcastWords(source)}
}

// nextSegmentStart returns the first segment start after segs[i].StartMS, or
// math.MaxInt when no later segment exists. segs is in start order.
func nextSegmentStart(segs []Cue, i int) int {
	for j := i + 1; j < len(segs); j++ {
		if segs[j].StartMS > segs[i].StartMS {
			return segs[j].StartMS
		}
	}
	return math.MaxInt
}

// wordsIn returns the source words that start in [start, end).
func (s sourceSpeech) wordsIn(start, end int) []broadcastWord {
	lo := sort.Search(len(s.words), func(i int) bool { return s.words[i].start >= start })
	hi := lo
	for hi < len(s.words) && s.words[hi].start < end {
		hi++
	}
	return s.words[lo:hi]
}

// speechRun is one spoken run of a source segment: its first word start, its
// last word end, and how many source words it holds.
type speechRun struct {
	start int
	end   int
	words int
}

// splitSpeechRuns splits ordered source words at each gap of at least pauseMS
// between the end of the run so far and the start of the next word.
func splitSpeechRuns(words []broadcastWord, pauseMS int) []speechRun {
	var runs []speechRun
	for i, w := range words {
		if i == 0 || w.start-runs[len(runs)-1].end >= pauseMS {
			runs = append(runs, speechRun{start: w.start, end: w.end})
		}
		r := &runs[len(runs)-1]
		r.words++
		if w.end > r.end {
			r.end = w.end
		}
	}
	return runs
}

// timeCue synthesizes the word times of one translated cue. It falls back to
// the per-segment spreading when the cue has no paired source segment, or when
// the source segment holds fewer than two spoken runs.
func (s sourceSpeech) timeCue(c Cue, pauseMS int) []broadcastWord {
	toks := cueTokens(c)
	if len(toks) == 0 {
		return nil
	}
	start, end := clampCueSpan(c)
	windowEnd, ok := s.windowEnd[c.StartMS]
	if !ok {
		return spreadTokens(toks, start, end)
	}
	runs := splitSpeechRuns(s.wordsIn(c.StartMS, windowEnd), pauseMS)
	if len(runs) < 2 {
		return spreadTokens(toks, start, end)
	}
	return spreadOverRuns(toks, runs, start, end)
}

// cueTokens splits cue text into tokens, with a newline read as a space.
func cueTokens(c Cue) []string {
	return strings.Fields(strings.ReplaceAll(c.Text, "\n", " "))
}

// clampCueSpan returns a cue's span with a negative start clamped to zero and
// an end that never precedes the start.
func clampCueSpan(c Cue) (int, int) {
	start, end := c.StartMS, c.EndMS
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	return start, end
}

// spreadOverRuns gives each run its share of the tokens and spreads the share
// over the run, clamped to the translated cue's own span. Every token of a run
// that another run follows carries the start of that next run as its hold. The
// first token of every run after the first is marked as a run start.
func spreadOverRuns(toks []string, runs []speechRun, cueStart, cueEnd int) []broadcastWord {
	counts := shareTokens(len(toks), runs)
	words := make([]broadcastWord, 0, len(toks))
	at := 0
	for i, r := range runs {
		share := toks[at : at+counts[i]]
		at += counts[i]
		if len(share) == 0 {
			continue
		}
		part := spreadTokens(share, clampInto(r.start, cueStart, cueEnd), clampInto(r.end, cueStart, cueEnd))
		hold := 0
		if i+1 < len(runs) {
			hold = runs[i+1].start
		}
		for k := range part {
			part[k].hold = hold
		}
		part[0].runStart = i > 0
		words = append(words, part...)
	}
	return words
}

// clampInto clamps v into [lo, hi].
func clampInto(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// shareTokens splits total tokens over the runs in proportion to the source
// words of each run. It uses the largest remainder rule (a tie goes to the
// earlier run), so the counts add up to total. Then fillEmptyRuns gives a token
// to each run that has none while tokens remain to move.
func shareTokens(total int, runs []speechRun) []int {
	sum := 0
	for _, r := range runs {
		sum += r.words
	}
	counts := make([]int, len(runs))
	rems := make([]int, len(runs))
	given := 0
	for i, r := range runs {
		counts[i] = total * r.words / sum
		rems[i] = total * r.words % sum
		given += counts[i]
	}
	order := make([]int, len(runs))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return rems[order[a]] > rems[order[b]] })
	for k := 0; given < total; k++ {
		counts[order[k]]++
		given++
	}
	fillEmptyRuns(counts)
	return counts
}

// fillEmptyRuns moves one token to each run that has none. The token comes from
// the run with the most tokens (the earliest on a tie), and only while that run
// has more than one. So every run gets a token when there are enough tokens.
func fillEmptyRuns(counts []int) {
	for i := range counts {
		if counts[i] > 0 {
			continue
		}
		most := 0
		for j := range counts {
			if counts[j] > counts[most] {
				most = j
			}
		}
		if counts[most] < 2 {
			return
		}
		counts[most]--
		counts[i]++
	}
}

// spreadTokens spreads [start, end] over the tokens in proportion to token rune
// length, with a trailing space counted in each weight so a punctuation-only
// token still gets a slice.
func spreadTokens(toks []string, start, end int) []broadcastWord {
	span := end - start
	total := 0
	for _, t := range toks {
		total += utf8.RuneCountInString(t) + 1
	}
	words := make([]broadcastWord, 0, len(toks))
	acc := 0
	for _, t := range toks {
		ws := start + span*acc/total
		acc += utf8.RuneCountInString(t) + 1
		we := start + span*acc/total
		words = append(words, broadcastWord{start: ws, end: we, text: t})
	}
	return words
}

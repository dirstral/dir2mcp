package retrieval

import (
	"context"
	"regexp"
	"strings"
	"unicode"

	"github.com/dirstral/dir2mcp/internal/usage"
)

// The answer-language rule (#880), the trailing reminder (#892) and the
// proper-noun clause (#957) all tell the model, in words, to answer in the
// language of the question. On a corpus written in the SAME script as the
// question that is enough. On a cross-lingual corpus it is not.
//
// Measured 2026-09-12 on an eight-recording Ukrainian/Kyrgyz/Russian/Georgian
// archive (broadcast material, 4h41m), chat provider openai / gpt-5.4-mini, three
// English questions asked six times each:
//
//	shipped rule + reminder ............. 13 of 18 answers in English
//	with the #957 proper-noun clause .... 14 of 18
//	with coarser transcript chunks ....... 7 of 18
//
// The answers were not in a random language: they were in the language of the
// retrieved CONTEXT. The pull scales with how much coherent foreign text the
// context holds, which is why the chunk window of #955 doubled the failure rate
// while improving the retrieval itself.
//
// The obvious remedy, naming the question's language in the prompt, is unsafe
// with the detector dir2mcp ships. whatlanggo is a trigram detector and a
// question is short and function-word heavy, which is its worst case:
//
//	"What is said about Crimea in these recordings?" -> Afrikaans (0.282)
//	"When did Dmytro Kuleba become Minister of Foreign Affairs?" -> German (0.356)
//	"Did Matt Chapman hit a home run?" -> Haitian Creole (0.043)
//
// Pinning one of those would be far worse than the drift it tried to fix.
//
// The SCRIPT of a question is a different matter: it is decided by counting
// runes, not by guessing, and it is exactly the axis the observed failures cross
// (a question in Latin answered in Cyrillic). So the reminder names the script,
// which is always right when it is stated at all, and leaves the language to the
// wording that is already there.

// scriptNames are the Unicode scripts a question may be written in, with the
// English name to put in the prompt. The list is deliberately about writing
// systems, not languages: it never has to be extended for a new language, and it
// carries no domain or region assumption.
var scriptNames = []struct {
	name  string
	table *unicode.RangeTable
}{
	{"Latin", unicode.Latin},
	{"Cyrillic", unicode.Cyrillic},
	{"Greek", unicode.Greek},
	{"Arabic", unicode.Arabic},
	{"Hebrew", unicode.Hebrew},
	{"Devanagari", unicode.Devanagari},
	{"Han", unicode.Han},
	{"Hiragana", unicode.Hiragana},
	{"Katakana", unicode.Katakana},
	{"Hangul", unicode.Hangul},
	{"Georgian", unicode.Georgian},
	{"Armenian", unicode.Armenian},
	{"Thai", unicode.Thai},
	{"Bengali", unicode.Bengali},
	{"Tamil", unicode.Tamil},
	{"Ethiopic", unicode.Ethiopic},
}

// minScriptLetters is the fewest letters a question must have before its script
// is named. Below this a single stray word decides the verdict, and a wrong
// instruction is worse than none.
const minScriptLetters = 8

// scriptShare is how much of a question's letter content the leading script must
// cover, and scriptMargin how far it must beat everything else, before the
// question is called that script.
//
// Two rules rather than one, because a single share threshold cannot separate
// the two cases that matter. "When is <a two-word Cyrillic name> on screen in
// the broadcast?" is a Latin question with a foreign name in it, at 68% Latin.
// "Крым Crimea Крым Crimea" is not a question in any script, at 60% Latin. The
// margin rule keeps the first and drops the second: a leading script that does
// not beat the rest put together is not dominant, it is merely ahead.
const (
	scriptShare  = 0.6
	scriptMargin = 2.0
)

// dominantScript names the script a question is written in, or "" when no script
// covers enough of it to be worth stating. Punctuation, digits and spaces do not
// vote: only letters do.
func dominantScript(question string) string {
	return dominantBy(question, func(name string) string { return name })
}

// dominantBy is dominantScript with the letters grouped by key before the
// share and margin rules run. The identity key gives dominantScript; the
// scriptFamily key gives dominantFamily.
func dominantBy(text string, key func(string) string) string {
	counts := make(map[string]int, len(scriptNames))
	letters := 0
	for _, r := range text {
		if !unicode.IsLetter(r) {
			continue
		}
		letters++
		for i := range scriptNames {
			if unicode.Is(scriptNames[i].table, r) {
				counts[key(scriptNames[i].name)]++
				break
			}
		}
	}
	if letters < minScriptLetters {
		return ""
	}
	// Walk scriptNames, not the map, so a tie resolves the same way on every
	// run: the first listed script wins, as it always has.
	best, bestN := "", 0
	for i := range scriptNames {
		k := key(scriptNames[i].name)
		if n := counts[k]; n > bestN {
			best, bestN = k, n
		}
	}
	if best == "" {
		return ""
	}
	if float64(bestN)/float64(letters) < scriptShare {
		return ""
	}
	// Letters in no listed script (and letters in the runner-up scripts) count
	// against the leader together: an unlisted writing system must not be read as
	// a silent endorsement of whichever listed one happens to lead.
	others := letters - bestN
	if float64(bestN) < scriptMargin*float64(others) {
		return ""
	}
	return best
}

// dominantFamily names the writing system a text is written in (see
// scriptFamily), or "". Han and kana letters vote together, so a Japanese text
// that splits its letters between them still has a clear verdict.
func dominantFamily(text string) string {
	return dominantBy(text, scriptFamily)
}

// scriptReminder is the sentence appended to the trailing language reminder when
// the question has a clear script. It is additive: the language wording stays
// exactly as it was, and this only closes the door the measurements showed open.
//
// It says "alphabet" rather than "script" because the word is the plainer one,
// and it allows the one legitimate exception, a quoted passage, so the model is
// not pushed into transliterating the evidence it is citing.
func scriptReminder(question string) string {
	name := dominantScript(question)
	if name == "" {
		return ""
	}
	return "The question above is written in the " + name + " alphabet, and the " +
		"answer must be written in that alphabet too, whatever alphabet the " +
		"documents use. Quoted words from a document keep their own spelling; " +
		"everything you write yourself does not.\n"
}

// ScriptReminderForQuestion is the exported form of scriptReminder, for tests in
// the tests/ tree. It returns "" when the question has no clear script.
func ScriptReminderForQuestion(question string) string { return scriptReminder(question) }

// The script check (#1083, SPEC §9.4.6). The rule, the reminder and the
// script sentence above all work on the INPUT. A small local model can still
// write the answer in another script: in a 344-question benchmark run,
// qwen2.5:7b-instruct answered "What is the 16th century known as the start
// of?" over an English corpus in Chinese, with all three in the prompt. More
// words in the prompt cannot close that gap. A check of the OUTPUT can, and the
// script is the one axis that is counted, not guessed.
//
// The check is deliberately narrow, so a wrong verdict costs at most one call:
//
//   - both texts must have a dominant script (the same dominantScript rules), so
//     a short answer, a mixed answer or a question with no clear script is never
//     corrected;
//   - inline citation tags are removed first, because a tag carries a file name
//     and a Latin file name must not vote for the answer's script;
//   - Han, Hiragana and Katakana are one family, so a Japanese answer to a
//     Japanese question is not a mismatch whichever of the three leads;
//   - at most one more generation, and its answer replaces the first only when
//     its dominant script is positively the question's. Absence of a verdict is
//     not a pass.

// hanKanaFamily is the name of the one writing system Han, Hiragana and
// Katakana form together. It names scripts, not a language.
const hanKanaFamily = "Han and Kana"

// scriptFamily maps a script name from scriptNames to the writing system it
// belongs to. Scripts that one language writes together share a family.
func scriptFamily(name string) string {
	switch name {
	case "Han", "Hiragana", "Katakana":
		return hanKanaFamily
	}
	return name
}

// markdownLinkTargetRe matches the destination half of a markdown link,
// "](https://...)". The label before it is answer text; the destination is a
// URL, whose Latin letters must not vote for the answer's script.
//
// It accepts the CommonMark forms a model writes: a destination with one level
// of balanced parentheses ("wiki/Foo_(bar)"), an angle-bracket destination, and
// an optional title in double quotes, single quotes or parentheses.
var markdownLinkTargetRe = regexp.MustCompile(
	`\]\(\s*(?:<[^<>\n]*>|[^()\s]*(?:\([^()\s]*\)[^()\s]*)*)` +
		`(?:\s+(?:"[^"\n]*"|'[^'\n]*'|\([^()\n]*\)))?\s*\)`)

// quotedPassageRe matches a quoted passage: text in straight double quotes,
// curly quotes, guillemets, low-high quotes or corner brackets, on one line or
// across lines, and a markdown block quote line ("> ..."). Single quotes are
// left out on purpose: an apostrophe ("Park's") is the same character.
//
// A stray unpaired mark can pair with a later one and remove the text between
// them. That only removes letters from the count, so its worst case is "no
// verdict", which never causes a retry: the safe direction.
var quotedPassageRe = regexp.MustCompile(
	`"[^"]*"|\x{201C}[^\x{201D}]*\x{201D}|\x{00AB}[^\x{00BB}]*\x{00BB}|` +
		`\x{201E}[^\x{201C}\x{201D}]*[\x{201C}\x{201D}]|\x{300C}[^\x{300D}]*\x{300D}|\x{300E}[^\x{300F}]*\x{300F}|` +
		`(?m:^[ \t]*>.*$)`)

// bareURLRe matches a URL written in the text, bare or as an <...> autolink.
var bareURLRe = regexp.MustCompile(`<?\b(?:[A-Za-z][A-Za-z0-9+.-]*://|www\.)[^\s<>]*>?`)

// codeSpanRe matches a fenced code block (``` or ~~~) and an inline code span.
// Code is written in the script of the programming language, not the answer's.
var codeSpanRe = regexp.MustCompile("(?s)```.*?```|~~~.*?~~~|`[^`\\n]*`")

// answerScript names the writing system (scriptFamily) of an answer's own prose,
// or "" when that prose has no dominant one.
//
// Four kinds of text are removed first, because none of them is the answer's
// own words: code (a Russian answer about a code corpus can hold a long Latin
// code block), citation tags (they carry file names), URLs (link destinations,
// autolinks and bare URLs) and quoted passages. The prompt lets a quote keep
// its own spelling, so an English answer that quotes a long Cyrillic passage is
// an English answer and must not be sent back to be rewritten.
//
// Only a bracket that names a document is removed (the same parse §9.4.1 uses).
// A bracket that holds prose, such as a markdown link label, is answer text and
// keeps its vote.
func answerScript(answer string) string {
	text := codeSpanRe.ReplaceAllString(answer, " ")
	text = markdownLinkTargetRe.ReplaceAllString(text, "]")
	text = bareURLRe.ReplaceAllString(text, " ")
	text = quotedPassageRe.ReplaceAllString(text, " ")
	text = inlineCitationRe.ReplaceAllStringFunc(text, func(match string) string {
		if inlineCitationTagPath(match) == "" {
			return match
		}
		return " "
	})
	return dominantFamily(text)
}

// answerScriptMismatch reports whether the answer is clearly written in another
// writing system than the question. want is the question's and got the
// answer's, both as scriptFamily names. Both are "" when either text has no
// dominant one, and then there is no mismatch.
func answerScriptMismatch(question, answer string) (want, got string, mismatch bool) {
	want = dominantFamily(question)
	if want == "" {
		return "", "", false
	}
	got = answerScript(answer)
	if got == "" {
		return "", "", false
	}
	return want, got, got != want
}

// AnswerScriptMismatch is the exported form of answerScriptMismatch, for tests
// in the tests/ tree.
func AnswerScriptMismatch(question, answer string) bool {
	_, _, mismatch := answerScriptMismatch(question, answer)
	return mismatch
}

// ragScriptCorrectionHeader delimits the correction section of the second
// prompt. It sits after the Reminder section, so it is the last thing the model
// reads, for the same reason the reminder is (#892).
const ragScriptCorrectionHeader = "Correction:\n"

// scriptCorrection is the instruction appended to the prompt for the second
// generation. Like the reminder, it speaks only about the script and language
// of the answer: it is the last instruction in the prompt, after the injection
// guard, so it must not touch grounding or citations.
func scriptCorrection(want, got string) string {
	return "\n" + ragScriptCorrectionHeader +
		"A previous answer to this question was written in the " + got + " alphabet. " +
		"The question is written in the " + want + " alphabet. Write the whole answer " +
		"in the " + want + " alphabet, in the language of the question. Quoted words " +
		"from a document keep their own spelling; everything you write yourself does not.\n"
}

// correctAnswerScript runs the §9.4.6 check on a generated answer and, on a
// mismatch, generates the answer once more with scriptCorrection appended to
// the same prompt. It returns the answer to publish: the second answer when its
// script is the question's, and the first answer in every other case (no
// mismatch, a failed or empty second call, or a second answer that still
// misses).
//
// It runs only when the prompt in force states the answer-language rule, the
// same condition that gates the reminder. An operator who fixed one answer
// language has replaced the rule, and correcting toward the question's script
// would overrule them.
//
// The log lines name the scripts only, never the question or the answer text.
func (s *Service) correctAnswerScript(ctx context.Context, question, prompt, systemPrompt, answer string) string {
	if !carriesAnswerLanguageRule(composeSystemPrompt(systemPrompt)) {
		return answer
	}
	want, got, mismatch := answerScriptMismatch(question, answer)
	if !mismatch {
		return answer
	}
	var second string
	err := usage.TimeStage(ctx, usage.StageGenerate, func() error {
		var gErr error
		second, gErr = s.gen.Generate(ctx, prompt+scriptCorrection(want, got))
		return gErr
	})
	if err != nil {
		s.logf("rag: answer script %s does not match question script %s; the second generation failed (%v), the first answer is kept", got, want, err)
		return answer
	}
	second = strings.TrimSpace(second)
	if again := answerScript(second); again != want {
		s.logf("rag: answer script %s does not match question script %s; the second answer has script %q, the first answer is kept", got, want, again)
		return answer
	}
	s.logf("rag: answer script %s does not match question script %s; corrected by one more generation", got, want)
	return second
}

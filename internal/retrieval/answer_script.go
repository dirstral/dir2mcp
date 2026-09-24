package retrieval

import (
	"unicode"
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
	counts := make([]int, len(scriptNames))
	letters := 0
	for _, r := range question {
		if !unicode.IsLetter(r) {
			continue
		}
		letters++
		for i := range scriptNames {
			if unicode.Is(scriptNames[i].table, r) {
				counts[i]++
				break
			}
		}
	}
	if letters < minScriptLetters {
		return ""
	}
	best, bestN := -1, 0
	for i, n := range counts {
		if n > bestN {
			best, bestN = i, n
		}
	}
	if best < 0 {
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
	return scriptNames[best].name
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

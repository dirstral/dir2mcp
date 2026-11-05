package tests

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// Issue #1083 (SPEC §9.4.6): a small local model can answer an English question
// in another script even with the answer-language rule, the reminder (#892) and
// the script sentence (#957) in the prompt. The fix checks the OUTPUT: an answer
// clearly written in another script than the question is generated once more,
// and the second answer is published only when its script is the question's.

// scriptedGenerator returns its outputs in order, one per call, and records
// every prompt. A call past the end of outs is a test failure: the check allows
// at most one more generation, so a third call is the bug.
type scriptedGenerator struct {
	t       *testing.T
	outs    []string
	errs    []error
	prompts []string
}

func (g *scriptedGenerator) Generate(_ context.Context, prompt string) (string, error) {
	n := len(g.prompts)
	g.prompts = append(g.prompts, prompt)
	if n >= len(g.outs) {
		g.t.Errorf("generator called %d times; the script check allows at most %d", n+1, len(g.outs))
		return "", errors.New("unexpected call")
	}
	if n < len(g.errs) && g.errs[n] != nil {
		return "", g.errs[n]
	}
	return g.outs[n], nil
}

const (
	question1083 = "what happened on Riley Park's home run"
	// The measured failure, with this suite's file name in the tag. The tag is
	// Latin, and it must not vote for the answer's script.
	hanAnswer1083   = "16世纪被称为法国宗教战争开始的时期。[" + gameFile + "]"
	latinAnswer1083 = "Riley Park homered in the sixth. [" + gameFile + "]"
)

func ask1083(t *testing.T, gen *scriptedGenerator, systemPrompt string) model.AskResult {
	t.Helper()
	svc := buildAnnotationService(t, gen, chapmanMoment())
	if systemPrompt != "" {
		svc.SetRAGSystemPrompt(systemPrompt)
	}
	res, err := svc.Ask(context.Background(), question1083, model.SearchQuery{K: 10})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	return res
}

// TestAsk1083_WrongScriptIsGeneratedOnceMore is the fix: the first answer is in
// Han, the second in Latin, and the Latin one is published.
func TestAsk1083_WrongScriptIsGeneratedOnceMore(t *testing.T) {
	gen := &scriptedGenerator{t: t, outs: []string{hanAnswer1083, latinAnswer1083}}
	res := ask1083(t, gen, "")

	if len(gen.prompts) != 2 {
		t.Fatalf("generator calls = %d, want 2 (one answer, one correction)", len(gen.prompts))
	}
	if !strings.HasPrefix(res.Answer, "Riley Park homered in the sixth.") {
		t.Fatalf("published answer = %q, want the second (Latin) answer", res.Answer)
	}
	if strings.Contains(res.Answer, "世纪") {
		t.Fatalf("the Han answer was published: %q", res.Answer)
	}
	// The second prompt is the first one plus a trailing correction, so the
	// model reads the same question, rules and context.
	first, second := gen.prompts[0], gen.prompts[1]
	if !strings.HasPrefix(second, first) {
		t.Fatal("the second prompt does not start with the first prompt")
	}
	correction := second[len(first):]
	for _, want := range []string{"Correction:", "Latin alphabet", "Han and Kana alphabet", "language of the question"} {
		if !strings.Contains(correction, want) {
			t.Errorf("correction does not mention %q: %q", want, correction)
		}
	}
	// It is the last instruction, after the injection guard, so it must speak
	// only about language (the same guard as #892's reminder).
	for _, forbidden := range []string{"ignore", "instead of", "only the provided context", "[rel_path]"} {
		if strings.Contains(strings.ToLower(correction), forbidden) {
			t.Errorf("correction mentions %q, which reaches beyond language: %q", forbidden, correction)
		}
	}
	// The published answer is generated, cited and in context.
	if res.AnswerSource != "" {
		t.Errorf("answer_source = %q, want empty (generated)", res.AnswerSource)
	}
	if len(res.Citations) == 0 {
		t.Error("the corrected answer lost its citations")
	}
}

// TestAsk1083_UncitedSecondAnswerKeepsTheStructuredCitations: the second answer
// may omit the inline tag the first one carried. The structured citations do
// not come from the answer text: they are the in-context set (§9.4.1), so the
// published result still names what the model was shown. This is the same
// outcome as a first answer that cites nothing inline.
func TestAsk1083_UncitedSecondAnswerKeepsTheStructuredCitations(t *testing.T) {
	gen := &scriptedGenerator{t: t, outs: []string{hanAnswer1083, "Riley Park homered in the sixth inning."}}
	res := ask1083(t, gen, "")
	if !strings.HasPrefix(res.Answer, "Riley Park homered in the sixth inning.") {
		t.Fatalf("answer = %q, want the second answer", res.Answer)
	}
	if len(res.Citations) == 0 {
		t.Fatal("the structured citations were lost with the inline tag")
	}
	for _, c := range res.Citations {
		if c.RelPath != gameFile {
			t.Errorf("citation %q is not the in-context document %q", c.RelPath, gameFile)
		}
	}
}

// TestAsk1083_RightScriptMakesOneCall is the no-retry path, the common case.
func TestAsk1083_RightScriptMakesOneCall(t *testing.T) {
	gen := &scriptedGenerator{t: t, outs: []string{latinAnswer1083}}
	res := ask1083(t, gen, "")
	if len(gen.prompts) != 1 {
		t.Fatalf("generator calls = %d, want 1", len(gen.prompts))
	}
	if strings.Contains(gen.prompts[0], "Correction:") {
		t.Fatal("the first prompt carries a correction")
	}
	if !strings.HasPrefix(res.Answer, "Riley Park homered") {
		t.Fatalf("answer = %q", res.Answer)
	}
}

// TestAsk1083_AQuotedForeignNameIsNotAMismatch: an English answer that quotes a
// Cyrillic name is still a Latin answer. No retry.
func TestAsk1083_AQuotedForeignNameIsNotAMismatch(t *testing.T) {
	gen := &scriptedGenerator{t: t, outs: []string{
		"Riley Park homered in the sixth; the caption read \"Хоумран\". [" + gameFile + "]",
	}}
	ask1083(t, gen, "")
	if len(gen.prompts) != 1 {
		t.Fatalf("generator calls = %d, want 1", len(gen.prompts))
	}
}

// TestAsk1083_SecondAnswerStillWrongKeepsTheFirst: the retry must earn its
// place. A second answer that misses too does not replace the first, and there
// is no third call (scriptedGenerator fails the test on one).
func TestAsk1083_SecondAnswerStillWrongKeepsTheFirst(t *testing.T) {
	second := "第二个答案也是中文。[" + gameFile + "]"
	gen := &scriptedGenerator{t: t, outs: []string{hanAnswer1083, second}}
	res := ask1083(t, gen, "")
	if len(gen.prompts) != 2 {
		t.Fatalf("generator calls = %d, want 2", len(gen.prompts))
	}
	if !strings.Contains(res.Answer, "16世纪") {
		t.Fatalf("answer = %q, want the first answer kept", res.Answer)
	}
}

// TestAsk1083_SecondCallFailureKeepsTheFirst: a failed correction does not turn
// a generated answer into a retrieval-only one (§9.4.5).
func TestAsk1083_SecondCallFailureKeepsTheFirst(t *testing.T) {
	gen := &scriptedGenerator{
		t:    t,
		outs: []string{hanAnswer1083, ""},
		errs: []error{nil, errors.New("provider down")},
	}
	res := ask1083(t, gen, "")
	if len(gen.prompts) != 2 {
		t.Fatalf("generator calls = %d, want 2", len(gen.prompts))
	}
	if !strings.Contains(res.Answer, "16世纪") {
		t.Fatalf("answer = %q, want the first answer kept", res.Answer)
	}
	if res.AnswerSource != "" || res.AnswerSourceReason != "" {
		t.Fatalf("answer_source = %q/%q, want generated", res.AnswerSource, res.AnswerSourceReason)
	}
}

// TestAsk1083_SecondAnswerWithNoScriptKeepsTheFirst: absence of a verdict is not
// a pass. A second answer too short to have a dominant script does not replace
// the first.
func TestAsk1083_SecondAnswerWithNoScriptKeepsTheFirst(t *testing.T) {
	gen := &scriptedGenerator{t: t, outs: []string{hanAnswer1083, "OK. [" + gameFile + "]"}}
	res := ask1083(t, gen, "")
	if !strings.Contains(res.Answer, "16世纪") {
		t.Fatalf("answer = %q, want the first answer kept", res.Answer)
	}
}

// TestAsk1083_OperatorFixedLanguageIsNotCorrected: an operator who fixed one
// answer language replaced the rule. The check follows the rule, as the #892
// reminder does, so a Han answer is published as the operator asked.
func TestAsk1083_OperatorFixedLanguageIsNotCorrected(t *testing.T) {
	gen := &scriptedGenerator{t: t, outs: []string{hanAnswer1083}}
	res := ask1083(t, gen,
		"Answer from the provided context only. Always answer in Chinese, whatever "+
			"language the question uses. Cite sources as [rel_path].")
	if len(gen.prompts) != 1 {
		t.Fatalf("generator calls = %d, want 1 (the operator fixed the language)", len(gen.prompts))
	}
	if !strings.Contains(res.Answer, "16世纪") {
		t.Fatalf("answer = %q", res.Answer)
	}
}

// TestScript1083_Mismatch pins the unit: which pairs are a mismatch.
func TestScript1083_Mismatch(t *testing.T) {
	cases := []struct {
		name, question, answer string
		want                   bool
	}{
		{"han answer to latin question", "What is the 16th century known as the start of?", hanAnswer1083, true},
		{"cyrillic answer to latin question", "What is said about Crimea in these recordings?",
			"В записях говорится о Крыме и его истории. [a.mp4]", true},
		{"latin answer to latin question", "What is the 16th century known as the start of?", latinAnswer1083, false},
		{"a long latin tag does not vote", "What is the 16th century known as the start of?",
			"16世纪被称为宗教战争的开始。[Black_Death_and_the_sixteenth_century_overview.md:L37-L43]", true},
		// Japanese writes Han and kana together, so a Japanese question can be
		// Han-dominant while its correct answer is kana-dominant. Han, Hiragana
		// and Katakana are one writing system (SPEC §9.4.6), so this is not a
		// mismatch. The cost is a known limit: a Chinese question answered in
		// kana-heavy Japanese is not caught either. The check is a script check,
		// not a language check.
		{"kana answer to a han-dominant japanese question", "東京都内の主要美術館一覧を教えて",
			"とうきょうのびじゅつかんはたくさんあります", false},
		{"cyrillic answer to a han question", "這些錄音中關於克里米亞說了什麼？",
			"В записях говорится о Крыме и его истории.", true},
		{"a markdown link label is answer text, its URL is not", "What is the 16th century known as the start of?",
			"[这是一个完整的中文回答](https://example.com/a/long/latin/path)", true},
		{"a link destination with parentheses and a title does not vote", "What is the 16th century known as the start of?",
			"[这是一个完整的中文回答](https://en.example.org/wiki/Religious_wars_(France) \"French Wars of Religion overview\")", true},
		{"an angle-bracket link destination does not vote", "What is the 16th century known as the start of?",
			"[这是一个完整的中文回答](<https://example.com/a long latin path with spaces>)", true},
		{"a time-span citation tag does not vote", "What is the 16th century known as the start of?",
			"宗教战争开始的时期。[broadcast_recording_of_the_lecture.mp4@t=00:01:00-00:01:08]", true},
		// Han and kana vote together BEFORE the share rule. Split, neither
		// reaches 60% here, so a per-script count would see no verdict.
		{"balanced han and kana answer to a latin question", "What is the 16th century known as the start of?",
			"十六世紀はフランスのしゅうきょうせんそうの時代です", true},
		// The prompt lets a quote keep its own spelling. An English answer that
		// quotes a long Cyrillic passage is an English answer.
		{"a long quoted passage does not vote", "What is said about Crimea in these recordings?",
			"The host says: «В записях говорится о Крыме и его долгой и сложной истории» [a.mp4]", false},
		{"curly quotes do not vote", "What is said about Crimea in these recordings?",
			"The host says “В записях говорится о Крыме и его долгой истории”. [a.mp4]", false},
		{"a quote across lines does not vote", "What is said about Crimea in these recordings?",
			"The host says: «В записях говорится о Крыме\nи его долгой и очень сложной истории» [a.mp4]", false},
		{"a markdown block quote does not vote", "What is said about Crimea in these recordings?",
			"The host says:\n> В записях говорится о Крыме\n> и его долгой и очень сложной истории\n\nSo Crimea is discussed. [a.mp4]", false},
		// Code is in the script of the programming language. A Russian answer
		// about a code corpus with a long Latin code block is a Russian answer.
		{"a fenced code block does not vote", "Как сервер проверяет заголовок авторизации?",
			"Сервер сравнивает токен с ожидаемым значением:\n```go\nif subtle.ConstantTimeCompare([]byte(header), []byte(expectedToken)) != 1 {\n\tlog.Printf(\"unauthorized request from remote address %s\", remoteAddr)\n\treturn errUnauthorized\n}\n```", false},
		{"a bare url does not vote", "Где находится документация по установке сервера?",
			"Ответ находится здесь: https://example.com/this-is-a-long-english-only-documentation-path", false},
		{"an autolink does not vote", "Где находится документация по установке сервера?",
			"Ответ находится здесь: <https://example.com/this-is-a-long-english-only-documentation-path>", false},
		{"inline code does not vote", "Как сервер проверяет заголовок авторизации?",
			"Сервер вызывает `subtle.ConstantTimeCompare(headerValue, expectedTokenValue)` для токена.", false},
		{"short answer has no verdict", "What is the 16th century known as the start of?", "是。", false},
		{"question with no clear script", "Крым Crimea Крым Crimea", hanAnswer1083, false},
	}
	for _, c := range cases {
		if got := retrieval.AnswerScriptMismatch(c.question, c.answer); got != c.want {
			t.Errorf("%s: mismatch = %v, want %v", c.name, got, c.want)
		}
	}
}

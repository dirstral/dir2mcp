package tests

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §8.6.13 reconcile pass (spec 0.88.0 and 0.89.0, dir2mcp #1143). A
// transcript stored before the low-confidence rule records no redecode_rule.
// Armed, the pass finds suspect windows from the stored text alone. Before it
// decodes a suspect again with the item language, it probes the window: one
// centred slice of at most media.stt.language_probe_sec (30 s here), decoded
// with no hint. Only a probe that reports the item language at or above
// media.stt.redecode_below lets the window be decoded again.

// rdRussian is a Russian window transcript long enough for the §8.8 detector.
var rdRussian = "[00:00] Правительство объявило новые правила для школ, и учителя вместе с родителями обсуждают, что изменится осенью.\n" +
	"[00:30] Синоптики обещают тёплую погоду на выходных, поэтому многие семьи собираются поехать к морю или в горы."

// rdProbeCut is the probe slice of window 1 [0,600000): 30 s centred on 300 s.
var rdProbeCut = sttWindowCut{startMS: 285_000, endMS: 315_000}

// storeUnmarked indexes a recording with the rule off, so the cache holds a
// transcript that records no redecode_rule. It returns the shared state dir.
func storeUnmarked(t *testing.T, relPath string, script map[int]rdReply, want string) string {
	t.Helper()
	stateDir := testutil.TempDir(t)
	first := newRedecodeHarnessIn(t, stateDir, &rdTranscriber{script: script}, 0)
	m := first.run(t, relPath)
	if m.RedecodeRule != "" || !strings.Contains(first.chunkText(), want) {
		t.Fatalf("setup: want an unmarked transcript holding %q, got rule %q:\n%s", want, m.RedecodeRule, first.chunkText())
	}
	return stateDir
}

// storeUnmarkedTranscript stores the #1143 recording: window 1 English, the
// item Ukrainian.
func storeUnmarkedTranscript(t *testing.T, relPath string) string {
	t.Helper()
	return storeUnmarked(t, relPath, jingleScript(), "Good evening")
}

// armedRun runs the armed pass over the stored transcript with the probe
// answer probe, and returns the harness and the meta.
func armedRun(t *testing.T, stateDir, relPath string, tr *rdTranscriber) (*rdHarness, redecodeMeta) {
	t.Helper()
	h := newRedecodeHarnessIn(t, stateDir, tr, 0.5)
	h.svc.SetReconcileLanguageWindows(true)
	return h, h.run(t, relPath)
}

// assertProbedOnce checks that exactly one unhinted decode ran, on the
// centred slice of window 1.
func assertProbedOnce(t *testing.T, h *rdHarness, tr *rdTranscriber) {
	t.Helper()
	if n := tr.callCount(); n != 1 {
		t.Errorf("unhinted decodes = %d, want exactly 1 (the probe)", n)
	}
	for _, c := range h.cutList() {
		if c == rdProbeCut {
			return
		}
	}
	t.Errorf("no probe cut %+v among %+v", rdProbeCut, h.cutList())
}

// TestReconcileRedecode_ProbeConfirmsTheJingleWindow: the stored text of
// window 1 is English, the item is Ukrainian, and the probe reports uk at
// 0.95. Only window 1 is decoded again, with uk, and the record names the
// probe.
func TestReconcileRedecode_ProbeConfirmsTheJingleWindow(t *testing.T) {
	t.Parallel()
	stateDir := storeUnmarkedTranscript(t, "news/stored.mp3")
	tr := &rdTranscriber{script: map[int]rdReply{1: {lang: "uk", conf: 0.95, text: rdUkHintText}}, hintText: rdUkHintText}
	h, m := armedRun(t, stateDir, "news/stored.mp3", tr)

	assertProbedOnce(t, h, tr)
	if got := tr.hintCalls(); len(got) != 1 || got[0] != "uk" {
		t.Fatalf("hinted decodes = %v, want exactly one in uk\nlogs:\n%s", got, h.logs.String())
	}
	if m.RedecodeRule != "low-confidence" || len(m.RedecodedWindows) != 1 {
		t.Fatalf("redecode_rule=%q redecoded_windows=%+v, want low-confidence and one entry", m.RedecodeRule, m.RedecodedWindows)
	}
	w := m.RedecodedWindows[0]
	if w.StartMS != 0 || w.EndMS != rdWin1EndMS || w.FirstLanguage != "uk" || w.FirstConfidence != 0.95 || w.Language != "uk" || !w.Replaced {
		t.Errorf("redecoded window = %+v, want [0,%d) probe uk@0.95 -> uk replaced", w, rdWin1EndMS)
	}
	text := h.chunkText()
	if !strings.Contains(text, "Доброго вечора") || strings.Contains(text, "Good evening") {
		t.Errorf("window 1 was not replaced in the index:\n%s", text)
	}
	if !strings.Contains(text, "Синоптики обіцяють") {
		t.Errorf("windows 2 and 3 were lost:\n%s", text)
	}
	assertReconcileIsDone(t, stateDir, tr)
}

// assertReconcileIsDone runs the pass once more on the same cache: the record
// is there now, so the run decodes nothing and keeps the record.
func assertReconcileIsDone(t *testing.T, stateDir string, tr *rdTranscriber) {
	t.Helper()
	calls, hints := tr.callCount(), len(tr.hintCalls())
	again := newRedecodeHarnessIn(t, stateDir, tr, 0.5)
	again.svc.SetReconcileLanguageWindows(true)
	m := again.run(t, "news/stored.mp3")
	if tr.callCount() != calls || len(tr.hintCalls()) != hints {
		t.Errorf("a marked transcript was decoded again: calls %d -> %d, hints %d -> %d", calls, tr.callCount(), hints, len(tr.hintCalls()))
	}
	if m.RedecodeRule != "low-confidence" {
		t.Errorf("the cache lost the record: rule %q windows %+v", m.RedecodeRule, m.RedecodedWindows)
	}
}

// TestReconcileRedecode_RealCodeSwitchIsLeftAlone: a Russian item whose
// window 1 holds a real Ukrainian answer. The probe reports uk at 0.97, so the
// window is not forced to Russian. The transcript is unchanged and still
// records redecode_rule, so the next armed run skips it.
func TestReconcileRedecode_RealCodeSwitchIsLeftAlone(t *testing.T) {
	t.Parallel()
	script := map[int]rdReply{
		1: {lang: "uk", conf: 0.97, text: rdUkrainian},
		2: {lang: "ru", conf: 0.99, text: rdRussian},
		3: {lang: "ru", conf: 0.98, text: rdRussian},
	}
	stateDir := storeUnmarked(t, "news/stored.mp3", script, "Синоптики обіцяють")
	tr := &rdTranscriber{script: map[int]rdReply{1: {lang: "uk", conf: 0.97, text: rdUkrainian}}, hintText: rdUkHintText}
	h, m := armedRun(t, stateDir, "news/stored.mp3", tr)

	assertProbedOnce(t, h, tr)
	if got := tr.hintCalls(); len(got) != 0 {
		t.Fatalf("hinted decodes = %v, want none: a real Ukrainian passage must not be forced to Russian", got)
	}
	if m.Language != "ru" {
		t.Errorf("item language = %q, want ru", m.Language)
	}
	if m.RedecodeRule != "low-confidence" || len(m.RedecodedWindows) != 0 {
		t.Errorf("redecode_rule=%q redecoded_windows=%+v, want low-confidence and no list", m.RedecodeRule, m.RedecodedWindows)
	}
	if !strings.Contains(h.chunkText(), "Синоптики обіцяють") {
		t.Errorf("the Ukrainian window changed:\n%s", h.chunkText())
	}
	assertReconcileIsDone(t, stateDir, tr)
}

// TestReconcileRedecode_ProbeBelowThresholdLeavesTheWindow: the probe reports
// the item language below redecode_below, so the window stays as recorded.
func TestReconcileRedecode_ProbeBelowThresholdLeavesTheWindow(t *testing.T) {
	t.Parallel()
	stateDir := storeUnmarkedTranscript(t, "news/stored.mp3")
	tr := &rdTranscriber{script: map[int]rdReply{1: {lang: "uk", conf: 0.3, text: rdUkHintText}}, hintText: rdUkHintText}
	h, m := armedRun(t, stateDir, "news/stored.mp3", tr)

	assertProbedOnce(t, h, tr)
	if got := tr.hintCalls(); len(got) != 0 {
		t.Fatalf("hinted decodes = %v, want none", got)
	}
	if m.RedecodeRule != "low-confidence" || len(m.RedecodedWindows) != 0 {
		t.Errorf("redecode_rule=%q redecoded_windows=%+v, want low-confidence and no list", m.RedecodeRule, m.RedecodedWindows)
	}
	if !strings.Contains(h.chunkText(), "Good evening") {
		t.Errorf("window 1 changed:\n%s", h.chunkText())
	}
}

// TestReconcileRedecode_ProbeFailureLeavesTheWindowUnchecked: a probe that
// fails leaves the window as recorded and records no redecode_rule, so the
// next armed run checks the window again.
func TestReconcileRedecode_ProbeFailureLeavesTheWindowUnchecked(t *testing.T) {
	t.Parallel()
	stateDir := storeUnmarkedTranscript(t, "news/stored.mp3")
	tr := &rdTranscriber{script: map[int]rdReply{1: {err: errors.New("probe refused")}}, hintText: rdUkHintText}
	h, m := armedRun(t, stateDir, "news/stored.mp3", tr)

	assertProbedOnce(t, h, tr)
	if got := tr.hintCalls(); len(got) != 0 {
		t.Fatalf("hinted decodes = %v, want none", got)
	}
	if m.RedecodeRule != "" || len(m.RedecodedWindows) != 0 {
		t.Errorf("redecode_rule=%q redecoded_windows=%+v, want neither after a failed probe", m.RedecodeRule, m.RedecodedWindows)
	}
	if !strings.Contains(h.chunkText(), "Good evening") {
		t.Errorf("window 1 changed:\n%s", h.chunkText())
	}
	if !strings.Contains(h.logs.String(), "the language probe failed") {
		t.Errorf("the failed probe was not logged:\n%s", h.logs.String())
	}
}

// TestReconcileRedecode_NotArmedServesAsRecorded: without the flag the stored
// transcript is served as recorded and nothing is decoded.
func TestReconcileRedecode_NotArmedServesAsRecorded(t *testing.T) {
	t.Parallel()
	stateDir := storeUnmarkedTranscript(t, "news/unarmed.mp3")
	tr := &rdTranscriber{hintText: rdUkHintText}
	h := newRedecodeHarnessIn(t, stateDir, tr, 0.5)
	m := h.run(t, "news/unarmed.mp3")
	if len(tr.hintCalls()) != 0 || tr.callCount() != 0 {
		t.Fatalf("decodes happened without the flag: hints %v, calls %d", tr.hintCalls(), tr.callCount())
	}
	if m.RedecodeRule != "" || !strings.Contains(h.chunkText(), "Good evening") {
		t.Errorf("the stored transcript changed without the flag: rule %q\n%s", m.RedecodeRule, h.chunkText())
	}
}

// TestReconcileRedecode_NoForeignWindowRecordsTheCheck: a stored transcript
// whose windows are all in the item language is marked as checked, with no
// probe and no decode.
func TestReconcileRedecode_NoForeignWindowRecordsTheCheck(t *testing.T) {
	t.Parallel()
	script := jingleScript()
	script[1] = rdReply{lang: "uk", conf: 0.9, text: rdUkrainian}
	stateDir := storeUnmarked(t, "news/clean.mp3", script, "Синоптики обіцяють")

	tr := &rdTranscriber{hintText: rdUkHintText}
	_, m := armedRun(t, stateDir, "news/clean.mp3", tr)
	if len(tr.hintCalls()) != 0 || tr.callCount() != 0 {
		t.Fatalf("decodes happened on a clean transcript: hints %v, calls %d", tr.hintCalls(), tr.callCount())
	}
	if m.RedecodeRule != "low-confidence" || len(m.RedecodedWindows) != 0 {
		t.Errorf("redecode_rule=%q redecoded_windows=%+v, want low-confidence and no list", m.RedecodeRule, m.RedecodedWindows)
	}
}

// TestReconcileRedecode_APartialRunKeepsItsRecord: the first armed run
// replaces window 1 and its probe of window 3 fails, so it records window 1
// and no rule. The next armed run checks only window 3 and keeps the record of
// window 1 beside the new one.
func TestReconcileRedecode_APartialRunKeepsItsRecord(t *testing.T) {
	t.Parallel()
	longUk := rdUkrainian + "\n" + strings.ReplaceAll(rdUkrainian, "[00:", "[01:") + "\n" + strings.ReplaceAll(rdUkrainian, "[00:", "[02:")
	script := map[int]rdReply{
		1: {lang: "en", conf: 0.34, text: rdEnglish},
		2: {lang: "uk", conf: 0.99, text: longUk},
		3: {lang: "ru", conf: 0.4, text: rdRussian},
	}
	stateDir := storeUnmarked(t, "news/stored.mp3", script, "Good evening")

	first := &rdTranscriber{script: map[int]rdReply{
		1: {lang: "uk", conf: 0.95},
		2: {err: errors.New("probe refused")},
	}, hintText: rdUkHintText}
	_, m1 := armedRun(t, stateDir, "news/stored.mp3", first)
	if m1.Language != "uk" {
		t.Fatalf("setup: item language = %q, want uk", m1.Language)
	}
	if m1.RedecodeRule != "" || len(m1.RedecodedWindows) != 1 || m1.RedecodedWindows[0].StartMS != 0 {
		t.Fatalf("first run: rule %q windows %+v, want no rule and window 1 only", m1.RedecodeRule, m1.RedecodedWindows)
	}

	second := &rdTranscriber{script: map[int]rdReply{1: {lang: "uk", conf: 0.9}}, hintText: rdUkHintText}
	_, m2 := armedRun(t, stateDir, "news/stored.mp3", second)
	if second.callCount() != 1 || len(second.hintCalls()) != 1 {
		t.Errorf("second run: calls %d hints %v, want one probe and one hinted decode", second.callCount(), second.hintCalls())
	}
	if m2.RedecodeRule != "low-confidence" || len(m2.RedecodedWindows) != 2 {
		t.Fatalf("second run: rule %q windows %+v, want low-confidence and both windows", m2.RedecodeRule, m2.RedecodedWindows)
	}
	if m2.RedecodedWindows[0].StartMS != 0 || m2.RedecodedWindows[1].StartMS != 1_180_000 {
		t.Errorf("record = %+v, want window 1 then window 3 in start order", m2.RedecodedWindows)
	}
}

// TestReconcileRedecode_TailOnlySecondDecodeKeepsTheCore: the second decode of
// window 1 holds one segment only, at 595 s. That is in the overlap tail of the
// window (its core ends where window 2 starts, at 590 s), so no new segment
// lands in the core. The stored core stays, and the record says the window was
// not replaced.
func TestReconcileRedecode_TailOnlySecondDecodeKeepsTheCore(t *testing.T) {
	t.Parallel()
	stateDir := storeUnmarkedTranscript(t, "news/stored.mp3")
	tail := "[09:55] Наші кореспонденти розповідають про події в містах і селах, про людей, які працюють для своєї громади."
	tr := &rdTranscriber{script: map[int]rdReply{1: {lang: "uk", conf: 0.95}}, hintText: tail}
	h, m := armedRun(t, stateDir, "news/stored.mp3", tr)

	if got := tr.hintCalls(); len(got) != 1 || got[0] != "uk" {
		t.Fatalf("hinted decodes = %v, want exactly one in uk\nlogs:\n%s", got, h.logs.String())
	}
	if m.RedecodeRule != "low-confidence" || len(m.RedecodedWindows) != 1 {
		t.Fatalf("redecode_rule=%q redecoded_windows=%+v, want low-confidence and one entry", m.RedecodeRule, m.RedecodedWindows)
	}
	if w := m.RedecodedWindows[0]; w.StartMS != 0 || w.Replaced {
		t.Errorf("redecoded window = %+v, want window 1 not replaced", w)
	}
	text := h.chunkText()
	if !strings.Contains(text, "Good evening") || !strings.Contains(text, "Our reporters") {
		t.Errorf("the stored core of window 1 was lost:\n%s", text)
	}
}

// TestReconcileRedecode_SameTextNewWordsRewritesTheCache: the second decode of
// window 1 has the stored text, but it brings word timings the stored
// transcript does not have. The pass writes the whole cache entry, so the new
// word timings are kept beside the record that names the window replaced.
func TestReconcileRedecode_SameTextNewWordsRewritesTheCache(t *testing.T) {
	t.Parallel()
	stateDir := storeUnmarkedTranscript(t, "news/stored.mp3")
	if n := len(wordsSidecars(t, stateDir)); n != 0 {
		t.Fatalf("setup: %d word-timing sidecars before the pass, want none", n)
	}
	words := []model.TimedWord{{Word: "Good", StartMS: 0, EndMS: 400}, {Word: "evening", StartMS: 400, EndMS: 900}}
	tr := &rdTranscriber{script: map[int]rdReply{1: {lang: "uk", conf: 0.95}}, hintText: rdEnglish, hintWords: words}
	h, m := armedRun(t, stateDir, "news/stored.mp3", tr)

	if m.RedecodeRule != "low-confidence" || len(m.RedecodedWindows) != 1 || !m.RedecodedWindows[0].Replaced {
		t.Fatalf("redecode_rule=%q redecoded_windows=%+v, want low-confidence and window 1 replaced\nlogs:\n%s", m.RedecodeRule, m.RedecodedWindows, h.logs.String())
	}
	sidecars := wordsSidecars(t, stateDir)
	if len(sidecars) != 1 {
		t.Fatalf("word-timing sidecars = %v, want one: the new word timings were not written", sidecars)
	}
	raw, err := os.ReadFile(sidecars[0])
	if err != nil {
		t.Fatalf("read %s: %v", sidecars[0], err)
	}
	if !strings.Contains(string(raw), "evening") {
		t.Errorf("the word-timing sidecar does not hold the new words:\n%s", raw)
	}
}

// wordsSidecars lists the transcript word-timing sidecars under stateDir.
func wordsSidecars(t *testing.T, stateDir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(stateDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".words.json") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", stateDir, err)
	}
	return out
}

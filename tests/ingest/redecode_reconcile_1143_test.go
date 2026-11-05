package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §8.6.13 reconcile pass (spec 0.88.0, dir2mcp #1143). A transcript
// stored before the low-confidence rule records no redecode_rule. Armed, the
// pass finds candidate windows from the stored text alone and decodes again
// only those windows, with the item language.

// storeUnmarkedTranscript indexes the #1143 recording with the rule off, so
// the cache holds a transcript whose window 1 is English and which records no
// redecode_rule. It returns the shared state dir.
func storeUnmarkedTranscript(t *testing.T, relPath string) string {
	t.Helper()
	stateDir := testutil.TempDir(t)
	first := newRedecodeHarnessIn(t, stateDir, &rdTranscriber{script: jingleScript()}, 0)
	m := first.run(t, relPath)
	if m.RedecodeRule != "" || !strings.Contains(first.chunkText(), "Good evening") {
		t.Fatalf("setup: want an unmarked transcript with English window 1, got rule %q:\n%s", m.RedecodeRule, first.chunkText())
	}
	return stateDir
}

// TestReconcileRedecode_OnlyTheForeignWindowIsDecodedAgain: the stored text
// of window 1 is English and the item is Ukrainian, so only window 1 is
// decoded again, and only with uk.
func TestReconcileRedecode_OnlyTheForeignWindowIsDecodedAgain(t *testing.T) {
	t.Parallel()
	stateDir := storeUnmarkedTranscript(t, "news/stored.mp3")
	tr := &rdTranscriber{hintText: rdUkHintText}
	h := newRedecodeHarnessIn(t, stateDir, tr, 0.5)
	h.svc.SetReconcileLanguageWindows(true)
	m := h.run(t, "news/stored.mp3")

	if got := tr.hintCalls(); len(got) != 1 || got[0] != "uk" {
		t.Fatalf("hinted decodes = %v, want exactly one in uk\nlogs:\n%s", got, h.logs.String())
	}
	if n := tr.callCount(); n != 0 {
		t.Errorf("unhinted decodes = %d, want 0: the pass must not decode the item again", n)
	}
	if m.RedecodeRule != "low-confidence" || len(m.RedecodedWindows) != 1 {
		t.Fatalf("redecode_rule=%q redecoded_windows=%+v, want low-confidence and one entry", m.RedecodeRule, m.RedecodedWindows)
	}
	w := m.RedecodedWindows[0]
	if w.StartMS != 0 || w.EndMS != rdWin1EndMS || w.FirstLanguage != "en" || w.Language != "uk" || !w.Replaced {
		t.Errorf("redecoded window = %+v, want [0,%d) en -> uk replaced", w, rdWin1EndMS)
	}
	text := h.chunkText()
	if !strings.Contains(text, "Доброго вечора") || strings.Contains(text, "Good evening") {
		t.Errorf("window 1 was not replaced in the index:\n%s", text)
	}
	if !strings.Contains(text, "Синоптики") {
		t.Errorf("windows 2 and 3 were lost:\n%s", text)
	}

	// The record is in the cache now, so the next armed run checks nothing.
	again := newRedecodeHarnessIn(t, stateDir, tr, 0.5)
	again.svc.SetReconcileLanguageWindows(true)
	m2 := again.run(t, "news/stored.mp3")
	if got := tr.hintCalls(); len(got) != 1 {
		t.Errorf("a marked transcript was decoded again: hints %v", got)
	}
	if m2.RedecodeRule != "low-confidence" || len(m2.RedecodedWindows) != 1 {
		t.Errorf("the cache lost the record: rule %q windows %+v", m2.RedecodeRule, m2.RedecodedWindows)
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
// decode.
func TestReconcileRedecode_NoForeignWindowRecordsTheCheck(t *testing.T) {
	t.Parallel()
	stateDir := testutil.TempDir(t)
	script := jingleScript()
	script[1] = rdReply{lang: "uk", conf: 0.9, text: rdUkrainian}
	first := newRedecodeHarnessIn(t, stateDir, &rdTranscriber{script: script}, 0)
	first.run(t, "news/clean.mp3")

	tr := &rdTranscriber{hintText: rdUkHintText}
	h := newRedecodeHarnessIn(t, stateDir, tr, 0.5)
	h.svc.SetReconcileLanguageWindows(true)
	m := h.run(t, "news/clean.mp3")
	if len(tr.hintCalls()) != 0 || tr.callCount() != 0 {
		t.Fatalf("decodes happened on a clean transcript: hints %v, calls %d", tr.hintCalls(), tr.callCount())
	}
	if m.RedecodeRule != "low-confidence" || len(m.RedecodedWindows) != 0 {
		t.Errorf("redecode_rule=%q redecoded_windows=%+v, want low-confidence and no list", m.RedecodeRule, m.RedecodedWindows)
	}
}

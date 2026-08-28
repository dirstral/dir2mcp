package tests

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// Cross-line translation context tests (issue #573): the chat translate engine
// batches consecutive cues in a window (with a read-only margin) so the model can
// resolve referents/agreement/split-sentences/terminology across cues, while a
// strict numbered 1:1 request/response contract guarantees exactly one output cue
// per input cue — a malformed response safe-degrades to per-line rather than
// desyncing subtitle timing.

// batchTargetLineRE matches a numbered target line ("N: <cue>") inside a windowed
// translate prompt's "Lines to translate:" section.
var batchTargetLineRE = regexp.MustCompile(`^(\d+):\s?(.*)$`)

// contextAwareTranslator honours the windowed numbered 1:1 contract — it returns
// exactly one translated line per numbered target and never translates the
// read-only context margin — and records every prompt it received so the presence
// of cross-line context can be asserted black-box. Per-line (opt-out / fallback)
// prompts are translated as a single tagged line.
type contextAwareTranslator struct {
	mu         sync.Mutex
	prompts    []string
	batchCalls int
	lineCalls  int
}

func (f *contextAwareTranslator) Generate(_ context.Context, prompt string) (string, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, prompt)
	isBatch := strings.Contains(prompt, "Lines to translate:")
	if isBatch {
		f.batchCalls++
	} else {
		f.lineCalls++
	}
	f.mu.Unlock()

	if !isBatch {
		// The source sits inside the untrusted-data fence (#888); the prompt no
		// longer ends with it, because the instruction is restated after.
		return "T[" + fencedPayload(prompt) + "]", nil
	}
	// Echo exactly the numbered targets, translated; stop at the context margin.
	var out []string
	inTargets := false
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(line, "Lines to translate:") {
			inTargets = true
			continue
		}
		if !inTargets {
			continue
		}
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "Context") {
			break // reached the trailing context margin / end of targets
		}
		if m := batchTargetLineRE.FindStringSubmatch(line); m != nil {
			out = append(out, m[1]+": T["+m[2]+"]")
		}
	}
	return strings.Join(out, "\n"), nil
}

func (f *contextAwareTranslator) snapshot() (prompts []string, batchCalls, lineCalls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.prompts...), f.batchCalls, f.lineCalls
}

// malformedBatchTranslator returns an UNPARSEABLE blob for a windowed batch (no
// numbering) so the safe-degrade path is exercised, while translating a per-line
// (fallback) prompt normally. It counts each path.
type malformedBatchTranslator struct {
	mu         sync.Mutex
	batchCalls int
	lineCalls  int
}

func (f *malformedBatchTranslator) Generate(_ context.Context, prompt string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Contains(prompt, "Lines to translate:") {
		f.batchCalls++
		return "here is the whole translation as one blob with no numbering at all", nil
	}
	f.lineCalls++
	// Per-line fallback: the source is inside the fence (#888).
	return "T[" + fencedPayload(prompt) + "]", nil
}

func (f *malformedBatchTranslator) counts() (batch, line int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.batchCalls, f.lineCalls
}

// readTranslateCache reads the single translated-transcript cache file written
// under <stateDir>/cache/translate. That file is the verbatim translated
// transcript (markers + text), so it is the cleanest black-box surface for
// asserting the 1:1 marker mapping.
func readTranslateCache(t *testing.T, stateDir string) string {
	t.Helper()
	dir := filepath.Join(stateDir, "cache", "translate")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read translate cache dir: %v", err)
	}
	var found []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".txt") {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("read translate cache file %s: %v", e.Name(), err)
			}
			found = append(found, string(b))
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one translate cache .txt, got %d in %s", len(found), dir)
	}
	return found[0]
}

// splitWindowPromptSections splits a windowed translate prompt into its
// context portion (everything outside the numbered target block) and the numbered
// target block itself.
func splitWindowPromptSections(prompt string) (ctx, targets string) {
	const marker = "Lines to translate:\n"
	i := strings.Index(prompt, marker)
	if i < 0 {
		return prompt, ""
	}
	rest := prompt[i+len(marker):]
	if j := strings.Index(rest, "\nContext after"); j >= 0 {
		return prompt[:i] + rest[j:], rest[:j]
	}
	return prompt[:i], rest
}

func countNumberedTargets(prompt string) int {
	_, targets := splitWindowPromptSections(prompt)
	n := 0
	for _, line := range strings.Split(targets, "\n") {
		if batchTargetLineRE.MatchString(line) {
			n++
		}
	}
	return n
}

// TestTranscriptTranslation_WindowedPreservesOneToOne is the anti-desync guard:
// an N-cue transcript translated in a single window yields exactly N output cues
// with the original [mm:ss] markers intact and in order.
func TestTranscriptTranslation_WindowedPreservesOneToOne(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir}, st)
	source := "[00:00] one\n[00:03] two\n[00:06] three\n[00:09] four\n[00:12] five"
	svc.SetTranscriber(&fakeTranscriber{text: source})
	svc.SetTranscriptLanguage("de")
	tr := &contextAwareTranslator{}
	svc.SetTranslator(tr, "mistral", "m", []string{"en"})

	doc := model.Document{DocID: 1, RelPath: "audio/a.mp3", DocType: "audio"}
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, []byte("audio")); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation: %v", err)
	}

	got := readTranslateCache(t, stateDir)
	want := "[00:00] T[one]\n[00:03] T[two]\n[00:06] T[three]\n[00:09] T[four]\n[00:12] T[five]"
	if got != want {
		t.Fatalf("windowed translation broke the 1:1 marker mapping:\n got: %q\nwant: %q", got, want)
	}
	// The default window (16) covers all 5 cues in one batch: exactly one batch
	// call, and no per-line fallback (the contract was satisfied).
	if _, batch, line := tr.snapshot(); batch != 1 || line != 0 {
		t.Fatalf("expected exactly one windowed batch call and no per-line fallback, got batch=%d line=%d", batch, line)
	}
}

// TestTranscriptTranslation_WindowedProvidesCrossLineContext proves the model is
// actually given cross-cue context: a windowed batch prompt carries more than one
// cue, and the surrounding-margin cue is supplied as READ-ONLY context (marked
// "do NOT translate or return"), not as a target.
func TestTranscriptTranslation_WindowedProvidesCrossLineContext(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{
		StateDir:                   stateDir,
		MediaTranslateWindowLines:  2,
		MediaTranslateContextLines: 1,
	}, st)
	source := "[00:00] alpha\n[00:03] bravo\n[00:06] charlie\n[00:09] delta"
	svc.SetTranscriber(&fakeTranscriber{text: source})
	svc.SetTranscriptLanguage("de")
	tr := &contextAwareTranslator{}
	svc.SetTranslator(tr, "mistral", "m", []string{"en"})

	doc := model.Document{DocID: 2, RelPath: "audio/b.mp3", DocType: "audio"}
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, []byte("audio")); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation: %v", err)
	}

	prompts, batch, _ := tr.snapshot()
	if batch == 0 {
		t.Fatalf("expected windowed batch calls, got none")
	}

	// (a) At least one window prompt carries more than one target cue — the model
	// sees neighbouring cues together, not one in isolation.
	sawMultiCueWindow := false
	for _, p := range prompts {
		if countNumberedTargets(p) >= 2 {
			sawMultiCueWindow = true
			break
		}
	}
	if !sawMultiCueWindow {
		t.Fatalf("no batch prompt contained >1 target cue (no cross-line context); prompts=%v", prompts)
	}

	// (b) The neighbour cue is read-only context, not a target: the charlie/delta
	// window must show 'bravo' in a "do NOT translate or return" context section
	// while 'charlie' is a numbered target and 'bravo' is NOT.
	sawContextOnlyMargin := false
	for _, p := range prompts {
		ctxSection, targetSection := splitWindowPromptSections(p)
		if strings.Contains(ctxSection, "bravo") &&
			strings.Contains(ctxSection, "do NOT translate or return") &&
			strings.Contains(targetSection, "charlie") &&
			!strings.Contains(targetSection, "bravo") {
			sawContextOnlyMargin = true
			break
		}
	}
	if !sawContextOnlyMargin {
		t.Fatalf("no prompt marked neighbour cue 'bravo' as read-only context for the charlie/delta window; prompts=%v", prompts)
	}

	// The 1:1 output is intact across BOTH windows.
	got := readTranslateCache(t, stateDir)
	want := "[00:00] T[alpha]\n[00:03] T[bravo]\n[00:06] T[charlie]\n[00:09] T[delta]"
	if got != want {
		t.Fatalf("windowed translation with a margin broke 1:1:\n got: %q\nwant: %q", got, want)
	}
}

// TestTranscriptTranslation_MalformedBatchFallsBackTo1to1 is the safe-degrade
// guard: when the model returns an unparseable batch response (wrong structure /
// line count), the window falls back to per-line translation so the result still
// has exactly one output cue per input cue with markers intact — never a desync.
func TestTranscriptTranslation_MalformedBatchFallsBackTo1to1(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir}, st)
	source := "[00:00] uno\n[00:03] dos\n[00:06] tres"
	svc.SetTranscriber(&fakeTranscriber{text: source})
	svc.SetTranscriptLanguage("de")
	tr := &malformedBatchTranslator{}
	svc.SetTranslator(tr, "mistral", "m", []string{"en"})

	doc := model.Document{DocID: 3, RelPath: "audio/c.mp3", DocType: "audio"}
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, []byte("audio")); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation: %v", err)
	}

	got := readTranslateCache(t, stateDir)
	want := "[00:00] T[uno]\n[00:03] T[dos]\n[00:06] T[tres]"
	if got != want {
		t.Fatalf("malformed batch response desynced the transcript:\n got: %q\nwant: %q", got, want)
	}

	batch, line := tr.counts()
	if batch < 1 {
		t.Fatalf("expected the windowed batch path to be attempted at least once, got %d", batch)
	}
	if line != 3 {
		t.Fatalf("expected per-line fallback for all 3 cues after the malformed batch, got %d per-line calls", line)
	}
	// The translated transcript must chunk into the same 3 time spans as the
	// source — the malformed response did not add/drop/merge a cue.
	if got := ingest.ChunkTranscriptByTime(want); len(got) != 3 {
		t.Fatalf("expected 3 time-aligned chunks from the fallback output, got %d", len(got))
	}
}

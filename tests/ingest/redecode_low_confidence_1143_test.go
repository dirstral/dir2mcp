package tests

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §8.6.13 (spec 0.88.0), dir2mcp #1143. Under language_scope: item, with
// no pin and no identifier hint, a window the decoder reported in another
// language than the item's, at a confidence below media.stt.redecode_below,
// is decoded once more with the item language, when another window reported
// the item language at or above that value.
//
// Every recording here is 27 minutes. The default 10-minute window schedules
// three windows that start at 0, 590 and 1180 s.

const (
	rdTotalMS    = 27 * 60 * 1000
	rdWin1EndMS  = 600_000
	rdUkHintText = "[00:00] Доброго вечора, шановні глядачі, сьогодні ми говоримо про новини з усієї країни та про погоду на завтра.\n" +
		"[00:30] Наші кореспонденти розповідають про події в містах і селах, про людей, які працюють для своєї громади."
)

// rdEnglish and rdUkrainian are window transcripts long enough for the §8.8
// detector to name their language.
var (
	rdEnglish = "[00:00] Good evening and welcome to the program, tonight we bring you the news from across the whole country.\n" +
		"[00:30] Our reporters talk to people in the cities and in the villages about the work they do for their community."
	rdUkrainian = "[00:00] Уряд оголосив нові правила для шкіл, а вчителі та батьки обговорюють, як зміниться навчання восени.\n" +
		"[00:30] Синоптики обіцяють теплу погоду на вихідних, тому багато родин планують поїхати до моря або в гори."
)

// rdReply is one scripted first-decode answer.
type rdReply struct {
	lang string
	conf float64
	text string
}

// rdTranscriber is a fake STT provider with the optional language-hint
// capability. Unhinted calls answer from script by call ordinal (1-based).
// A hinted call (through ForLanguage) records the hint and answers hintText,
// which may be empty to model a second decode with no content.
type rdTranscriber struct {
	mu       sync.Mutex
	calls    int
	script   map[int]rdReply
	hints    []string
	hintText string
}

func (r *rdTranscriber) Transcribe(ctx context.Context, relPath string, data []byte) (string, error) {
	res, err := r.TranscribeStructured(ctx, relPath, data)
	return res.Text, err
}

func (r *rdTranscriber) TranscribeStructured(context.Context, string, []byte) (model.TranscriptResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	rep := r.script[r.calls]
	return model.TranscriptResult{Text: rep.text, Language: rep.lang, LanguageConfidence: rep.conf}, nil
}

func (r *rdTranscriber) MaxTranscribePayloadBytes() int { return 0 }

func (r *rdTranscriber) ForLanguage(lang string) model.Transcriber {
	return &rdHinted{parent: r, lang: lang}
}

func (r *rdTranscriber) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *rdTranscriber) hintCalls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hints...)
}

// rdHinted is the transcriber ForLanguage returns.
type rdHinted struct {
	parent *rdTranscriber
	lang   string
}

func (h *rdHinted) Transcribe(ctx context.Context, relPath string, data []byte) (string, error) {
	res, err := h.TranscribeStructured(ctx, relPath, data)
	return res.Text, err
}

func (h *rdHinted) TranscribeStructured(context.Context, string, []byte) (model.TranscriptResult, error) {
	h.parent.mu.Lock()
	defer h.parent.mu.Unlock()
	h.parent.hints = append(h.parent.hints, h.lang)
	return model.TranscriptResult{Text: h.parent.hintText, Language: h.lang, LanguageConfidence: 0.97}, nil
}

// jingleScript is the #1143 recording: window 1 reported en at 0.34, windows
// 2 and 3 reported uk at 0.99 and 1.00.
func jingleScript() map[int]rdReply {
	return map[int]rdReply{
		1: {lang: "en", conf: 0.34, text: rdEnglish},
		2: {lang: "uk", conf: 0.99, text: rdUkrainian},
		3: {lang: "uk", conf: 1.0, text: rdUkrainian},
	}
}

// redecodeMeta is the wire shape of the §8.6.13 re-decode fields.
type redecodeMeta struct {
	Language         string `json:"language"`
	RedecodeRule     string `json:"redecode_rule"`
	RedecodedWindows []struct {
		StartMS         int     `json:"start_ms"`
		EndMS           int     `json:"end_ms"`
		FirstLanguage   string  `json:"first_language"`
		FirstConfidence float64 `json:"first_confidence"`
		Language        string  `json:"language"`
		Replaced        bool    `json:"replaced"`
	} `json:"redecoded_windows"`
	Coverage *struct {
		WindowsAttempted int `json:"windows_attempted"`
	} `json:"coverage"`
}

type rdHarness struct {
	svc   *ingest.Service
	store *fakeIngestStore
	logs  *syncBuffer
}

// newRedecodeHarness builds a Service with language detection on and the
// given redecode_below, a stubbed duration probe and segment cut, and tr as
// the default transcriber.
func newRedecodeHarness(t *testing.T, tr model.Transcriber, below float64) *rdHarness {
	t.Helper()
	h := &rdHarness{store: &fakeIngestStore{}, logs: &syncBuffer{}}
	cfg := config.Config{StateDir: testutil.TempDir(t), LanguageDetectionEnabled: true, MediaSTTRedecodeBelow: below}
	h.svc = mustNewIngestService(t, cfg, h.store)
	h.svc.SetTranscriber(tr)
	h.svc.SetSTTIdentity("whisper", "large-v3")
	h.svc.SetLogger(log.New(h.logs, "", 0))
	h.svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) {
		return time.Duration(rdTotalMS) * time.Millisecond, nil
	}
	h.svc.ExtractSegmentFunc = func(_ context.Context, _ string, startMS, endMS int) ([]byte, error) {
		return make([]byte, (endMS-startMS)/1000+1), nil
	}
	return h
}

func (h *rdHarness) run(t *testing.T, relPath string) redecodeMeta {
	t.Helper()
	if err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc(relPath), make([]byte, 300_000)); err != nil {
		t.Fatalf("transcription failed: %v\nlogs:\n%s", err, h.logs.String())
	}
	if len(h.store.reps) == 0 {
		t.Fatalf("no representation persisted\nlogs:\n%s", h.logs.String())
	}
	var m redecodeMeta
	if err := json.Unmarshal([]byte(h.store.reps[len(h.store.reps)-1].MetaJSON), &m); err != nil {
		t.Fatalf("decode meta_json: %v", err)
	}
	if m.Coverage == nil || m.Coverage.WindowsAttempted != 3 {
		t.Fatalf("want a recorded three-window decode, got coverage %+v\nlogs:\n%s", m.Coverage, h.logs.String())
	}
	return m
}

func (h *rdHarness) chunkText() string {
	var b strings.Builder
	for _, c := range h.store.chunks {
		b.WriteString(c.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestRedecode_LowConfidenceWindowIsReplaced is the #1143 case: window 1 is
// decoded once more with uk and its text is replaced; the record names it.
func TestLowConfidenceRedecode_LowConfidenceWindowIsReplaced(t *testing.T) {
	t.Parallel()
	tr := &rdTranscriber{script: jingleScript(), hintText: rdUkHintText}
	h := newRedecodeHarness(t, tr, 0.5)
	m := h.run(t, "news/jingle.mp3")

	if got := tr.hintCalls(); len(got) != 1 || got[0] != "uk" {
		t.Fatalf("hinted decodes = %v, want exactly one in uk", got)
	}
	if m.Language != "uk" {
		t.Errorf("item language = %q, want uk", m.Language)
	}
	if m.RedecodeRule != "low-confidence" {
		t.Errorf("redecode_rule = %q, want low-confidence", m.RedecodeRule)
	}
	if len(m.RedecodedWindows) != 1 {
		t.Fatalf("redecoded_windows = %+v, want one entry", m.RedecodedWindows)
	}
	w := m.RedecodedWindows[0]
	if w.StartMS != 0 || w.EndMS != rdWin1EndMS || w.FirstLanguage != "en" || w.FirstConfidence != 0.34 || w.Language != "uk" || !w.Replaced {
		t.Errorf("redecoded window = %+v, want [0,%d) en@0.34 -> uk replaced", w, rdWin1EndMS)
	}
	text := h.chunkText()
	if !strings.Contains(text, "Доброго вечора") {
		t.Errorf("the second decode is not in the index:\n%s", text)
	}
	if strings.Contains(text, "Good evening") {
		t.Errorf("the first decode of window 1 is still in the index:\n%s", text)
	}
}

// TestRedecode_EmptySecondDecodeKeepsTheFirst: a second decode with no
// content leaves the first decode and records replaced=false.
func TestLowConfidenceRedecode_EmptySecondDecodeKeepsTheFirst(t *testing.T) {
	t.Parallel()
	tr := &rdTranscriber{script: jingleScript(), hintText: "[00:00]   "}
	h := newRedecodeHarness(t, tr, 0.5)
	m := h.run(t, "news/empty.mp3")

	if got := tr.hintCalls(); len(got) != 1 {
		t.Fatalf("hinted decodes = %v, want exactly one", got)
	}
	if len(m.RedecodedWindows) != 1 || m.RedecodedWindows[0].Replaced {
		t.Fatalf("redecoded_windows = %+v, want one entry with replaced=false", m.RedecodedWindows)
	}
	if !strings.Contains(h.chunkText(), "Good evening") {
		t.Errorf("the first decode of window 1 was lost:\n%s", h.chunkText())
	}
}

// TestRedecode_NoAnchorNoRedecode: no window reported the item language at or
// above the threshold, so nothing qualifies. The rule still records that it
// ran.
func TestLowConfidenceRedecode_NoAnchorNoRedecode(t *testing.T) {
	t.Parallel()
	script := jingleScript()
	script[2] = rdReply{lang: "uk", conf: 0.45, text: rdUkrainian}
	script[3] = rdReply{lang: "uk", conf: 0.4, text: rdUkrainian}
	tr := &rdTranscriber{script: script, hintText: rdUkHintText}
	h := newRedecodeHarness(t, tr, 0.5)
	m := h.run(t, "news/noanchor.mp3")

	if got := tr.hintCalls(); len(got) != 0 {
		t.Fatalf("hinted decodes = %v, want none", got)
	}
	if m.RedecodeRule != "low-confidence" || len(m.RedecodedWindows) != 0 {
		t.Errorf("redecode_rule=%q redecoded_windows=%+v, want low-confidence and no list", m.RedecodeRule, m.RedecodedWindows)
	}
}

// TestRedecode_NoReportedConfidenceDoesNotQualify: a window with a language
// but no confidence does not qualify.
func TestLowConfidenceRedecode_NoReportedConfidenceDoesNotQualify(t *testing.T) {
	t.Parallel()
	script := jingleScript()
	script[1] = rdReply{lang: "en", conf: 0, text: rdEnglish}
	tr := &rdTranscriber{script: script, hintText: rdUkHintText}
	h := newRedecodeHarness(t, tr, 0.5)
	m := h.run(t, "news/noconf.mp3")

	if got := tr.hintCalls(); len(got) != 0 {
		t.Fatalf("hinted decodes = %v, want none", got)
	}
	if m.RedecodeRule != "low-confidence" {
		t.Errorf("redecode_rule = %q, want low-confidence", m.RedecodeRule)
	}
}

// TestRedecode_OffCases: a pin, window scope and redecode_below 0 each leave
// the rule off: no second decode and no record.
func TestLowConfidenceRedecode_OffCases(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]struct {
		below float64
		apply func(*ingest.Service)
	}{
		"pin":           {below: 0.5, apply: func(s *ingest.Service) { s.SetTranscriptLanguage("uk") }},
		"window scope":  {below: 0.5, apply: func(s *ingest.Service) { s.SetLanguageScope("window") }},
		"threshold off": {below: 0, apply: func(*ingest.Service) {}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tr := &rdTranscriber{script: jingleScript(), hintText: rdUkHintText}
			h := newRedecodeHarness(t, tr, setup.below)
			setup.apply(h.svc)
			m := h.run(t, "news/off.mp3")
			if got := tr.hintCalls(); len(got) != 0 {
				t.Fatalf("hinted decodes = %v, want none", got)
			}
			if m.RedecodeRule != "" || len(m.RedecodedWindows) != 0 {
				t.Errorf("redecode_rule=%q redecoded_windows=%+v, want neither", m.RedecodeRule, m.RedecodedWindows)
			}
			if !strings.Contains(h.chunkText(), "Good evening") {
				t.Errorf("window 1 text changed with the rule off:\n%s", h.chunkText())
			}
		})
	}
}

// TestRedecode_OneWindowItemRecordsNothing: a recording that fits one
// request has one window, so the rule does not run.
func TestLowConfidenceRedecode_OneWindowItemRecordsNothing(t *testing.T) {
	t.Parallel()
	tr := &rdTranscriber{script: map[int]rdReply{1: {lang: "en", conf: 0.34, text: rdEnglish}}, hintText: rdUkHintText}
	h := newRedecodeHarness(t, tr, 0.5)
	h.svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 5 * time.Minute, nil }
	if err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("news/short.mp3"), make([]byte, 1000)); err != nil {
		t.Fatalf("transcription failed: %v", err)
	}
	if len(h.store.reps) != 1 {
		t.Fatalf("persisted %d representations, want 1", len(h.store.reps))
	}
	if got := tr.hintCalls(); len(got) != 0 {
		t.Fatalf("hinted decodes = %v, want none", got)
	}
	meta := h.store.reps[0].MetaJSON
	if !strings.Contains(meta, `"source":"stt"`) {
		t.Fatalf("meta_json is not an STT transcript: %s", meta)
	}
	if strings.Contains(meta, "redecode") {
		t.Errorf("a one-window item recorded a re-decode field: %s", meta)
	}
}

// TestRedecode_IdentityAndCacheUnchanged: the rule and the key do not join the
// derivation identity, and a second run serves the cached text and record
// without a decode.
func TestLowConfidenceRedecode_IdentityAndCacheUnchanged(t *testing.T) {
	t.Parallel()
	on := newRedecodeHarness(t, &rdTranscriber{}, 0.5)
	off := newRedecodeHarness(t, &rdTranscriber{}, 0)
	idOn, idOff := on.svc.ActiveTranscriptIdentity(), off.svc.ActiveTranscriptIdentity()
	if idOn == "" || idOn != idOff {
		t.Fatalf("identity with the rule on = %q, off = %q; want equal and non-empty", idOn, idOff)
	}

	tr := &rdTranscriber{script: jingleScript(), hintText: rdUkHintText}
	h := newRedecodeHarness(t, tr, 0.5)
	first := h.run(t, "news/cache.mp3")
	calls := tr.callCount()
	second := h.run(t, "news/cache.mp3")
	if tr.callCount() != calls || len(tr.hintCalls()) != 1 {
		t.Errorf("the second run decoded again (calls %d -> %d, hints %v), want a cache hit", calls, tr.callCount(), tr.hintCalls())
	}
	if len(second.RedecodedWindows) != 1 || second.RedecodedWindows[0] != first.RedecodedWindows[0] || second.RedecodeRule != first.RedecodeRule {
		t.Errorf("the cache hit lost the record: first %+v, second %+v", first.RedecodedWindows, second.RedecodedWindows)
	}
}

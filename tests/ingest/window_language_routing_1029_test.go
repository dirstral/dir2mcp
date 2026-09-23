package tests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// SPEC §8.2.2 (dirstral-spec 0.71.0), dir2mcp #1029. §8.2.1 resolves ONE
// language per recording and routes the whole recording on it, so a recording
// that changes language inside itself is decoded under one language for its
// whole length and the minority passages come out in the wrong script. Under
// media.stt.language_scope: window the decoder's own language report identifies
// each window, the route is chosen per window, the honest-coverage floor is
// evaluated per window, and the transcript records what happened in
// coverage.languages and coverage.refused.
//
// The recording in every test is 30 minutes, which the default 10-minute
// decode window schedules as four windows starting at 0, 590, 1180 and 1770 s
// (window minus a 10 s overlap). A window's CORE, the stretch the merge keeps of
// it, runs to the next window's start; the last one runs to the end.

const (
	scopeTotalMS = 30 * 60 * 1000
	scopeWinMS   = 600_000
	win2StartMS  = 590_000
	win3StartMS  = 1_180_000
	win4StartMS  = 1_770_000
)

// langReply is what the fake decoder answers for one call: the language it
// reports, its confidence (0 = none reported), an optional transcript, or a
// provider failure instead of any of them.
type langReply struct {
	lang string
	conf float64
	text string
	err  error
}

// langWindowTranscriber is a fake STT provider that reports a scripted language
// per call, keyed on the call ordinal (1-based), the way a Whisper server
// reports the language it decoded under. A call with no script entry answers
// def. Every reply is a two-line window-relative transcript unless the entry
// carries its own text.
type langWindowTranscriber struct {
	mu     sync.Mutex
	calls  int
	script map[int]langReply
	def    langReply
}

func (w *langWindowTranscriber) Transcribe(ctx context.Context, relPath string, data []byte) (string, error) {
	res, err := w.TranscribeStructured(ctx, relPath, data)
	return res.Text, err
}

func (w *langWindowTranscriber) TranscribeStructured(context.Context, string, []byte) (model.TranscriptResult, error) {
	w.mu.Lock()
	w.calls++
	n := w.calls
	r, ok := w.script[n]
	w.mu.Unlock()
	if !ok {
		r = w.def
	}
	if r.err != nil {
		return model.TranscriptResult{}, r.err
	}
	text := r.text
	if text == "" {
		text = fmt.Sprintf("[00:00] opening line of decode %d spoken at some length\n[00:30] closing line of decode %d spoken at some length", n, n)
	}
	return model.TranscriptResult{Text: text, Language: r.lang, LanguageConfidence: r.conf}, nil
}

// MaxTranscribePayloadBytes declares no cap, so only the duration rule windows.
func (w *langWindowTranscriber) MaxTranscribePayloadBytes() int { return 0 }

func (w *langWindowTranscriber) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// scopedMeta is the wire shape of the §8.2.2 fields on a transcript meta_json.
type scopedMeta struct {
	Language           string   `json:"language"`
	LanguageSource     string   `json:"language_source"`
	LanguageConfidence *float64 `json:"language_confidence"`
	LanguageCovered    *bool    `json:"language_covered"`
	LanguageScope      string   `json:"language_scope"`
	LanguageRoutes     string   `json:"language_routes"`
	Coverage           *struct {
		WindowsAttempted int `json:"windows_attempted"`
		WindowsDecoded   int `json:"windows_decoded"`
		Ranges           []struct {
			StartMS int `json:"start_ms"`
			EndMS   int `json:"end_ms"`
		} `json:"ranges"`
		Languages []covLang    `json:"languages"`
		Refused   []covRefused `json:"refused"`
	} `json:"coverage"`
}

// covLang is one coverage.languages entry as a consumer reads it.
type covLang struct {
	StartMS            int      `json:"start_ms"`
	EndMS              int      `json:"end_ms"`
	Language           string   `json:"language"`
	LanguageSource     string   `json:"language_source"`
	LanguageConfidence *float64 `json:"language_confidence"`
	Route              string   `json:"route"`
	Covered            bool     `json:"covered"`
}

// covRefused is one coverage.refused entry.
type covRefused struct {
	StartMS int    `json:"start_ms"`
	EndMS   int    `json:"end_ms"`
	Reason  string `json:"reason"`
}

func f64(v float64) *float64 { return &v }

// assertLang compares one coverage.languages entry field by field, so a test
// reads as a table of expected entries rather than a chain of conditions.
func assertLang(t *testing.T, label string, got, want covLang) {
	t.Helper()
	if got.Language != want.Language || got.LanguageSource != want.LanguageSource {
		t.Errorf("%s: language/source = %q/%q, want %q/%q", label, got.Language, got.LanguageSource, want.Language, want.LanguageSource)
	}
	if got.StartMS != want.StartMS || got.EndMS != want.EndMS {
		t.Errorf("%s: range = [%d,%d), want [%d,%d)", label, got.StartMS, got.EndMS, want.StartMS, want.EndMS)
	}
	if got.Route != want.Route || got.Covered != want.Covered {
		t.Errorf("%s: route/covered = %q/%v, want %q/%v", label, got.Route, got.Covered, want.Route, want.Covered)
	}
	switch {
	case want.LanguageConfidence == nil && got.LanguageConfidence != nil:
		t.Errorf("%s: confidence = %v, want none", label, *got.LanguageConfidence)
	case want.LanguageConfidence != nil && got.LanguageConfidence == nil:
		t.Errorf("%s: confidence absent, want %v", label, *want.LanguageConfidence)
	case want.LanguageConfidence != nil && *got.LanguageConfidence != *want.LanguageConfidence:
		t.Errorf("%s: confidence = %v, want %v", label, *got.LanguageConfidence, *want.LanguageConfidence)
	}
}

// assertLangs asserts the whole coverage.languages array.
func assertLangs(t *testing.T, got []covLang, want []covLang) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("coverage.languages = %+v, want %d entries: %+v", got, len(want), want)
	}
	for i := range want {
		assertLang(t, fmt.Sprintf("entry %d", i), got[i], want[i])
	}
}

// assertRepLanguage asserts the representation-level language fields.
func assertRepLanguage(t *testing.T, meta scopedMeta, lang, source string, conf *float64) {
	t.Helper()
	if meta.Language != lang || meta.LanguageSource != source {
		t.Errorf("representation language = %q/%q, want %q/%q", meta.Language, meta.LanguageSource, lang, source)
	}
	switch {
	case conf == nil && meta.LanguageConfidence != nil:
		t.Errorf("representation confidence = %v, want none", *meta.LanguageConfidence)
	case conf != nil && meta.LanguageConfidence == nil:
		t.Errorf("representation confidence absent, want %v", *conf)
	case conf != nil && *meta.LanguageConfidence != *conf:
		t.Errorf("representation confidence = %v, want %v", *meta.LanguageConfidence, *conf)
	}
}

// assertSpanLanguages checks that every time span from boundaryMS on carries
// lang and every earlier one carries none, and that both sides exist.
func assertSpanLanguages(t *testing.T, spans []model.Span, boundaryMS int, lang string) {
	t.Helper()
	var before, after int
	for _, sp := range spans {
		if sp.Kind != "time" {
			continue
		}
		if sp.StartMS >= boundaryMS {
			after++
			if sp.Language != lang {
				t.Errorf("segment at %d ms is past the language change but records %q, want %q", sp.StartMS, sp.Language, lang)
			}
			continue
		}
		before++
		if sp.Language != "" {
			t.Errorf("segment at %d ms is in the representation's language but records %q, want none", sp.StartMS, sp.Language)
		}
	}
	if before == 0 || after == 0 {
		t.Fatalf("expected segments on both sides of the language change, got %d before and %d after", before, after)
	}
}

func decodeScopedMeta(t *testing.T, metaJSON string) scopedMeta {
	t.Helper()
	var m scopedMeta
	if err := json.Unmarshal([]byte(metaJSON), &m); err != nil {
		t.Fatalf("decode meta_json %q: %v", metaJSON, err)
	}
	return m
}

// newScopedHarness wires the #954 windowing harness with the language-reporting
// fake as the DEFAULT route under window scope, identified as provider "whisper".
func newScopedHarness(t *testing.T, tr *langWindowTranscriber, totalMS int) (*windowSTTHarness, []byte) {
	t.Helper()
	content := make([]byte, 300_000)
	h := newWindowSTTHarness(t, &windowRecordingTranscriber{}, totalMS, content)
	h.svc.SetTranscriber(tr)
	h.svc.SetSTTIdentity("whisper", "large-v3")
	h.svc.SetLanguageScope("window")
	return h, content
}

// persistedText joins the chunk texts the run persisted, in order. The transcript
// cache file is keyed on the STT identity these tests set, so it is read from
// the store rather than from a bytes-only cache path.
func persistedText(h *windowSTTHarness) string {
	var b strings.Builder
	for _, c := range h.store.chunks {
		b.WriteString(c.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

func runScoped(t *testing.T, h *windowSTTHarness, relPath string, content []byte) scopedMeta {
	t.Helper()
	if err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc(relPath), content); err != nil {
		t.Fatalf("transcription failed: %v\nlogs:\n%s", err, h.logs.String())
	}
	if len(h.store.reps) != 1 {
		t.Fatalf("persisted %d representations, want exactly 1\nlogs:\n%s", len(h.store.reps), h.logs.String())
	}
	return decodeScopedMeta(t, h.store.reps[0].MetaJSON)
}

// TestWindowLanguage_ItemScopeIsUnchanged pins the default: under item scope a
// language-reporting decoder changes nothing. No coverage.languages, no
// language_scope on the meta, and the representation language comes from the
// text detector as before, never from the decoder's per-window report.
func TestWindowLanguage_ItemScopeIsUnchanged(t *testing.T) {
	t.Parallel()
	tr := &langWindowTranscriber{def: langReply{lang: "uk", conf: 0.9}}
	h, content := newScopedHarness(t, tr, scopeTotalMS)
	h.svc.SetLanguageScope("item")

	meta := runScoped(t, h, "talks/item.m4a", content)
	if meta.LanguageScope != "" || meta.LanguageRoutes != "" {
		t.Errorf("item scope recorded language_scope=%q language_routes=%q, want neither", meta.LanguageScope, meta.LanguageRoutes)
	}
	if meta.Coverage == nil {
		t.Fatalf("a four-window decode must still record §8.6.13 coverage under item scope")
	}
	if len(meta.Coverage.Languages) != 0 || len(meta.Coverage.Refused) != 0 {
		t.Errorf("item scope recorded languages=%+v refused=%+v, want none", meta.Coverage.Languages, meta.Coverage.Refused)
	}
	if meta.Language == "uk" {
		t.Errorf("item scope took the decoder's per-window language %q; the item-level resolution must stand", meta.Language)
	}
}

// TestWindowLanguage_RecordsCoalescedLanguagesAndRepresentationLanguage is the
// core of §8.2.2: two Russian windows then two Ukrainian ones become two
// coalesced coverage.languages entries clipped to the window cores; the
// representation language is the one with the largest covered duration
// (Russian, 1180 s against 620 s), recorded as detected with the reported
// confidence; and every segment in the minority language carries its own
// language on its span, while the majority segments carry none.
//
// Mutants killed: recording one entry per window (four, not two); recording the
// full piece span instead of the core (overlapping entries); picking the LAST
// language instead of the largest; stamping every segment or none.
func TestWindowLanguage_RecordsCoalescedLanguagesAndRepresentationLanguage(t *testing.T) {
	t.Parallel()
	tr := &langWindowTranscriber{script: map[int]langReply{
		1: {lang: "ru", conf: 0.95}, 2: {lang: "ru", conf: 0.9},
		3: {lang: "uk", conf: 0.92}, 4: {lang: "uk", conf: 0.88},
	}}
	h, content := newScopedHarness(t, tr, scopeTotalMS)

	meta := runScoped(t, h, "talks/mixed.m4a", content)
	if meta.LanguageScope != "window" {
		t.Errorf("language_scope = %q, want window", meta.LanguageScope)
	}
	if meta.Coverage == nil {
		t.Fatal("no coverage recorded under window scope")
	}
	assertLangs(t, meta.Coverage.Languages, []covLang{
		{StartMS: 0, EndMS: win3StartMS, Language: "ru", LanguageSource: "detected", LanguageConfidence: f64(0.9), Route: "whisper", Covered: true},
		{StartMS: win3StartMS, EndMS: scopeTotalMS, Language: "uk", LanguageSource: "detected", LanguageConfidence: f64(0.88), Route: "whisper", Covered: true},
	})
	assertRepLanguage(t, meta, "ru", "detected", f64(0.9))
	if meta.LanguageCovered != nil {
		t.Errorf("language_covered = %v, want absent: every range is covered", *meta.LanguageCovered)
	}
	assertSpanLanguages(t, h.store.spans, win3StartMS, "uk")
}

// TestWindowLanguage_InheritanceAndUnknown pins continuity: a first window whose
// report is below the floor, with nothing to inherit, is recorded WITHOUT a
// language tag and covered; a later window below the floor inherits the
// preceding window's language as its own "inherited" entry, never merged into
// the detected one; and a window that reports nothing and has too little text
// to detect inherits too.
//
// Mutants killed: inventing a tag for the unknown window; merging inherited
// into detected (the coalescing key must include the source); repeating the
// detected confidence on the inherited entry.
func TestWindowLanguage_InheritanceAndUnknown(t *testing.T) {
	t.Parallel()
	tr := &langWindowTranscriber{script: map[int]langReply{
		1: {lang: "ru", conf: 0.2},
		2: {lang: "uk", conf: 0.9},
		3: {lang: "uk", conf: 0.3},
		4: {lang: "", conf: 0, text: "[00:00] ok"},
	}}
	h, content := newScopedHarness(t, tr, scopeTotalMS)

	meta := runScoped(t, h, "talks/inherit.m4a", content)
	assertLangs(t, meta.Coverage.Languages, []covLang{
		{StartMS: 0, EndMS: win2StartMS, Route: "whisper", Covered: true},
		{StartMS: win2StartMS, EndMS: win3StartMS, Language: "uk", LanguageSource: "detected", LanguageConfidence: f64(0.9), Route: "whisper", Covered: true},
		{StartMS: win3StartMS, EndMS: scopeTotalMS, Language: "uk", LanguageSource: "inherited", Route: "whisper", Covered: true},
	})
	assertRepLanguage(t, meta, "uk", "detected", f64(0.9))
	// The unknown first window's spans record "und", the undetermined tag, so
	// a chunk built from them is never read or filtered as Ukrainian; the
	// spans of the uk windows record nothing, since uk is the representation
	// language.
	var und, plain int
	for _, sp := range h.store.spans {
		if sp.Kind != "time" {
			continue
		}
		switch {
		case sp.StartMS < win2StartMS:
			und++
			if sp.Language != "und" {
				t.Errorf("segment at %d ms is in the unknown window but records %q, want und", sp.StartMS, sp.Language)
			}
		default:
			plain++
			if sp.Language != "" {
				t.Errorf("segment at %d ms is in the representation's language but records %q, want none", sp.StartMS, sp.Language)
			}
		}
	}
	if und == 0 || plain == 0 {
		t.Fatalf("expected segments in both the unknown window and the uk windows, got %d and %d", und, plain)
	}
}

// TestWindowLanguage_RoutesPerWindowAndRedecodesOnlyMovedWindows pins routing: a
// window whose resolved language maps to another profile is decoded AGAIN on
// that profile and its entry names that route; windows that stay put are not
// re-decoded. The default decoder identifies windows 1 and 2, the routed one
// identifies 3 and 4 (a routed decoder reports its own view), and the route
// moves back when it reports Russian.
//
// Mutants killed: routing without re-decoding (the routed fake sees nothing);
// re-decoding every window (both fakes see four calls); recording the default
// route on a routed entry.
func TestWindowLanguage_RoutesPerWindowAndRedecodesOnlyMovedWindows(t *testing.T) {
	t.Parallel()
	def := &langWindowTranscriber{script: map[int]langReply{
		1: {lang: "ru", conf: 0.9}, 2: {lang: "uk", conf: 0.9}, 3: {lang: "ru", conf: 0.9},
	}}
	ukRoute := &langWindowTranscriber{script: map[int]langReply{
		1: {lang: "uk", conf: 0.95, text: "[00:00] uk route decode 1 spoken at some length\n[00:30] uk route decode 1 closing line"},
		2: {lang: "uk", conf: 0.95, text: "[00:00] uk route decode 2 spoken at some length\n[00:30] uk route decode 2 closing line"},
		3: {lang: "ru", conf: 0.9, text: "[00:00] uk route decode 3 spoken at some length\n[00:30] uk route decode 3 closing line"},
	}}
	h, content := newScopedHarness(t, def, scopeTotalMS)
	h.svc.SetRouteTranscriber("uk", ukRoute, "whisper-uk", []string{"uk"})

	meta := runScoped(t, h, "talks/routed.m4a", content)
	if got := def.callCount(); got != 3 {
		t.Errorf("default route decoded %d windows, want 3 (windows 1, 2 and the moved-back 4)", got)
	}
	if got := ukRoute.callCount(); got != 3 {
		t.Errorf("uk route decoded %d windows, want 3 (window 2 re-decoded, then 3 and 4)", got)
	}
	assertLangs(t, meta.Coverage.Languages, []covLang{
		{StartMS: 0, EndMS: win2StartMS, Language: "ru", LanguageSource: "detected", LanguageConfidence: f64(0.9), Route: "whisper", Covered: true},
		// Window 2 was identified by the DEFAULT decoder (0.9) before it moved, and
		// window 3 by the uk route (0.95); the coalesced entry keeps the minimum.
		{StartMS: win2StartMS, EndMS: win4StartMS, Language: "uk", LanguageSource: "detected", LanguageConfidence: f64(0.9), Route: "whisper-uk", Covered: true},
		{StartMS: win4StartMS, EndMS: scopeTotalMS, Language: "ru", LanguageSource: "detected", LanguageConfidence: f64(0.9), Route: "whisper", Covered: true},
	})
	// The merged transcript carries the ROUTED decode of the moved windows, not
	// the default decoder's first attempt: the uk route's own text for windows 2
	// and 3, and none of the default decoder's discarded window-2 decode. Window
	// 4 moved back, so the uk route's decode 3 is discarded in turn.
	text := persistedText(h)
	if !strings.Contains(text, "uk route decode 1") || !strings.Contains(text, "uk route decode 2") {
		t.Errorf("routed windows must carry the uk route's text (its decodes 1 and 2), got:\n%s", text)
	}
	if strings.Contains(text, "opening line of decode 2") || strings.Contains(text, "uk route decode 3") {
		t.Errorf("a discarded decode reached the transcript:\n%s", text)
	}
}

// TestWindowLanguage_FloorPerWindow_Warn pins the fail-open floor: a window whose
// language is outside the decoding route's declared coverage is still indexed,
// its entry records covered=false, the transcript records language_covered=false,
// and the operator is told.
func TestWindowLanguage_FloorPerWindow_Warn(t *testing.T) {
	t.Parallel()
	tr := &langWindowTranscriber{script: map[int]langReply{
		1: {lang: "ru", conf: 0.9}, 2: {lang: "ru", conf: 0.9}, 3: {lang: "uk", conf: 0.9}, 4: {lang: "ru", conf: 0.9},
	}}
	h, content := newScopedHarness(t, tr, scopeTotalMS)
	h.svc.SetSTTLanguages([]string{"ru"})
	h.svc.SetOnUncoveredLanguage("warn")

	meta := runScoped(t, h, "talks/warn.m4a", content)
	if meta.Coverage.WindowsDecoded != 4 {
		t.Errorf("windows_decoded = %d, want 4: warn keeps the uncovered window", meta.Coverage.WindowsDecoded)
	}
	var uncovered int
	for _, e := range meta.Coverage.Languages {
		if e.Language == "uk" {
			if e.Covered {
				t.Errorf("uk entry %+v records covered=true, but the route declares only ru", e)
			}
			uncovered++
		} else if !e.Covered {
			t.Errorf("ru entry %+v records covered=false", e)
		}
	}
	if uncovered != 1 {
		t.Errorf("found %d uncovered entries, want exactly the Ukrainian window", uncovered)
	}
	if meta.LanguageCovered == nil || *meta.LanguageCovered {
		t.Errorf("language_covered = %v, want false: one decoded range is uncovered", meta.LanguageCovered)
	}
	if !strings.Contains(h.logs.String(), "covered=false") {
		t.Errorf("no warning named the uncovered window:\n%s", h.logs.String())
	}
}

// TestWindowLanguage_FloorPerWindow_SkipRefusesOnlyThatWindow pins the strict
// floor: the uncovered window is NOT decoded into the transcript, it is listed
// in coverage.refused with its core range, it is absent from ranges and from
// windows_decoded, and the other three windows are indexed as usual.
//
// Mutants killed: refusing the whole item (§8.2.1 behaviour); counting the
// refused window as decoded; leaving its text in the merged transcript.
func TestWindowLanguage_FloorPerWindow_SkipRefusesOnlyThatWindow(t *testing.T) {
	t.Parallel()
	tr := &langWindowTranscriber{script: map[int]langReply{
		1: {lang: "ru", conf: 0.9}, 2: {lang: "ru", conf: 0.9}, 3: {lang: "uk", conf: 0.9}, 4: {lang: "ru", conf: 0.9},
	}}
	h, content := newScopedHarness(t, tr, scopeTotalMS)
	h.svc.SetSTTLanguages([]string{"ru"})
	h.svc.SetOnUncoveredLanguage("skip")

	meta := runScoped(t, h, "talks/skip.m4a", content)
	cov := meta.Coverage
	if cov.WindowsAttempted != 4 || cov.WindowsDecoded != 3 {
		t.Errorf("coverage = %d/%d windows, want 3 decoded of 4 attempted", cov.WindowsDecoded, cov.WindowsAttempted)
	}
	// The second window decoded through its full 600 s, ten seconds into the
	// third window's core, and the merge keeps that tail because the third
	// window is missing. So the refused range begins where the decoded audio
	// ends, the decoded ranges and the refused range are disjoint, and the
	// retained tail carries the second window's ru entry.
	refusedStart := win2StartMS + scopeWinMS
	if len(cov.Refused) != 1 || cov.Refused[0].Reason != "language_uncovered" || cov.Refused[0].StartMS != refusedStart || cov.Refused[0].EndMS != win4StartMS {
		t.Errorf("refused = %+v, want one language_uncovered range [%d,%d)", cov.Refused, refusedStart, win4StartMS)
	}
	for _, r := range cov.Ranges {
		if r.StartMS < win4StartMS && r.EndMS > refusedStart {
			t.Errorf("decoded range %+v overlaps the refused range [%d,%d)", r, refusedStart, win4StartMS)
		}
	}
	assertLangs(t, cov.Languages, []covLang{
		{StartMS: 0, EndMS: refusedStart, Language: "ru", LanguageSource: "detected", LanguageConfidence: f64(0.9), Route: "whisper", Covered: true},
		{StartMS: win4StartMS, EndMS: scopeTotalMS, Language: "ru", LanguageSource: "detected", LanguageConfidence: f64(0.9), Route: "whisper", Covered: true},
	})
	text := persistedText(h)
	if strings.Contains(text, "decode 3") {
		t.Errorf("the refused window's text reached the transcript:\n%s", text)
	}
	if !strings.Contains(h.logs.String(), "refusing window") {
		t.Errorf("no log line named the refusal:\n%s", h.logs.String())
	}
}

// TestWindowLanguage_AllWindowsRefusedIsALanguageSkip pins the §8.2.2 terminal
// status: when every window is refused for its language under skip, nothing is
// persisted and the document is a durable status=skipped with
// skip_reason=language_uncovered, the same reason the §8.2.1 item-level skip
// records, so the skip_reasons aggregate tells it apart from a partial decode.
func TestWindowLanguage_AllWindowsRefusedIsALanguageSkip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	cfg := config.Config{RootDir: root, StateDir: t.TempDir(), STTProvider: "off"}
	svc := mustNewIngestService(t, cfg, st)
	svc.SetIndexingState(appstate.NewIndexingState(appstate.ModeIncremental))
	svc.SetTranscriber(&langWindowTranscriber{def: langReply{lang: "uk", conf: 0.9}})
	svc.SetSTTIdentity("whisper", "large-v3")
	svc.SetSTTLanguages([]string{"ru"})
	svc.SetOnUncoveredLanguage("skip")
	svc.SetLanguageScope("window")
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) {
		return time.Duration(scopeTotalMS) * time.Millisecond, nil
	}
	svc.ExtractSegmentFunc = func(_ context.Context, _ string, startMS, endMS int) ([]byte, error) {
		return make([]byte, 10*(endMS-startMS)/scopeTotalMS+1), nil
	}

	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(ctx, f, nil, false); err != nil {
		t.Fatalf("ProcessDocument hard-failed on an all-refused decode: %v", err)
	}
	doc := mustGetDoc(t, st, "talk.mp3")
	if doc.Status != "skipped" || doc.SkipReason != model.SkipReasonLanguageUncovered {
		t.Fatalf("talk.mp3: status=%q skip_reason=%q, want skipped/%s", doc.Status, doc.SkipReason, model.SkipReasonLanguageUncovered)
	}
}

// TestWindowLanguage_SingleRequestIsOneWindow pins that a recording which fits
// one request is still ONE window under window scope: coverage.languages is
// recorded (required whatever the window count) with one attempted window
// spanning the recording.
func TestWindowLanguage_SingleRequestIsOneWindow(t *testing.T) {
	t.Parallel()
	const shortMS = 5 * 60 * 1000
	tr := &langWindowTranscriber{def: langReply{lang: "ka", conf: 0.8}}
	h, content := newScopedHarness(t, tr, shortMS)

	meta := runScoped(t, h, "talks/short.m4a", content)
	if meta.Coverage == nil {
		t.Fatal("a one-window decode under window scope must record coverage (languages is required)")
	}
	if meta.Coverage.WindowsAttempted != 1 || meta.Coverage.WindowsDecoded != 1 {
		t.Errorf("coverage = %d/%d, want 1/1", meta.Coverage.WindowsDecoded, meta.Coverage.WindowsAttempted)
	}
	if len(meta.Coverage.Languages) != 1 || meta.Coverage.Languages[0].Language != "ka" || meta.Coverage.Languages[0].EndMS != shortMS {
		t.Errorf("languages = %+v, want one ka entry over the whole recording", meta.Coverage.Languages)
	}
	if meta.Language != "ka" {
		t.Errorf("representation language = %q, want ka", meta.Language)
	}
	if got := tr.callCount(); got != 1 {
		t.Errorf("decoder saw %d requests, want 1: a short recording is not sliced", got)
	}
}

// TestWindowLanguage_PinAppliesToEveryWindow pins that an operator pin disables
// detection: every window records the pinned language as configured, and the
// representation language is the pin, whatever the decoder reports.
func TestWindowLanguage_PinAppliesToEveryWindow(t *testing.T) {
	t.Parallel()
	tr := &langWindowTranscriber{def: langReply{lang: "uk", conf: 0.99}}
	h, content := newScopedHarness(t, tr, scopeTotalMS)
	h.svc.SetTranscriptLanguage("ru")

	meta := runScoped(t, h, "talks/pinned.m4a", content)
	if len(meta.Coverage.Languages) != 1 || meta.Coverage.Languages[0].Language != "ru" || meta.Coverage.Languages[0].LanguageSource != "configured" {
		t.Errorf("languages = %+v, want one ru configured entry over the whole recording", meta.Coverage.Languages)
	}
	if meta.Language != "ru" || meta.LanguageSource != "configured" || meta.LanguageConfidence != nil {
		t.Errorf("representation language = %q/%q/%v, want ru/configured with no confidence", meta.Language, meta.LanguageSource, meta.LanguageConfidence)
	}
}

// TestWindowLanguage_RefusedPlusFailedWindowsIsAFailure pins the other half of
// the terminal-status rule: when no window decoded and one of them FAILED (a
// provider error, not a refusal), the decode is a failure to retry, never a
// language skip that would leave the failed window untried for good.
//
// Mutant killed: returning the refusal-only success whenever at least one
// window was refused (one refused window plus one transport failure became a
// durable skipped/language_uncovered).
func TestWindowLanguage_RefusedPlusFailedWindowsIsAFailure(t *testing.T) {
	t.Parallel()
	tr := &langWindowTranscriber{
		def:    langReply{lang: "uk", conf: 0.9},
		script: map[int]langReply{4: {err: errors.New("whisper: 503 upstream")}},
	}
	h, content := newScopedHarness(t, tr, scopeTotalMS)
	h.svc.SetSTTLanguages([]string{"ru"})
	h.svc.SetOnUncoveredLanguage("skip")

	err := h.svc.GenerateTranscriptRepresentation(context.Background(), mediaDoc("talks/mixed.m4a"), content)
	if err == nil {
		t.Fatalf("three refused windows plus one failed window returned success; the failure must win\nlogs:\n%s", h.logs.String())
	}
	if !strings.Contains(err.Error(), "503 upstream") || !strings.Contains(err.Error(), "3 refused") {
		t.Errorf("error = %v, want the provider failure with the refusal count", err)
	}
	if len(h.store.reps) != 0 {
		t.Errorf("persisted %d representations on a failed decode, want none", len(h.store.reps))
	}
}

// TestWindowLanguage_RouteIdentityNamesTheModel pins §8.6.7 for the route table:
// the identity component names each route's profile AND the model it binds, so a
// profile that keeps its name while its model changes re-derives the transcripts
// decoded through it. Naming the profile alone would let the old cache pass.
func TestWindowLanguage_RouteIdentityNamesTheModel(t *testing.T) {
	t.Parallel()
	identity := func(model string) string {
		yaml := "" +
			"providers:\n" +
			"  whisper-uk:\n" +
			"    kind: whisper\n" +
			"    base_url: http://127.0.0.1:1\n" +
			"    stt_model: " + model + "\n" +
			"    stt_languages: [uk]\n" +
			"media:\n" +
			"  stt:\n" +
			"    language_scope: window\n" +
			"    language_providers:\n" +
			"      uk: whisper-uk\n"
		path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
		writeFile(t, path, yaml)
		cfg, err := config.LoadFile(path)
		if err != nil {
			t.Fatalf("LoadFile: %v", err)
		}
		cfg.StateDir = t.TempDir()
		svc := mustNewIngestService(t, cfg, &fakeIngestStore{})
		svc.SetSTTIdentity("whisper", "large-v3")
		return svc.ActiveTranscriptIdentity()
	}
	before := identity("large-v3-uk")
	after := identity("large-v3-uk-2026")
	if !strings.Contains(before, "uk=whisper-uk|large-v3-uk") {
		t.Errorf("identity %q does not name the route's profile and model", before)
	}
	if before == after {
		t.Errorf("changing the routed model left the identity unchanged: %q", before)
	}
}

// TestWindowLanguage_ScopeJoinsTheDerivationIdentity pins §8.6.7: the scope and
// route table join the transcript identity only under window scope, so every
// item-scoped corpus keeps a byte-stable identity and a corpus switched to
// window scope re-derives.
func TestWindowLanguage_ScopeJoinsTheDerivationIdentity(t *testing.T) {
	t.Parallel()
	svc := mustNewIngestService(t, config.Config{StateDir: t.TempDir()}, &fakeIngestStore{})
	svc.SetSTTIdentity("whisper", "large-v3")
	svc.SetLanguageScope("item")
	item := svc.ActiveTranscriptIdentity()
	svc.SetLanguageScope("window")
	window := svc.ActiveTranscriptIdentity()
	if item == window {
		t.Fatalf("window scope did not change the transcript identity: %q", item)
	}
	if !strings.HasPrefix(window, item) || !strings.Contains(window, "scope=window") {
		t.Errorf("window identity %q must extend the item identity %q with the scope component", window, item)
	}
}

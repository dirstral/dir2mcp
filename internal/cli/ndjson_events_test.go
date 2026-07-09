package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// decodeNDJSON parses the emitter output into events keyed by event name.
// Later occurrences win, which is what the periodic-progress assertions want:
// they care about the most recent counter values on the stream.
func decodeNDJSON(t *testing.T, raw string) map[string]map[string]interface{} {
	t.Helper()
	events := make(map[string]map[string]interface{})
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev struct {
			Level string                 `json:"level"`
			Event string                 `json:"event"`
			Data  map[string]interface{} `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("unmarshal ndjson line %q: %v", line, err)
		}
		ev.Data["__level"] = ev.Level
		events[ev.Event] = ev.Data
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan ndjson: %v", err)
	}
	return events
}

// TestEmitProgressEvents_CarriesLiveCounters is the regression guard for #414:
// scan_progress/embed_progress must report the real indexing counters, not the
// hardcoded zeros that were emitted once at startup.
func TestEmitProgressEvents_CarriesLiveCounters(t *testing.T) {
	var buf bytes.Buffer
	emitter := newNDJSONEmitter(&buf, true)

	emitProgressEvents(emitter, corpusIndexing{
		Scanned:         412,
		Indexed:         55,
		Skipped:         340,
		Deleted:         2,
		Representations: 88,
		ChunksTotal:     1480,
		EmbeddedOK:      920,
		Errors:          1,
	})

	events := decodeNDJSON(t, buf.String())

	scan, ok := events["scan_progress"]
	if !ok {
		t.Fatalf("no scan_progress event emitted; got %v", buf.String())
	}
	for field, want := range map[string]float64{
		"scanned": 412, "indexed": 55, "skipped": 340,
		"deleted": 2, "reps": 88, "chunks": 1480, "errors": 1,
	} {
		if got, _ := scan[field].(float64); got != want {
			t.Errorf("scan_progress[%s] = %v, want %v", field, scan[field], want)
		}
	}

	embed, ok := events["embed_progress"]
	if !ok {
		t.Fatalf("no embed_progress event emitted; got %v", buf.String())
	}
	for field, want := range map[string]float64{
		"embedded": 920, "chunks": 1480, "errors": 1,
	} {
		if got, _ := embed[field].(float64); got != want {
			t.Errorf("embed_progress[%s] = %v, want %v", field, embed[field], want)
		}
	}
}

// TestEmitProgressEvents_PassesThroughUnavailableSentinel pins the spec rule
// that -1 means "not derivable" (the ListFiles-only fallback) and MUST reach
// the client verbatim rather than being clamped to 0 — a 0 would be read as
// "nothing embedded", which is a different, wrong claim.
func TestEmitProgressEvents_PassesThroughUnavailableSentinel(t *testing.T) {
	var buf bytes.Buffer
	emitter := newNDJSONEmitter(&buf, true)

	emitProgressEvents(emitter, corpusIndexing{
		Scanned:         10,
		Representations: -1,
		ChunksTotal:     -1,
		EmbeddedOK:      -1,
	})

	events := decodeNDJSON(t, buf.String())
	if got, _ := events["scan_progress"]["reps"].(float64); got != -1 {
		t.Errorf("scan_progress[reps] = %v, want -1", events["scan_progress"]["reps"])
	}
	if got, _ := events["embed_progress"]["embedded"].(float64); got != -1 {
		t.Errorf("embed_progress[embedded] = %v, want -1", events["embed_progress"]["embedded"])
	}
}

func TestEmitProgressEvents_DisabledEmitterIsSilent(t *testing.T) {
	var buf bytes.Buffer
	emitProgressEvents(newNDJSONEmitter(&buf, false), corpusIndexing{Scanned: 1})
	if buf.Len() != 0 {
		t.Fatalf("disabled emitter wrote %q, want no output", buf.String())
	}
}

func TestEmitProgressEvents_NilEmitterDoesNotPanic(t *testing.T) {
	emitProgressEvents(nil, corpusIndexing{Scanned: 1})
}

// TestNewFileErrorEmitter_EmitsPerDocumentIdentity covers the other half of
// #414 part 3: a non-fatal per-document failure must reach the stream with the
// document identity attached, not just a bare message.
func TestNewFileErrorEmitter_EmitsPerDocumentIdentity(t *testing.T) {
	var buf bytes.Buffer
	emitter := newNDJSONEmitter(&buf, true)

	newFileErrorEmitter(emitter)("docs/broken.pdf", "pdf", "extract: encrypted document")

	events := decodeNDJSON(t, buf.String())
	fe, ok := events["file_error"]
	if !ok {
		t.Fatalf("no file_error event emitted; got %q", buf.String())
	}
	if got := fe["rel_path"]; got != "docs/broken.pdf" {
		t.Errorf("rel_path = %v, want docs/broken.pdf", got)
	}
	if got := fe["doc_type"]; got != "pdf" {
		t.Errorf("doc_type = %v, want pdf", got)
	}
	if got := fe["message"]; got != "extract: encrypted document" {
		t.Errorf("message = %v, want the extract error", got)
	}
	if got := fe["__level"]; got != "error" {
		t.Errorf("level = %v, want error", got)
	}
}

// TestNewFileSkipEmitter_EmitsReasonAtWarnLevel covers the `file_skip` half of
// the never-indexed partition: the event must name the file and carry a
// `skip_reasons` enum value, at level=warn per SPEC §3.2.
func TestNewFileSkipEmitter_EmitsReasonAtWarnLevel(t *testing.T) {
	var buf bytes.Buffer
	emitter := newNDJSONEmitter(&buf, true)

	newFileSkipEmitter(emitter)("notes/report.odt", "document", model.SkipReasonUnsupportedFormat)

	events := decodeNDJSON(t, buf.String())
	fs, ok := events["file_skip"]
	if !ok {
		t.Fatalf("no file_skip event emitted; got %q", buf.String())
	}
	if got := fs["rel_path"]; got != "notes/report.odt" {
		t.Errorf("rel_path = %v, want notes/report.odt", got)
	}
	if got := fs["doc_type"]; got != "document" {
		t.Errorf("doc_type = %v, want document", got)
	}
	if got := fs["reason"]; got != model.SkipReasonUnsupportedFormat {
		t.Errorf("reason = %v, want %v", got, model.SkipReasonUnsupportedFormat)
	}
	if got := fs["__level"]; got != "warn" {
		t.Errorf("level = %v, want warn (SPEC §3.2 enum is info|warn|error)", got)
	}
}

// notifierStub records what wireIngestorHooks registers on it.
type notifierStub struct {
	model.Ingestor
	errFn  func(relPath, docType, message string)
	skipFn func(relPath, docType, reason string)
}

func (s *notifierStub) SetOnDocumentError(fn func(relPath, docType, message string)) {
	s.errFn = fn
}

func (s *notifierStub) SetOnDocumentSkip(fn func(relPath, docType, reason string)) {
	s.skipFn = fn
}

func TestWireIngestorHooks_RegistersDocumentCallbacks(t *testing.T) {
	stub := &notifierStub{}
	errCalled, skipCalled := false, false
	wireIngestorHooks(stub, ingestorHooks{
		onDocError: func(string, string, string) { errCalled = true },
		onDocSkip:  func(string, string, string) { skipCalled = true },
	})

	if stub.errFn == nil || stub.skipFn == nil {
		t.Fatal("wireIngestorHooks did not register both document callbacks")
	}
	stub.errFn("a.pdf", "pdf", "boom")
	stub.skipFn("b.odt", "document", model.SkipReasonUnsupportedFormat)
	if !errCalled || !skipCalled {
		t.Fatalf("registered callbacks were not the ones passed in (err=%v skip=%v)", errCalled, skipCalled)
	}
}

func TestWireIngestorHooks_NilCallbacksAreNotRegistered(t *testing.T) {
	stub := &notifierStub{}
	wireIngestorHooks(stub, ingestorHooks{})
	if stub.errFn != nil || stub.skipFn != nil {
		t.Fatal("wireIngestorHooks registered a nil callback")
	}
}

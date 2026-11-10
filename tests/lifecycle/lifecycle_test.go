package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/lifecycle"
	"github.com/dirstral/dir2mcp/internal/model"
)

// The three functions below are verbatim copies of the rules as they stood
// before the typed states landed (store.normalizeStatus,
// store.normalizeEmbeddingStatus, mcp.normalizeFileStatus). The tests run the
// old and the new rule side by side, so any change of fold behaviour fails.

func oldNormalizeStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "skipped":
		return "skipped"
	case "error":
		return "error"
	case "secret_excluded":
		return "secret_excluded"
	case "pending":
		return "pending"
	default:
		return "ok"
	}
}

func oldNormalizeEmbeddingStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ok":
		return "ok"
	case "error":
		return "error"
	default:
		return "pending"
	}
}

func oldNormalizeFileStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "skipped", "secret_excluded":
		return "skipped"
	case "pending":
		return "pending"
	case "error":
		return "error"
	default:
		return "ok"
	}
}

// statusInputs covers every known value, case and space variants, the legacy
// "indexed" spelling, empty and garbage.
var statusInputs = []string{
	"", " ", "ok", "OK", " ok ", "skipped", "Skipped", " skipped\t", "error", "ERROR",
	"secret_excluded", "Secret_Excluded", "pending", "PENDING", " pending ",
	"indexed", "embedded", "deleted", "unknown", "ok ok", "skip", "errors", "\nerror\n",
}

func TestDocumentStatus_ConstantValues(t *testing.T) {
	want := map[model.DocumentStatus]string{
		model.DocumentStatusOK:             "ok",
		model.DocumentStatusSkipped:        "skipped",
		model.DocumentStatusError:          "error",
		model.DocumentStatusSecretExcluded: "secret_excluded",
		model.DocumentStatusPending:        "pending",
	}
	for c, s := range want {
		if string(c) != s {
			t.Errorf("constant %q, want %q", string(c), s)
		}
	}
	if len(model.DocumentLifecycle.States) != len(want) {
		t.Fatalf("DocumentLifecycle.States has %d states, want %d", len(model.DocumentLifecycle.States), len(want))
	}
	for _, s := range model.DocumentLifecycle.States {
		if _, ok := want[s]; !ok {
			t.Errorf("unexpected document state %q", s)
		}
	}
}

func TestEmbeddingStatus_ConstantValues(t *testing.T) {
	want := map[model.EmbeddingStatus]string{
		model.EmbeddingStatusOK:      "ok",
		model.EmbeddingStatusPending: "pending",
		model.EmbeddingStatusError:   "error",
	}
	for c, s := range want {
		if string(c) != s {
			t.Errorf("constant %q, want %q", string(c), s)
		}
	}
	if len(model.EmbeddingLifecycle.States) != len(want) {
		t.Fatalf("EmbeddingLifecycle.States has %d states, want %d", len(model.EmbeddingLifecycle.States), len(want))
	}
}

func TestNormalizeDocumentStatus_MatchesOldRule(t *testing.T) {
	for _, in := range statusInputs {
		got := string(model.NormalizeDocumentStatus(in))
		if want := oldNormalizeStatus(in); got != want {
			t.Errorf("NormalizeDocumentStatus(%q) = %q, old rule %q", in, got, want)
		}
		if !model.DocumentLifecycle.Valid(model.DocumentStatus(got)) {
			t.Errorf("NormalizeDocumentStatus(%q) = %q is not a table state", in, got)
		}
	}
}

func TestNormalizeEmbeddingStatus_MatchesOldRule(t *testing.T) {
	for _, in := range statusInputs {
		got := string(model.NormalizeEmbeddingStatus(in))
		if want := oldNormalizeEmbeddingStatus(in); got != want {
			t.Errorf("NormalizeEmbeddingStatus(%q) = %q, old rule %q", in, got, want)
		}
		if !model.EmbeddingLifecycle.Valid(model.EmbeddingStatus(got)) {
			t.Errorf("NormalizeEmbeddingStatus(%q) = %q is not a table state", in, got)
		}
	}
}

func TestListFilesStatus_MatchesOldProjection(t *testing.T) {
	enum := map[string]bool{}
	for _, v := range model.ListFilesStatusEnum {
		enum[v] = true
	}
	if got := strings.Join(model.ListFilesStatusEnum, ","); got != "ok,skipped,pending,error" {
		t.Fatalf("ListFilesStatusEnum = %s, want ok,skipped,pending,error", got)
	}
	for _, in := range statusInputs {
		got := model.ListFilesStatus(in)
		if want := oldNormalizeFileStatus(in); got != want {
			t.Errorf("ListFilesStatus(%q) = %q, old projection %q", in, got, want)
		}
		if !enum[got] {
			t.Errorf("ListFilesStatus(%q) = %q is outside the schema enum", in, got)
		}
	}
}

// TestLifecycleTables_WellFormed checks that every edge names a table state,
// that every state has an outgoing entry, and that the Via notes and the
// edges match one to one, so the generated doc has no blank or orphan rows.
func TestLifecycleTables_WellFormed(t *testing.T) {
	checkTable(t, model.DocumentLifecycle)
	checkTable(t, model.EmbeddingLifecycle)
}

func checkTable[S ~string](t *testing.T, tbl lifecycle.Table[S]) {
	t.Helper()
	listed := map[lifecycle.Edge]bool{}
	for _, e := range tbl.Transitions() {
		listed[e] = true
		if e.From != "" && !tbl.Valid(S(e.From)) {
			t.Errorf("%s: edge from unknown state %q", tbl.Name, e.From)
		}
		if !tbl.Valid(S(e.To)) {
			t.Errorf("%s: edge to unknown state %q", tbl.Name, e.To)
		}
		if tbl.Via[e] == "" {
			t.Errorf("%s: edge %q->%q has no Via note", tbl.Name, e.From, e.To)
		}
	}
	for e := range tbl.Via {
		if !listed[e] {
			t.Errorf("%s: Via note for unlisted edge %q->%q", tbl.Name, e.From, e.To)
		}
	}
	for _, s := range tbl.States {
		if _, ok := tbl.Allowed[s]; !ok {
			t.Errorf("%s: state %q has no Allowed entry", tbl.Name, s)
		}
	}
}

// TestDocumentLifecycle_WritersAreListed pins the writes the code makes today:
// every written status is reachable from a new row and from every state. A
// later PR that forbids an edge must change this test on purpose.
func TestDocumentLifecycle_WritersAreListed(t *testing.T) {
	tbl := model.DocumentLifecycle
	written := []model.DocumentStatus{
		model.DocumentStatusOK, model.DocumentStatusSkipped,
		model.DocumentStatusError, model.DocumentStatusSecretExcluded,
	}
	for _, to := range written {
		if !tbl.CanInitial(to) {
			t.Errorf("new row -> %q not listed", to)
		}
		for _, from := range tbl.States {
			if !tbl.CanTransition(from, to) {
				t.Errorf("%q -> %q not listed", from, to)
			}
		}
	}
	if tbl.CanInitial(model.DocumentStatusPending) {
		t.Errorf("new row -> pending is listed, but no writer produces pending")
	}
}

func TestEmbeddingLifecycle_WritersAreListed(t *testing.T) {
	tbl := model.EmbeddingLifecycle
	for _, to := range []model.EmbeddingStatus{model.EmbeddingStatusPending, model.EmbeddingStatusError} {
		if !tbl.CanInitial(to) {
			t.Errorf("new chunk -> %q not listed", to)
		}
	}
	if tbl.CanInitial(model.EmbeddingStatusOK) {
		t.Errorf("new chunk -> ok is listed, but chunks are never inserted embedded")
	}
	for _, from := range tbl.States {
		for _, to := range tbl.States {
			if !tbl.CanTransition(from, to) {
				t.Errorf("%q -> %q not listed (the UPDATE is unconditional)", from, to)
			}
		}
	}
}

func TestTable_GenericHelper(t *testing.T) {
	type st string
	tbl := lifecycle.Table[st]{
		Name:    "thing",
		States:  []st{"a", "b", "c"},
		Initial: []st{"a"},
		Allowed: map[st][]st{"a": {"b"}, "b": {"c", "a"}},
	}
	if !tbl.Valid("b") || tbl.Valid("z") {
		t.Fatalf("Valid wrong")
	}
	if !tbl.CanTransition("a", "b") || tbl.CanTransition("a", "c") || tbl.CanTransition("c", "a") {
		t.Fatalf("CanTransition wrong")
	}
	var got []string
	for _, e := range tbl.Transitions() {
		got = append(got, e.From+">"+e.To)
	}
	if strings.Join(got, ",") != ">a,a>b,b>a,b>c" {
		t.Fatalf("Transitions order = %v", got)
	}

	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	if !tbl.Observe(logf, "id1", "a", "b") || !tbl.Observe(logf, "id2", "", "a") {
		t.Fatalf("listed writes reported unlisted")
	}
	if len(lines) != 0 {
		t.Fatalf("listed writes logged: %v", lines)
	}
	if tbl.Observe(logf, "id3", "c", "a") || tbl.Observe(logf, "id4", "", "b") {
		t.Fatalf("unlisted writes reported listed")
	}
	want := []string{
		"lifecycle: unlisted transition thing id3 c->a",
		"lifecycle: unlisted transition thing id4 (new)->b",
	}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("log lines = %q, want %q", lines, want)
	}
	if tbl.Observe(nil, "id5", "c", "a") {
		t.Fatalf("nil logf: unlisted write reported listed")
	}
}

// TestStateMachinesDoc_NoDrift regenerates docs/state-machines.md into memory
// and fails when the committed file differs. Set
// DIR2MCP_UPDATE_STATE_MACHINES_DOC=1 to rewrite the file.
func TestStateMachinesDoc_NoDrift(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(here), "..", "..", "docs", "state-machines.md")
	want := model.StateMachinesMarkdown()
	if os.Getenv("DIR2MCP_UPDATE_STATE_MACHINES_DOC") == "1" {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("docs/state-machines.md is stale; run DIR2MCP_UPDATE_STATE_MACHINES_DOC=1 go test ./tests/lifecycle -run StateMachinesDoc")
	}
}

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/editorial"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// `dir2mcp annotate import` (SPEC §7.11.1), the command half of conformance
// case 1: validate the whole file, stage nothing on any invalid record, and
// never write meta.sqlite. All records are made up.

func runAnnotate(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	var code int
	withWorkingDir(t, dir, func() {
		code = app.RunWithContext(context.Background(), append([]string{"--state-dir", filepath.Join(dir, "state")}, args...))
	})
	return code, stdout.String(), stderr.String()
}

func writeImportFile(t *testing.T, dir string, lines ...string) string {
	t.Helper()
	path := filepath.Join(dir, "annotations.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validLine = `{"stem": "abc", "content_id": "C-1001", "match": {"score": 1.25, "asset_id": "A-7"}, "people": [{"name": "Пётр Ёлкин", "x": 1}]}`

func TestAnnotateImport_InvalidRecordStagesNothing(t *testing.T) {
	dir := testutil.TempDir(t)
	file := writeImportFile(t, dir, validLine, `{"stem": "def", "people": [{"role": "инженер"}]}`)
	code, _, stderr := runAnnotate(t, dir, "annotate", "import", file, "--source", "example-catalogue")
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "line 2:") {
		t.Fatalf("stderr does not name line 2:\n%s", stderr)
	}
	staged, err := editorial.ListStaged(filepath.Join(dir, "state"))
	if err != nil || len(staged) != 0 {
		t.Fatalf("an invalid file staged %v (err %v)", staged, err)
	}
}

func TestAnnotateImport_ValidFileStagesOneImportAndNoIndex(t *testing.T) {
	dir := testutil.TempDir(t)
	file := writeImportFile(t, dir, validLine)
	code, stdout, stderr := runAnnotate(t, dir, "--json", "annotate", "import", file, "--source", "example-catalogue")
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr)
	}
	stateDir := filepath.Join(dir, "state")
	staged, err := editorial.ListStaged(stateDir)
	if err != nil || len(staged) != 1 {
		t.Fatalf("staged = %v (err %v), want one import", staged, err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "meta.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("annotate import wrote meta.sqlite (stat err %v)", err)
	}
	var event struct {
		Level string `json:"level"`
		Event string `json:"event"`
		Data  struct {
			Source        string         `json:"source"`
			Key           string         `json:"key"`
			Records       int            `json:"records"`
			IgnoredFields map[string]int `json:"ignored_fields"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &event); err != nil {
		t.Fatalf("stdout is not one NDJSON event: %v\n%s", err, stdout)
	}
	if event.Event != "editorial_import_staged" || event.Data.Source != "example-catalogue" ||
		event.Data.Key != "stem" || event.Data.Records != 1 || event.Data.IgnoredFields["people[].x"] != 1 {
		t.Fatalf("event = %+v", event)
	}
}

func TestAnnotateImport_ReservedTopLevelFieldIsInvalid(t *testing.T) {
	dir := testutil.TempDir(t)
	file := writeImportFile(t, dir, `{"stem": "abc", "match_note": "x"}`)
	if code, _, stderr := runAnnotate(t, dir, "annotate", "import", file, "--source", "example-catalogue"); code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", code, stderr)
	}
}

func TestAnnotateImport_FlagErrors(t *testing.T) {
	dir := testutil.TempDir(t)
	file := writeImportFile(t, dir, validLine)
	for name, args := range map[string][]string{
		"no source":   {"annotate", "import", file},
		"bad key":     {"annotate", "import", file, "--source", "s", "--key", "name"},
		"no file":     {"annotate", "import", "--source", "s"},
		"no sub":      {"annotate"},
		"unknown sub": {"annotate", "export"},
	} {
		t.Run(name, func(t *testing.T) {
			if code, _, _ := runAnnotate(t, dir, args...); code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
		})
	}
}

package tests

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestRepoSplitBoundary_CLICommandSurface(t *testing.T) {
	root := repoRoot(t)
	appPath := filepath.Join(root, "internal", "cli", "app.go")

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, appPath, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", appPath, err)
	}

	commands := map[string]struct{}{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok || len(valueSpec.Names) != 1 || valueSpec.Names[0].Name != "commands" || len(valueSpec.Values) != 1 {
				continue
			}
			composite, ok := valueSpec.Values[0].(*ast.CompositeLit)
			if !ok {
				t.Fatalf("commands var is not a composite literal")
			}
			for _, elt := range composite.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.BasicLit)
				if !ok || key.Kind != token.STRING {
					continue
				}
				name := strings.Trim(key.Value, "\"")
				commands[name] = struct{}{}
			}
		}
	}

	expected := map[string]struct{}{
		"up":         {},
		"status":     {},
		"ask":        {},
		"search":     {},
		"open-file":  {},
		"list-files": {},
		"reindex":    {},
		"bridge":     {},
		"config":     {},
		"version":    {},
	}

	if len(commands) == 0 {
		t.Fatalf("failed to locate commands map in internal/cli/app.go")
	}
	if len(commands) != len(expected) {
		t.Fatalf("unexpected command surface size: got=%d want=%d commands=%v", len(commands), len(expected), mapKeys(commands))
	}
	for name := range expected {
		if _, ok := commands[name]; !ok {
			t.Fatalf("missing expected command %q; command surface must remain explicit during repo split", name)
		}
	}
	for name := range commands {
		if _, ok := expected[name]; !ok {
			t.Fatalf("unexpected command %q added to internal/cli command surface", name)
		}
	}
}

func TestRepoSplitBoundary_CLILegacyShimDocs(t *testing.T) {
	root := repoRoot(t)
	readmePath := filepath.Join(root, "README.md")
	appPath := filepath.Join(root, "internal", "cli", "app.go")

	readmeRaw, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("read %s: %v", readmePath, err)
	}
	appRaw, err := os.ReadFile(appPath)
	if err != nil {
		t.Fatalf("read %s: %v", appPath, err)
	}

	readme := string(readmeRaw)
	app := string(appRaw)

	if !strings.Contains(readme, "Legacy compatibility shim; prefer `dirstral-cli` for client UX") {
		t.Fatalf("README must mark ask as a legacy compatibility shim to preserve repo split boundary guidance")
	}
	if !strings.Contains(readme, "new client/orchestrator UX belongs in `dirstral-cli`") {
		t.Fatalf("README must direct new client/orchestrator UX to dirstral-cli")
	}
	for _, row := range []string{
		"| `ask \"<question>\"` | Legacy compatibility shim; prefer `dirstral-cli` for client UX |",
		"| `search \"<query>\"` | Legacy compatibility shim; prefer `dirstral-cli` for client UX |",
		"| `open-file <rel-path>` | Legacy compatibility shim; prefer `dirstral-cli` for client UX |",
		"| `list-files` | Legacy compatibility shim; prefer `dirstral-cli` for client UX |",
	} {
		if !strings.Contains(readme, row) {
			t.Fatalf("README CLI command table must include legacy shim row: %s", row)
		}
	}
	for _, cmd := range []string{"ask", "search", "open-file", "list-files"} {
		want := "legacy compatibility shim; prefer dirstral-cli for client UX"
		if !strings.Contains(app, want) {
			t.Fatalf("usage text must mark %q command as legacy compatibility shim", cmd)
		}
	}
}

func TestRepoSplitBoundary_InternalCLIFileOwnership(t *testing.T) {
	root := repoRoot(t)
	cliDir := filepath.Join(root, "internal", "cli")
	entries, err := os.ReadDir(cliDir)
	if err != nil {
		t.Fatalf("read internal/cli: %v", err)
	}

	allowed := map[string]struct{}{
		"app.go":                  {},
		"ask.go":                  {},
		"bridge.go":               {},
		"config_cmd.go":           {},
		"corpus_snapshot_test.go": {},
		"corpus_writer_test.go":   {},
		"embed_options_test.go":   {},
		"reindex.go":              {},
		"remote_commands.go":      {},
		"status.go":               {},
		"style.go":                {},
		"up.go":                   {},
		"version.go":              {},
	}

	var unexpected []string
	for _, entry := range entries {
		if entry.IsDir() {
			unexpected = append(unexpected, entry.Name()+"/")
			continue
		}
		if _, ok := allowed[entry.Name()]; !ok {
			unexpected = append(unexpected, entry.Name())
		}
	}
	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		t.Fatalf("unexpected internal/cli files (possible boundary drift): %v", unexpected)
	}
}

func mapKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

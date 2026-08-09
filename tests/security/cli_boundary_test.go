package tests

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

	commands := extractCommandsMap(t, file)

	expected := map[string]struct{}{
		"up":             {},
		"down":           {},
		"status":         {},
		"ask":            {},
		"search":         {},
		"open-file":      {},
		"list-files":     {},
		"reindex":        {},
		"embed-worker":   {},
		"export":         {},
		"bridge":         {},
		"config":         {},
		"install":        {},
		"uninstall":      {},
		"doctor":         {},
		"print-config":   {},
		"support-bundle": {},
		"service":        {},
		"version":        {},
	}

	assertCommandSurface(t, commands, expected)
}

func extractCommandsMap(t *testing.T, file *ast.File) map[string]struct{} {
	t.Helper()
	commands := map[string]struct{}{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			collectCommandsFromSpec(t, spec, commands)
		}
	}
	return commands
}

func collectCommandsFromSpec(t *testing.T, spec ast.Spec, commands map[string]struct{}) {
	t.Helper()
	valueSpec, ok := spec.(*ast.ValueSpec)
	if !ok || len(valueSpec.Names) != 1 || valueSpec.Names[0].Name != "commands" || len(valueSpec.Values) != 1 {
		return
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
		name, err := strconv.Unquote(key.Value)
		if err != nil {
			t.Fatalf("decode command key literal %q: %v", key.Value, err)
		}
		commands[name] = struct{}{}
	}
}

func assertCommandSurface(t *testing.T, commands, expected map[string]struct{}) {
	t.Helper()
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
		want := "{\"" + cmd + "\", \"legacy compatibility shim; prefer dirstral-cli for client UX\"}"
		if !strings.Contains(app, want) {
			t.Fatalf("usage text must mark %q command as legacy compatibility shim with explicit command entry", cmd)
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
		"app.go":                                 {},
		"ask.go":                                 {},
		"bridge.go":                              {},
		"claude_cmd.go":                          {},
		"client_cmds.go":                         {},
		"config_cmd.go":                          {},
		"corpus_snapshot_test.go":                {},
		"corpus_writer_test.go":                  {},
		"daemon.go":                              {},
		"daemon_other.go":                        {},
		"daemon_unix.go":                         {},
		"down.go":                                {},
		"down_internal_test.go":                  {},
		"embed_options_test.go":                  {},
		"embed_preflight_probe_internal_test.go": {},
		"embed_worker.go":                        {},
		"error_codes_test.go":                    {},
		"export.go":                              {},
		"export_reflow_internal_test.go":         {},
		"export_ttml.go":                         {},
		"flag_ordering_test.go":                  {},
		"ndjson_events_test.go":                  {},
		"pididentity_darwin.go":                  {},
		"pididentity_linux.go":                   {},
		"pididentity_other.go":                   {},
		"reindex.go":                             {},
		"registration_hint.go":                   {},
		"registration_hint_test.go":              {},
		"remote_commands.go":                     {},
		"retry_embeddings.go":                    {},
		"retry_embeddings_internal_test.go":      {},
		"rerank_selection_test.go":               {},
		"routing_decision.go":                    {},
		"server_doctor.go":                       {},
		"server_doctor_coverage_test.go":         {},
		"server_doctor_egress_test.go":           {},
		"server_log_tee_internal_test.go":        {},
		"service.go":                             {},
		"service_darwin.go":                      {},
		"service_darwin_test.go":                 {},
		"service_linux.go":                       {},
		"service_linux_test.go":                  {},
		"service_other.go":                       {},
		"service_test.go":                        {},
		"status.go":                              {},
		"status_coverage_test.go":                {},
		"style.go":                               {},
		"support_bundle.go":                      {},
		"support_bundle_internal_test.go":        {},
		"support_bundle_redact.go":               {},
		"syncwriter.go":                          {},
		"syncwriter_test.go":                     {},
		"up.go":                                  {},
		"up_daemon.go":                           {},
		"up_distributed_embed.go":                {},
		"up_graceful_stop_test.go":               {},
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

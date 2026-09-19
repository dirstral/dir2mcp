package conformance

// dir2mcp_list_files carries two optional fields that no conformance test
// covered (dirstral-conformance#3):
//
//   - `include_hidden` (INPUT). It shipped in the published input schema and in
//     no prose at all, so a reader of the spec could not tell what it does, and
//     nothing at the tool boundary proved that the server honours it.
//   - `files[].title` (OUTPUT). The server emitted it on every document that has
//     a title, while every canonical copy of the output object closed itself
//     with `additionalProperties: false` and declared no such property. A client
//     that validated against the canonical schema therefore rejected the whole
//     response for any corpus with a titled document (the #850 defect class).
//
// dirstral-spec#76 fixed both on the spec side. These tests pin the behaviour,
// on the schema inside the dirstral-spec submodule rather than on a copy, so a
// spec-side change reaches them through the submodule pin (the
// tests/conformance/stats_canonical_schema_850_test.go idiom).
//
// The two schema tests alone would pass on a server that publishes
// `include_hidden` and then ignores it, and on one that never emits a title. So
// each is paired with a test that calls the tool over the production transport
// against a real corpus and reads what comes back on the wire.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/store"
)

// canonicalListFilesSchemaPath is the pinned canonical schema inside the
// dirstral-spec submodule.
var canonicalListFilesSchemaPath = filepath.Join("..", "..", "dirstral-spec", "spec", "tools", "schemas", "list_files.json")

// canonicalListFilesSection returns the named top-level subschema ("input" or
// "output") of the canonical list_files.json as a generic tree.
func canonicalListFilesSection(t *testing.T, section string) map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(canonicalListFilesSchemaPath)
	if err != nil {
		t.Fatalf("read canonical list_files.json (run: git submodule update --init): %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode canonical list_files.json: %v", err)
	}
	part, ok := doc[section]
	if !ok {
		t.Fatalf("canonical list_files.json declares no %q subschema", section)
	}
	var tree map[string]interface{}
	if err := json.Unmarshal(part, &tree); err != nil {
		t.Fatalf("decode canonical list_files %s subschema: %v", section, err)
	}
	return tree
}

// schemaChild returns a named child object of a schema node, failing the test
// when it is missing or is not an object.
func schemaChild(t *testing.T, node map[string]interface{}, key, label string) map[string]interface{} {
	t.Helper()
	child, ok := node[key].(map[string]interface{})
	if !ok {
		t.Fatalf("%s declares no %q object: %#v", label, key, node[key])
	}
	return child
}

// fileEntrySchema returns the `files[]` item subschema of a list_files output
// schema, canonical or served. That node, not the output root, is where
// `title` lives.
func fileEntrySchema(t *testing.T, output map[string]interface{}, label string) map[string]interface{} {
	t.Helper()
	properties := schemaChild(t, output, "properties", label)
	files := schemaChild(t, properties, "files", label+" properties")
	return schemaChild(t, files, "items", label+" files")
}

// servedListFilesInputSchema returns the inputSchema the server publishes for
// dir2mcp_list_files through tools/list.
func servedListFilesInputSchema(t *testing.T) map[string]interface{} {
	t.Helper()
	cfg := defaultConfig()
	srv := newServer(t, cfg)
	defer srv.Close()

	mcpURL := srv.URL + cfg.MCPPath
	schema, ok := toolInputSchemas(t, mcpURL, initSession(t, mcpURL))[protocol.ToolNameListFiles]
	if !ok {
		t.Fatalf("tools/list advertises no inputSchema for %s", protocol.ToolNameListFiles)
	}
	return schema
}

// TestListFiles_IncludeHiddenIsDeclaredAndAgreesWithCanonical_76 pins the input
// half of the contract. `include_hidden` must be declared on both sides, with
// the same type and the same default, and the whole input object must carry the
// same property set as the canonical file.
//
// The exact property-set comparison is what makes this a contract test rather
// than a spot check: the object is closed on both sides, so a served-only
// property is a field no canonical client may send, and a canonical-only
// property is one the server refuses.
func TestListFiles_IncludeHiddenIsDeclaredAndAgreesWithCanonical_76(t *testing.T) {
	t.Parallel()
	canonical := canonicalListFilesSection(t, "input")
	served := servedListFilesInputSchema(t)

	assertClosedObject(t, canonical, "canonical list_files input")
	assertClosedObject(t, served, "served list_files inputSchema")

	wantProps := schemaPropertyNames(t, canonical, "canonical list_files input")
	gotProps := schemaPropertyNames(t, served, "served list_files inputSchema")
	if !reflect.DeepEqual(gotProps, wantProps) {
		t.Fatalf("served list_files input properties = %v, canonical = %v (dirstral-conformance#3)", gotProps, wantProps)
	}
	assertContains(t, gotProps, []string{"include_hidden"}, "served list_files input properties")

	canonicalField := schemaChild(t, schemaChild(t, canonical, "properties", "canonical list_files input"),
		"include_hidden", "canonical list_files input properties")
	servedField := schemaChild(t, schemaChild(t, served, "properties", "served list_files inputSchema"),
		"include_hidden", "served list_files inputSchema properties")

	if canonicalField["type"] != "boolean" {
		t.Fatalf("canonical include_hidden type = %#v, want \"boolean\"", canonicalField["type"])
	}
	if servedField["type"] != canonicalField["type"] {
		t.Fatalf("served include_hidden type = %#v, canonical = %#v", servedField["type"], canonicalField["type"])
	}
	if canonicalField["default"] != false {
		t.Fatalf("canonical include_hidden default = %#v, want false", canonicalField["default"])
	}
	if servedField["default"] != canonicalField["default"] {
		t.Fatalf("served include_hidden default = %#v, canonical = %#v", servedField["default"], canonicalField["default"])
	}
}

// TestListFiles_FileTitleIsDeclaredAndAgreesWithCanonical_76 pins the output
// half. `title` must be declared on the `files[]` entry on both sides and must
// stay OPTIONAL on both: the spec says it is present only when the document has
// a non-empty title, so requiring it would condemn every untitled document.
//
// The entry object is closed, which is the whole reason this matters: before
// dirstral-spec#76 the canonical entry declared no `title`, so a canonically
// validating client rejected any response that carried one.
func TestListFiles_FileTitleIsDeclaredAndAgreesWithCanonical_76(t *testing.T) {
	t.Parallel()
	canonicalEntry := fileEntrySchema(t, canonicalListFilesSection(t, "output"), "canonical list_files output")
	servedEntry := fileEntrySchema(t, servedOutputSchema(t, protocol.ToolNameListFiles), "served list_files outputSchema")

	assertClosedObject(t, canonicalEntry, "canonical list_files files[] entry")
	assertClosedObject(t, servedEntry, "served list_files files[] entry")

	wantProps := schemaPropertyNames(t, canonicalEntry, "canonical list_files files[] entry")
	gotProps := schemaPropertyNames(t, servedEntry, "served list_files files[] entry")
	if !reflect.DeepEqual(gotProps, wantProps) {
		t.Fatalf("served list_files files[] properties = %v, canonical = %v (dirstral-conformance#3)", gotProps, wantProps)
	}
	assertContains(t, gotProps, []string{"title"}, "served list_files files[] properties")

	wantRequired := schemaRequired(t, canonicalEntry, "canonical list_files files[] entry")
	gotRequired := schemaRequired(t, servedEntry, "served list_files files[] entry")
	if !reflect.DeepEqual(gotRequired, wantRequired) {
		t.Fatalf("served list_files files[] required = %v, canonical = %v (dirstral-conformance#3)", gotRequired, wantRequired)
	}
	assertAbsent(t, wantRequired, []string{"title"}, "canonical list_files files[] required")

	titleField := schemaChild(t, schemaChild(t, canonicalEntry, "properties", "canonical list_files files[] entry"),
		"title", "canonical list_files files[] properties")
	if titleField["type"] != "string" {
		t.Fatalf("canonical files[].title type = %#v, want \"string\"", titleField["type"])
	}
}

// --- behaviour ------------------------------------------------------------

// listFilesCorpus76 is one document of the behaviour fixture.
type listFilesCorpus76 struct {
	relPath string
	title   string
}

// seedListFilesCorpus76 writes real files plus their store rows and returns a
// server wired the production way.
//
// Real files are mandatory. list_files resolves every row of the page against
// the corpus root and drops what is not there (#176), so a store-only fixture
// returns an empty listing and every assertion below would pass vacuously
// against a server that ignores include_hidden entirely.
func seedListFilesCorpus76(t *testing.T, docs []listFilesCorpus76) (*runningServer, config.Config) {
	t.Helper()
	tmp := t.TempDir()
	st := store.NewSQLiteStore(filepath.Join(tmp, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	root := filepath.Join(tmp, "corpus")
	for i, doc := range docs {
		full := filepath.Join(root, filepath.FromSlash(doc.relPath))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("mkdir for %s: %v", doc.relPath, err)
		}
		if err := os.WriteFile(full, []byte("body"), 0o600); err != nil {
			t.Fatalf("write %s: %v", doc.relPath, err)
		}
		row := model.Document{
			RelPath:   doc.relPath,
			DocType:   "md",
			Title:     doc.title,
			SizeBytes: 4,
			MTimeUnix: int64(1700000000 + i),
			Status:    "ok",
		}
		if err := st.UpsertDocument(context.Background(), row); err != nil {
			t.Fatalf("seed %s: %v", doc.relPath, err)
		}
	}

	cfg := defaultConfig()
	cfg.RootDir = root
	cfg.StateDir = tmp
	srv := newServerWithRetriever(t, cfg, nil, mcp.WithStore(st))
	t.Cleanup(srv.Close)
	return srv, cfg
}

// callListFiles76 calls dir2mcp_list_files over the production transport with
// the given raw argument object and returns the structuredContent payload a
// real client validates.
func callListFiles76(t *testing.T, srv *runningServer, cfg config.Config, argsJSON string) map[string]interface{} {
	t.Helper()
	mcpURL := srv.URL + cfg.MCPPath
	sid := initSession(t, mcpURL)
	body := `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"` +
		protocol.ToolNameListFiles + `","arguments":` + argsJSON + `}}`
	resp := sendRPC(t, mcpURL, sid, body, nil)
	payload := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list_files: status=%d want=200 body=%s", resp.StatusCode, payload)
	}
	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("list_files: decode: %v body=%s", err, payload)
	}
	if envelope.Result.IsError {
		t.Fatalf("list_files(%s): isError=true body=%s", argsJSON, payload)
	}
	if envelope.Result.StructuredContent == nil {
		t.Fatalf("list_files: missing structuredContent body=%s", payload)
	}
	assertCanonicalListFiles76(t, envelope.Result.StructuredContent)
	return envelope.Result.StructuredContent
}

// assertCanonicalListFiles76 validates a whole list_files payload against the
// canonical output subschema. The object and its `files[]` entries are closed,
// so this is what proves an emitted `title` is legal for a strict client rather
// than merely present.
func assertCanonicalListFiles76(t *testing.T, structured map[string]interface{}) {
	t.Helper()
	raw, err := json.Marshal(canonicalListFilesSection(t, "output"))
	if err != nil {
		t.Fatalf("re-encode canonical list_files output subschema: %v", err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decode canonical list_files output subschema: %v", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve canonical list_files output subschema: %v", err)
	}
	if err := resolved.Validate(structured); err != nil {
		pretty, _ := json.MarshalIndent(structured, "", "  ")
		t.Fatalf("dir2mcp_list_files payload is invalid against the canonical list_files.json: %v\npayload:\n%s", err, pretty)
	}
}

// listedRelPaths76 returns the sorted rel_path set of a list_files payload.
func listedRelPaths76(t *testing.T, structured map[string]interface{}) []string {
	t.Helper()
	files, ok := structured["files"].([]interface{})
	if !ok {
		t.Fatalf("list_files payload carries no files array: %#v", structured["files"])
	}
	out := make([]string, 0, len(files))
	for _, raw := range files {
		entry, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("files[] entry is not an object: %#v", raw)
		}
		relPath, ok := entry["rel_path"].(string)
		if !ok {
			t.Fatalf("files[] entry carries no rel_path string: %#v", entry)
		}
		out = append(out, relPath)
	}
	sort.Strings(out)
	return out
}

// listedTotal76 returns the payload's `total`.
func listedTotal76(t *testing.T, structured map[string]interface{}) int {
	t.Helper()
	total, ok := structured["total"].(float64)
	if !ok {
		t.Fatalf("list_files payload carries no numeric total: %#v", structured["total"])
	}
	return int(total)
}

// TestListFiles_IncludeHiddenChangesWhatComesBack_76 is the half that matters.
// A schema test passes on a server that publishes `include_hidden` and then
// drops it on the floor, so this one asks for the same corpus three ways and
// requires the listing to differ.
//
// Both directions are asserted positively: the dotfile is PRESENT when asked
// for and ABSENT when not, and the visible document is present in every case.
// An empty listing therefore fails rather than passing silently.
func TestListFiles_IncludeHiddenChangesWhatComesBack_76(t *testing.T) {
	t.Parallel()
	srv, cfg := seedListFilesCorpus76(t, []listFilesCorpus76{
		{relPath: "visible.md"},
		{relPath: ".hidden.md"},
		{relPath: "notes/.private/secret.md"},
	})

	visibleOnly := []string{"visible.md"}
	everything := []string{".hidden.md", "notes/.private/secret.md", "visible.md"}

	cases := []struct {
		label string
		args  string
		want  []string
	}{
		{"include_hidden=true", `{"limit":50,"offset":0,"include_hidden":true}`, everything},
		{"include_hidden=false", `{"limit":50,"offset":0,"include_hidden":false}`, visibleOnly},
		{"include_hidden absent", `{"limit":50,"offset":0}`, visibleOnly},
	}
	for _, tc := range cases {
		got := callListFiles76(t, srv, cfg, tc.args)
		relPaths := listedRelPaths76(t, got)
		if !reflect.DeepEqual(relPaths, tc.want) {
			t.Fatalf("%s listed %v, want %v; the server does not honour the include_hidden it advertises (dirstral-conformance#3)",
				tc.label, relPaths, tc.want)
		}
		if total := listedTotal76(t, got); total != len(tc.want) {
			t.Fatalf("%s total=%d want %d; the total must describe the same set the page is drawn from",
				tc.label, total, len(tc.want))
		}
	}
}

// TestListFiles_TitleIsOnTheWireOnlyForATitledDocument_76 is the behaviour half
// of the output contract. A schema test passes on a server that never emits a
// title at all, and a presence-only test passes on one that invents a title for
// every document (echoing the filename, say). So this asserts both: the titled
// document carries EXACTLY its stored title, and the untitled one carries no
// title at all.
func TestListFiles_TitleIsOnTheWireOnlyForATitledDocument_76(t *testing.T) {
	t.Parallel()
	const wantTitle = "Quarterly Revenue Report"
	srv, cfg := seedListFilesCorpus76(t, []listFilesCorpus76{
		{relPath: "titled.md", title: wantTitle},
		{relPath: "untitled.md"},
	})

	structured := callListFiles76(t, srv, cfg, `{"limit":50,"offset":0}`)
	entries := fileEntriesByRelPath76(t, structured)

	titled, ok := entries["titled.md"]
	if !ok {
		t.Fatalf("titled.md is missing from the listing, so the title assertion would be vacuous: %v", listedRelPaths76(t, structured))
	}
	gotTitle, present := titled["title"]
	if !present {
		t.Fatalf("titled.md carries no title on the wire, but the document has one (dirstral-conformance#3): %#v", titled)
	}
	if gotTitle != wantTitle {
		t.Fatalf("titled.md title = %#v, want %q", gotTitle, wantTitle)
	}

	untitled, ok := entries["untitled.md"]
	if !ok {
		t.Fatalf("untitled.md is missing from the listing: %v", listedRelPaths76(t, structured))
	}
	// Absent is the contract; an empty string is tolerated as the same
	// statement. An invented title is not.
	if raw, present := untitled["title"]; present && raw != "" {
		t.Fatalf("untitled.md carries title %#v, but the document has none; a title must be reported, not invented", raw)
	}
}

// fileEntriesByRelPath76 indexes a list_files payload's entries by rel_path.
func fileEntriesByRelPath76(t *testing.T, structured map[string]interface{}) map[string]map[string]interface{} {
	t.Helper()
	files, ok := structured["files"].([]interface{})
	if !ok {
		t.Fatalf("list_files payload carries no files array: %#v", structured["files"])
	}
	out := make(map[string]map[string]interface{}, len(files))
	for _, raw := range files {
		entry, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("files[] entry is not an object: %#v", raw)
		}
		relPath, ok := entry["rel_path"].(string)
		if !ok {
			t.Fatalf("files[] entry carries no rel_path string: %#v", entry)
		}
		out[relPath] = entry
	}
	return out
}

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

// This file is the guard that keeps issue #670 from happening again.
//
// Issue #416 hardened the provider HTTP clients: it refused redirects, which
// carry a custom API-key header to the redirect target, and it capped a
// success body before the decode, which an endless 2xx response can otherwise
// use to exhaust memory. That fix was copied into each adapter package, so
// adapters written later (anthropic, cohere, colbertrerank, whisperapi) simply
// did not have it.
//
// internal/providerhttp now holds the one hardened path. The rules below make
// a new adapter safe by construction: an adapter that builds its own client,
// or reads a body without the cap, fails this test in CI.

// providerHTTPHelperPackage is the shared package that every adapter uses.
const providerHTTPHelperPackage = "providerhttp"

// providerAdapterFile is one parsed non-test file of a provider adapter
// package.
type providerAdapterFile struct {
	path string
	fset *token.FileSet
	file *ast.File
}

// TestProviderAdaptersUseTheSharedHardenedClient checks the three rules.
func TestProviderAdaptersUseTheSharedHardenedClient(t *testing.T) {
	files := providerAdapterFiles(t)
	if len(files) == 0 {
		t.Fatal("found no provider adapter files to check")
	}
	seenPackages := map[string]struct{}{}
	for _, f := range files {
		seenPackages[filepath.Base(filepath.Dir(f.path))] = struct{}{}
	}
	// A short sanity list. It proves the discovery rule still finds the
	// adapters named in issue #670.
	for _, want := range []string{"anthropic", "cohere", "colbertrerank", "elevenlabs", "gemini", "mistral", "omniembed", "openai", "whisperapi"} {
		if _, ok := seenPackages[want]; !ok {
			t.Fatalf("adapter package %q was not discovered; the discovery rule needs an update", want)
		}
	}

	for _, f := range files {
		checkClientLiterals(t, f)
		checkUnboundedBodyReads(t, f)
		checkRoundTrips(t, f)
	}
}

// checkClientLiterals enforces rule 1: an http.Client built inside an adapter
// must carry a redirect policy.
func checkClientLiterals(t *testing.T, f providerAdapterFile) {
	t.Helper()
	ast.Inspect(f.file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !isSelector(lit.Type, "http", "Client") {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "CheckRedirect" {
				return true
			}
		}
		t.Errorf("%s: an http.Client literal has no CheckRedirect policy. Use %s.NewClient, %s.WithTimeout or %s.ClientOrDefault, so a custom API-key header cannot follow a redirect to another host (issue #670).",
			f.position(lit.Pos()), providerHTTPHelperPackage, providerHTTPHelperPackage, providerHTTPHelperPackage)
		return true
	})
}

// checkUnboundedBodyReads enforces rule 2: a response body must go through the
// shared capped reader before it is decoded.
func checkUnboundedBodyReads(t *testing.T, f providerAdapterFile) {
	t.Helper()
	ast.Inspect(f.file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		if !isSelector(call.Fun, "json", "NewDecoder") && !isSelector(call.Fun, "io", "ReadAll") {
			return true
		}
		arg, ok := call.Args[0].(*ast.SelectorExpr)
		if !ok || arg.Sel.Name != "Body" {
			return true
		}
		t.Errorf("%s: a response body is read with no size cap. Use %s.ReadLimitedJSONBody (or ReadLimitedBody) so an endless 2xx response cannot exhaust memory (issue #670).",
			f.position(call.Pos()), providerHTTPHelperPackage)
		return true
	})
}

// checkRoundTrips enforces rule 3: an adapter must get its client from the
// shared package, not from a field or a local variable of its own.
func checkRoundTrips(t *testing.T, f providerAdapterFile) {
	t.Helper()
	ast.Inspect(f.file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Do" || !looksLikeHTTPClient(sel.X) {
			return true
		}
		if isProviderHTTPCall(sel.X) {
			return true
		}
		t.Errorf("%s: an HTTP round trip uses a client that did not come from %s. Call %s.ClientOrDefault or %s.WithTimeout at the call site, so the redirect policy is always installed (issue #670).",
			f.position(call.Pos()), providerHTTPHelperPackage, providerHTTPHelperPackage, providerHTTPHelperPackage)
		return true
	})
}

// looksLikeHTTPClient reports whether expr names something that reads like an
// HTTP client. It keeps the rule off unrelated Do methods, for example
// sync.Once.Do.
func looksLikeHTTPClient(expr ast.Expr) bool {
	switch v := expr.(type) {
	case *ast.Ident:
		return strings.Contains(strings.ToLower(v.Name), "client")
	case *ast.SelectorExpr:
		return strings.Contains(strings.ToLower(v.Sel.Name), "client")
	case *ast.CallExpr:
		return true
	}
	return false
}

// isProviderHTTPCall reports whether expr is a call into the shared package,
// for example providerhttp.WithTimeout(c.HTTPClient, timeout).
func isProviderHTTPCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == providerHTTPHelperPackage
}

func isSelector(expr ast.Expr, pkg, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	if star, ok := sel.X.(*ast.Ident); ok {
		return star.Name == pkg
	}
	return false
}

func (f providerAdapterFile) position(pos token.Pos) string {
	p := f.fset.Position(pos)
	return filepath.Base(filepath.Dir(f.path)) + "/" + filepath.Base(f.path) + ":" + strconv.Itoa(p.Line)
}

// providerAdapterFiles finds every non-test file of every provider adapter
// package. A provider adapter package is a package under internal/ that speaks
// HTTP (it imports net/http) and reports provider failures (it uses
// model.ProviderError). The rule is structural, so a new adapter is covered on
// the day it is added. No allowlist can go stale.
func providerAdapterFiles(t *testing.T) []providerAdapterFile {
	t.Helper()
	internalDir := filepath.Join(repoRoot(t), "internal")
	entries, err := os.ReadDir(internalDir)
	if err != nil {
		t.Fatalf("read internal/: %v", err)
	}
	var out []providerAdapterFile
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pkgDir := filepath.Join(internalDir, entry.Name())
		parsed := parsePackageFiles(t, pkgDir)
		if isProviderAdapterPackage(parsed) {
			out = append(out, parsed...)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

func parsePackageFiles(t *testing.T, pkgDir string) []providerAdapterFile {
	t.Helper()
	names, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatalf("read %s: %v", pkgDir, err)
	}
	var out []providerAdapterFile
	for _, name := range names {
		if name.IsDir() || !strings.HasSuffix(name.Name(), ".go") || strings.HasSuffix(name.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(pkgDir, name.Name())
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		out = append(out, providerAdapterFile{path: path, fset: fset, file: file})
	}
	return out
}

func isProviderAdapterPackage(files []providerAdapterFile) bool {
	var speaksHTTP, reportsProviderErrors bool
	for _, f := range files {
		for _, imp := range f.file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err == nil && path == "net/http" {
				speaksHTTP = true
			}
		}
		ast.Inspect(f.file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && isSelector(sel, "model", "ProviderError") {
				reportsProviderErrors = true
			}
			return true
		})
	}
	return speaksHTTP && reportsProviderErrors
}

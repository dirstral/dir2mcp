package mcp

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// TestRenderSearchHitsTextIncludesSnippets guards that the search / search-only
// content text carries each hit's document and snippet, not just a count. A bare
// "found N result(s)" (the old behavior) left a model that reads the content
// block unable to see or cite the matches — it looked like search "returned only
// counts" even though structuredContent had the data.
func TestRenderSearchHitsTextIncludesSnippets(t *testing.T) {
	out := renderSearchHitsText([]model.SearchHit{
		{RelPath: "a.pdf", Title: "Act A", Score: 0.5, Snippet: "the operative clause text"},
	}, "result")
	for _, want := range []string{"found 1 result(s)", "Act A", "a.pdf", "the operative clause text"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered search text missing %q:\n%s", want, out)
		}
	}
	if got := renderSearchHitsText(nil, "result"); got != "found 0 result(s)" {
		t.Errorf("empty hits: got %q, want %q", got, "found 0 result(s)")
	}
}

// TestSerializeHitKeysDeclaredInSchema guards against serializer/outputSchema
// drift on search/ask hits (issue #387). The hit object is
// additionalProperties:false, so any key serializeHit emits that the schema does
// not declare makes a strict MCP client (Claude Desktop validates
// structuredContent against outputSchema) reject the whole result with "Failed
// to call tool". A media/multimodal hit exercises the conditional fields
// (modality, media_ref) that were previously missing from the schema.
func TestSerializeHitKeysDeclaredInSchema(t *testing.T) {
	hit := model.SearchHit{
		ChunkID: 1, RelPath: "a.pdf", Title: "A", DocType: "pdf", RepType: "extracted_markdown",
		Score: 0.5, Snippet: "x",
		Span:     model.Span{Kind: "region", Region: &model.RegionSpan{StartPage: 1, EndPage: 1}},
		Modality: "text", MediaRef: "a.pdf",
	}
	out := serializeHit(hit)
	props, ok := hitDefinitionSchema()["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("hitDefinitionSchema has no properties map")
	}
	for k := range out {
		if _, declared := props[k]; !declared {
			t.Errorf("serializeHit emits %q but hitDefinitionSchema does not declare it "+
				"(additionalProperties:false → strict MCP clients reject the result)", k)
		}
	}
}

// TestLooksLikeBinaryContent exercises the heuristic upgrade landed for
// PR #180: NUL-byte detection alone misses binary formats (mp3 frames, some
// pdf byte ranges) that contain no NULs but plenty of non-text bytes. The
// strengthened check rejects such payloads via UTF-8 validation plus a
// non-whitespace control-character ratio, while leaving real text alone.
func TestLooksLikeBinaryContent(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "empty",
			content: "",
			want:    false,
		},
		{
			name:    "pure ascii prose",
			content: "Hello, world! This is plain ASCII text. The quick brown fox jumps over the lazy dog.",
			want:    false,
		},
		{
			name:    "valid utf-8 with emoji",
			content: "résumé — naïve façade 🚀 — Привет, мир. これはテストです。",
			want:    false,
		},
		{
			name:    "whitespace only including \\t \\n \\r",
			content: "\t\n\r\n\t  \n",
			want:    false,
		},
		{
			name:    "ascii markdown with code fences",
			content: "# Title\n\n```go\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n```\n",
			want:    false,
		},
		{
			name:    "embedded NUL byte",
			content: "abc\x00def",
			want:    true,
		},
		{
			name:    "invalid utf-8 (lone continuation byte)",
			content: "ok then \x80\x81 not utf8",
			want:    true,
		},
		{
			name: "high control-character ratio",
			// no NULs, valid UTF-8, but >30% non-whitespace control bytes.
			content: strings.Repeat("\x01\x02\x03\x04\x05x", 50),
			want:    true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeBinaryContent(tc.content); got != tc.want {
				t.Fatalf("looksLikeBinaryContent(%q...) = %v; want %v", preview(tc.content), got, tc.want)
			}
		})
	}
}

// TestBinaryContentMessageForDocType verifies the DOC_TYPE_UNSUPPORTED message
// is tailored to the inferred doc_type instead of always pointing at audio /
// transcribe.
func TestBinaryContentMessageForDocType(t *testing.T) {
	cases := []struct {
		docType     string
		mustHave    []string
		mustNotHave []string
	}{
		{docType: "audio", mustHave: []string{"transcribe"}},
		{docType: "pdf", mustHave: []string{"page="}, mustNotHave: []string{"transcribe"}},
		{docType: "md", mustNotHave: []string{"transcribe", "page="}},
		{docType: "unknown", mustNotHave: []string{"transcribe", "page="}},
		{docType: "", mustNotHave: []string{"transcribe", "page="}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run("doc_type="+tc.docType, func(t *testing.T) {
			msg := binaryContentMessageForDocType(tc.docType)
			for _, want := range tc.mustHave {
				if !strings.Contains(msg, want) {
					t.Fatalf("doc_type=%q message %q missing %q", tc.docType, msg, want)
				}
			}
			for _, bad := range tc.mustNotHave {
				if strings.Contains(msg, bad) {
					t.Fatalf("doc_type=%q message %q must not contain %q", tc.docType, msg, bad)
				}
			}
		})
	}
}

// spanMatchesOneOf reports how many of the Span schema's oneOf branches the
// given rendered span satisfies. A schema-valid span must match exactly one.
// Each branch is a const-kind object with additionalProperties:false, so the
// check is: kind const matches, every required key is present, and the span
// emits no key the branch does not declare.
func spanMatchesOneOf(t *testing.T, span map[string]interface{}) int {
	t.Helper()
	branches, ok := spanDefinitionSchema()["oneOf"].([]interface{})
	if !ok {
		t.Fatal("spanDefinitionSchema has no oneOf list")
	}
	matched := 0
	for _, b := range branches {
		branch, ok := b.(map[string]interface{})
		if !ok {
			t.Fatalf("oneOf branch is not an object: %T", b)
		}
		props, _ := branch["properties"].(map[string]interface{})
		kindSchema, _ := props["kind"].(map[string]interface{})
		if span["kind"] != kindSchema["const"] {
			continue
		}
		ok = true
		if reqs, hasReq := branch["required"].([]string); hasReq {
			for _, r := range reqs {
				if _, present := span[r]; !present {
					ok = false
					break
				}
			}
		}
		if ok {
			for k := range span {
				if _, declared := props[k]; !declared {
					ok = false
					break
				}
			}
		}
		if ok {
			matched++
		}
	}
	return matched
}

// TestBuildOpenFileSpan_EmptyAndUnknownKindFallback pins the fix for issue #397:
// a span with an empty or unknown Kind (e.g. a BM25 hit on a cache miss whose
// Span.Kind is "") must degrade to the schema-valid "document" variant rather
// than echoing {"kind":""} (or {"kind":"<unknown>"}), which matches none of the
// Span oneOf branches and makes a strict MCP client (Claude Desktop validates
// structuredContent against outputSchema) reject the whole result with "Failed
// to call tool".
func TestBuildOpenFileSpan_EmptyAndUnknownKindFallback(t *testing.T) {
	cases := []struct {
		name string
		kind string
	}{
		{name: "empty kind", kind: ""},
		{name: "whitespace kind", kind: "   "},
		{name: "unknown kind", kind: "bogus"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := buildOpenFileSpan(model.Span{Kind: tc.kind})
			if got["kind"] != "document" || len(got) != 1 {
				t.Fatalf("buildOpenFileSpan(kind=%q) = %+v, want {\"kind\":\"document\"}", tc.kind, got)
			}
			if n := spanMatchesOneOf(t, got); n != 1 {
				t.Fatalf("rendered span %+v matched %d Span oneOf branches, want exactly 1", got, n)
			}
		})
	}
}

func preview(s string) string {
	if len(s) <= 40 {
		return s
	}
	return s[:40]
}

package ingest

import (
	"bytes"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

var classifyBaseNames = map[string]string{
	"dockerfile":        "code",
	"makefile":          "code",
	"jenkinsfile":       "code",
	"readme":            "text",
	"license":           "text",
	"changelog":         "text",
	"go.mod":            "data",
	"go.sum":            "data",
	"package.json":      "data",
	"package-lock.json": "data",
	"yarn.lock":         "data",
	"pnpm-lock.yaml":    "data",
}

func classifyByExtension(ext string) string {
	switch ext {
	case ".go", ".rs", ".py", ".js", ".jsx", ".ts", ".tsx", ".java", ".c", ".cc", ".cpp", ".h", ".hpp", ".cs", ".rb", ".php", ".swift", ".kt", ".kts", ".scala", ".sh", ".bash", ".zsh", ".sql",
		// Common languages a repository holds that the first list missed; each
		// was classified binary_ignored and never indexed. An extension still
		// unknown after this list is sniffed (SniffTextDocType).
		".mjs", ".cjs", ".mts", ".cts", ".vue", ".svelte", ".astro", ".coffee",
		".css", ".scss", ".sass", ".less", ".graphql", ".gql",
		".lua", ".ex", ".exs", ".erl", ".hrl", ".hs", ".ml", ".mli", ".clj", ".cljs", ".cljc",
		".dart", ".r", ".jl", ".pl", ".pm", ".ps1", ".psm1", ".fish", ".bat", ".cmd",
		".tf", ".hcl", ".proto", ".thrift", ".gradle", ".groovy", ".cmake", ".mk", ".bzl", ".star",
		".nix", ".zig", ".elm", ".fs", ".fsx", ".vb", ".m", ".mm", ".sol", ".asm", ".s",
		".rkt", ".scm", ".lisp", ".el", ".vim", ".cu", ".cuh", ".f90", ".nim", ".cr",
		".sv", ".vhd", ".vhdl", ".jsonnet":
		return "code"
	case ".md", ".markdown", ".mdx", ".rst", ".adoc":
		return "md"
	case ".txt", ".log", ".ini", ".cfg", ".conf", ".properties", ".tex", ".bib", ".org":
		return "text"
	case ".csv", ".tsv", ".parquet", ".json", ".jsonl", ".ndjson", ".geojson", ".ipynb", ".xml", ".yaml", ".yml", ".toml":
		return "data"
	case ".html", ".htm", ".xhtml":
		return "html"
	case ".pdf":
		return "pdf"
	case ".doc", ".docx", ".ppt", ".pptx", ".xls", ".xlsx", ".odt", ".odp", ".ods", ".rtf", ".epub":
		return "document"
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".tif", ".tiff", ".svg":
		return "image"
	case ".mp3", ".wav", ".m4a", ".flac", ".aac", ".ogg", ".opus":
		return "audio"
	case ".mp4", ".mov":
		return "video"
	case ".zip", ".tar", ".gz", ".tgz", ".bz2", ".xz", ".7z", ".rar":
		return "archive"
	default:
		return "binary_ignored"
	}
}

// ClassifyDocType maps a path to an ingestion document type.
func ClassifyDocType(relPath string) string {
	base := strings.ToLower(filepath.Base(relPath))

	// treat plain ".env" and dot-separated variants as sensitive and
	// skip them during ingestion. these often contain secrets/credentials
	// so we classify them as "ignore". previously they were marked as
	// "data" which risked accidental indexing; other variants would fall
	// through to extension-based logic yielding "binary_ignored".
	// note: the exact filename ".env" is caught by the equality check
	// (base == ".env"), whereas names like ".env.local" use HasPrefix.
	if base == ".env" || strings.HasPrefix(base, ".env.") {
		return "ignore"
	}

	if t, ok := classifyBaseNames[base]; ok {
		return t
	}

	return classifyByExtension(strings.ToLower(filepath.Ext(base)))
}

// sniffSampleBytes is how much of a file SniffTextDocType reads.
const sniffSampleBytes = 8 << 10

// SniffTextDocType refines a path-based classification with the content, as
// SPEC §7.3 requires ("extension + MIME sniff + binary heuristics"). A file
// whose extension is unknown (binary_ignored by path) is "text" when its first
// 8 KiB is valid UTF-8 with no NUL byte: a dotfile such as .gitignore, a
// language the extension table does not list, a README without an extension.
// It stays binary_ignored when it looks binary, when it is empty, when it holds
// a private key (a key saved under an unusual name must never become a
// document), and for subtitle files, which are read as sidecars of their media
// and would otherwise be indexed twice. Every other classification is returned
// unchanged.
func SniffTextDocType(docType, relPath string, content []byte) string {
	if docType != "binary_ignored" || len(content) == 0 {
		return docType
	}
	ext := strings.ToLower(filepath.Ext(relPath))
	for _, sc := range sidecarExtensions {
		if ext == sc {
			return docType
		}
	}
	sample := content
	if len(sample) > sniffSampleBytes {
		sample = sample[:sniffSampleBytes]
		// Do not let a multi-byte rune cut at the sample edge fail the check.
		for i := 0; i < utf8.UTFMax && len(sample) > 0 && !utf8.Valid(sample); i++ {
			sample = sample[:len(sample)-1]
		}
	}
	if bytes.IndexByte(sample, 0) >= 0 || !utf8.Valid(sample) {
		return docType
	}
	if bytes.Contains(sample, []byte("PRIVATE KEY-----")) {
		return docType
	}
	return "text"
}

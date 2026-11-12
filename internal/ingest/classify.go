package ingest

import (
	"bytes"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/dirstral/dir2mcp/internal/avutil"
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
	// Audio and video containers come from the one table avutil keeps, so the
	// classifier, rendition grouping and the media tools cannot disagree about
	// which extension is media. The extensions a video container shares with
	// another format (.ts, .mts, .mod) are not in it: the switch below gives
	// them their other type, and SniffMediaDocType decides by content.
	if avutil.IsAudioContainerExt(ext) {
		return "audio"
	}
	if avutil.IsVideoContainerExt(ext) {
		return "video"
	}
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
// SPEC §7.3 requires ("extension + MIME sniff + binary heuristics"). It first
// applies SniffMediaDocType, so a .ts transport stream is video. A file
// whose extension is unknown (binary_ignored by path) is "text" when its first
// 8 KiB is valid UTF-8 with no NUL byte: a dotfile such as .gitignore, a
// language the extension table does not list, a README without an extension.
// It stays binary_ignored when it looks binary, when it is empty, when it holds
// a private key (a key saved under an unusual name must never become a
// document), and for subtitle files, which are read as sidecars of their media
// and would otherwise be indexed twice. Every other classification is returned
// unchanged.
func SniffTextDocType(docType, relPath string, content []byte) string {
	docType = SniffMediaDocType(docType, relPath, content)
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
		sample = trimIncompleteRune(sample[:sniffSampleBytes])
	}
	if bytes.IndexByte(sample, 0) >= 0 || !utf8.Valid(sample) {
		return docType
	}
	if bytes.Contains(sample, []byte("PRIVATE KEY-----")) {
		return docType
	}
	return "text"
}

// trimIncompleteRune drops a multi-byte UTF-8 rune that the sample edge cut in
// two, so a valid text file does not fail the check at byte 8 KiB. It trims
// only an incomplete rune start and its continuation bytes. An invalid byte is
// a complete (one-byte) error for utf8.FullRune, so it stays in the sample and
// still fails the check, and so does any NUL after it.
func trimIncompleteRune(sample []byte) []byte {
	for i := 1; i < utf8.UTFMax && i <= len(sample); i++ {
		if utf8.RuneStart(sample[len(sample)-i]) {
			if !utf8.FullRune(sample[len(sample)-i:]) {
				return sample[:len(sample)-i]
			}
			return sample
		}
	}
	return sample
}

// mediaSniffPrefixBytes is how much of a file SniffMediaDocType needs: the
// first bytes of three 192-byte BDAV packets fit in it. The ingest service
// reads this prefix before it decides how to read the rest of the file.
const mediaSniffPrefixBytes = 512

const (
	mpegTSPacketBytes = 188
	mpegTSSyncByte    = 0x47
	// bdavHeaderBytes is the timestamp header that the BDAV (M2TS) variant puts
	// before each 188-byte packet: the sync byte is at offset 4 and repeats
	// every 192 bytes.
	bdavHeaderBytes = 4
)

// mpegTSLayouts are the two packet layouts of a transport stream: the offset
// of the first sync byte and the distance between two sync bytes.
var mpegTSLayouts = []struct{ offset, stride int }{
	{0, mpegTSPacketBytes},
	{bdavHeaderBytes, bdavHeaderBytes + mpegTSPacketBytes},
}

// needsMediaSniff reports whether the type of relPath depends on its content:
// the extension is one that a video container shares with another format, and
// the path alone gave the other format. A basename rule (go.mod is data) wins
// over the extension, so such a file is never sniffed.
func needsMediaSniff(relPath, pathType string) bool {
	if !avutil.IsContentSniffedVideoExt(filepath.Ext(relPath)) {
		return false
	}
	return pathType == "code" || pathType == "binary_ignored"
}

// SniffMediaDocType resolves an extension that a video container shares with
// another format by the first bytes of the file (SPEC §7.3):
//
//   - .ts and .mts are TypeScript by extension. They are video when the
//     content is an MPEG transport stream: the sync byte 0x47 at offsets 0, 188
//     and 376, or at 4, 196 and 388 for the BDAV (M2TS) variant.
//   - .mod is video when the content starts with an MPEG program stream pack
//     header (00 00 01 BA), as camcorder .mod files do.
//
// The sync byte is the ASCII letter "G", so a transport stream must also hold
// a byte that text never holds (a NUL, or invalid UTF-8) in the sampled window.
// Real transport streams always do; a TypeScript file never does. Every other
// classification is returned unchanged.
func SniffMediaDocType(docType, relPath string, prefix []byte) string {
	if !needsMediaSniff(relPath, docType) {
		return docType
	}
	switch strings.ToLower(filepath.Ext(relPath)) {
	case ".ts", ".mts":
		if docType == "code" && isMPEGTransportStream(prefix) {
			return "video"
		}
	case ".mod":
		if docType == "binary_ignored" && isMPEGProgramStream(prefix) {
			return "video"
		}
	}
	return docType
}

// isMPEGTransportStream reports whether prefix starts three MPEG-TS packets,
// in the plain 188-byte layout or the 192-byte BDAV layout.
func isMPEGTransportStream(prefix []byte) bool {
	for _, l := range mpegTSLayouts {
		end := l.offset + 2*l.stride + 1
		if len(prefix) < end {
			continue
		}
		synced := true
		for i := 0; i < 3; i++ {
			if prefix[l.offset+i*l.stride] != mpegTSSyncByte {
				synced = false
				break
			}
		}
		if synced && !looksLikeText(prefix[:end]) {
			return true
		}
	}
	return false
}

// isMPEGProgramStream reports whether prefix starts with an MPEG program
// stream pack header.
func isMPEGProgramStream(prefix []byte) bool {
	return bytes.HasPrefix(prefix, []byte{0x00, 0x00, 0x01, 0xBA})
}

// looksLikeText reports whether sample is valid UTF-8 with no NUL byte, the
// same test SniffTextDocType applies. A multi-byte rune that the window edge
// cut in two does not count against it.
func looksLikeText(sample []byte) bool {
	sample = trimIncompleteRune(sample)
	return bytes.IndexByte(sample, 0) < 0 && utf8.Valid(sample)
}

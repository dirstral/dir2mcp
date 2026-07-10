package ingest

import (
	"path/filepath"
	"strings"
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
	case ".go", ".rs", ".py", ".js", ".jsx", ".ts", ".tsx", ".java", ".c", ".cc", ".cpp", ".h", ".hpp", ".cs", ".rb", ".php", ".swift", ".kt", ".kts", ".scala", ".sh", ".bash", ".zsh", ".sql":
		return "code"
	case ".md", ".markdown", ".mdx", ".rst", ".adoc":
		return "md"
	case ".txt", ".log", ".ini", ".cfg", ".conf":
		return "text"
	case ".csv", ".tsv", ".parquet", ".json", ".jsonl", ".xml", ".yaml", ".yml", ".toml":
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

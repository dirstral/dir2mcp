package model

import (
	"strings"
)

// StateMachinesMarkdown renders docs/state-machines.md from DocumentLifecycle,
// EmbeddingLifecycle and the list_files projection. A drift test in
// tests/lifecycle compares the committed file with this output, so the doc
// cannot go stale. Regenerate with:
//
//	DIR2MCP_UPDATE_STATE_MACHINES_DOC=1 go test ./tests/lifecycle -run StateMachinesDoc
func StateMachinesMarkdown() string {
	var b strings.Builder
	b.WriteString("# State machines\n\n")
	b.WriteString("<!-- Generated from internal/model/lifecycle_status.go. Do not edit by hand.\n")
	b.WriteString("     Regenerate: DIR2MCP_UPDATE_STATE_MACHINES_DOC=1 go test ./tests/lifecycle -run StateMachinesDoc -->\n\n")
	b.WriteString("dir2mcp keeps two persisted lifecycles: the status of a document and the\n")
	b.WriteString("embedding status of a chunk. This page lists their states, the transitions\n")
	b.WriteString("the code can make today, and where each state is visible.\n\n")
	b.WriteString("The tables record what the code does. They forbid nothing yet. A write\n")
	b.WriteString("that the table does not list is logged as\n")
	b.WriteString("`lifecycle: unlisted transition <entity> <id> <from>-><to>` and then goes\n")
	b.WriteString("ahead unchanged.\n\n")

	b.WriteString("## Document status (`documents.status`)\n\n")
	writeStates(&b, DocumentLifecycle.States)
	b.WriteString("\nThe store folds any other value to `ok` (`NormalizeDocumentStatus`).\n")
	b.WriteString("No code path writes `pending` today; the store keeps it when written.\n\n")
	b.WriteString("### Transitions\n\n")
	b.WriteString(DocumentLifecycle.MarkdownTable())
	b.WriteString("\n### Where it is visible\n\n")
	b.WriteString("- MCP `list_files` field `status`, enum ")
	writeCodeList(&b, ListFilesStatusEnum)
	b.WriteString(". Projection:\n\n")
	b.WriteString("| Stored | `list_files` |\n| --- | --- |\n")
	for _, s := range DocumentLifecycle.States {
		b.WriteString("| `" + string(s) + "` | `" + ListFilesStatus(string(s)) + "` |\n")
	}
	b.WriteString("| any other value | `" + ListFilesStatus("") + "` |\n\n")
	b.WriteString("- Persisted corpus stats (`CorpusStats`, read by the `stats` tool and\n  `dir2mcp status`), live documents only:\n")
	b.WriteString("  `indexed` counts `ok`; `skipped` counts `skipped` and `secret_excluded`;\n")
	b.WriteString("  `errors` counts `error`; `deleted` counts tombstoned rows of any status.\n")
	b.WriteString("- `skip_summary` groups `skipped` and `secret_excluded` rows by `skip_reason`.\n\n")

	b.WriteString("## Chunk embedding status (`chunks.embedding_status`)\n\n")
	writeStates(&b, EmbeddingLifecycle.States)
	b.WriteString("\nThe store folds any other value to `pending` (`NormalizeEmbeddingStatus`).\n\n")
	b.WriteString("### Transitions\n\n")
	b.WriteString(EmbeddingLifecycle.MarkdownTable())
	b.WriteString("\n### Where it is visible\n\n")
	b.WriteString("- Persisted corpus stats (`CorpusStats`), live chunks only:\n")
	b.WriteString("  `embedded_ok` counts `ok`; `embedded_pending` counts `pending`;\n")
	b.WriteString("  `failed_chunks` and the failure summary group `error` rows by `error_category`.\n")
	b.WriteString("- The embed worker reads `pending` rows (`NextPending`). BM25 and the\n")
	b.WriteString("  summary paths leave out `error` rows.\n")
	b.WriteString("- `dir2mcp reindex --embeddings-only` moves `error` rows back to `pending`.\n")
	return b.String()
}

func writeStates[S ~string](b *strings.Builder, states []S) {
	b.WriteString("States: ")
	names := make([]string, 0, len(states))
	for _, s := range states {
		names = append(names, string(s))
	}
	writeCodeList(b, names)
	b.WriteString(".\n")
}

func writeCodeList(b *strings.Builder, values []string) {
	for i, v := range values {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("`" + v + "`")
	}
}

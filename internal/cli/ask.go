package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dir2mcp/internal/model"
)

func (a *App) runAsk(ctx context.Context, global globalOptions, args []string) int {
	opts, err := parseAskOptions(args)
	if err != nil {
		writef(a.stderr, "invalid ask flags: %v\n", err)
		return exitConfigInvalid
	}

	cfg, err := loadConfigWithGlobalOptions(global)
	if err != nil {
		writef(a.stderr, "load config: %v\n", err)
		return exitConfigInvalid
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		cfg.StateDir = filepath.Join(".", ".dir2mcp")
	}

	if strings.TrimSpace(cfg.MistralAPIKey) == "" {
		se := a.sty(global.jsonOutput)
		nonInteractiveMode := global.nonInteractive || !isTerminal(os.Stdin) || !isTerminal(os.Stdout)
		if nonInteractiveMode {
			writef(a.stderr, "%s CONFIG_INVALID: Missing MISTRAL_API_KEY\n", se.errPrefix())
			writeln(a.stderr, "Set env: MISTRAL_API_KEY=...")
			writeln(a.stderr, "Or run: dir2mcp config init")
		} else {
			writef(a.stderr, "%s Missing MISTRAL_API_KEY\n", se.errPrefix())
			writeln(a.stderr, "Run: dir2mcp config init")
		}
		return exitConfigInvalid
	}

	st := a.storeForConfig(cfg)
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil && !errors.Is(err, model.ErrNotImplemented) {
		writef(a.stderr, "initialize metadata store: %v\n", err)
		return exitIndexLoadFailure
	}

	retriever, cleanup, err := a.buildRetrieverForAsk(ctx, cfg, st)
	if err != nil {
		writef(a.stderr, "initialize retriever: %v\n", err)
		return exitIndexLoadFailure
	}
	if cleanup != nil {
		defer cleanup()
	}

	query := model.SearchQuery{
		Query:      opts.question,
		K:          opts.k,
		Index:      opts.index,
		PathPrefix: opts.pathPrefix,
		FileGlob:   opts.fileGlob,
		DocTypes:   opts.docTypes,
	}

	if opts.mode == "search_only" {
		return a.runAskSearchOnly(ctx, global, opts.question, retriever, query)
	}

	askResult, askErr := retriever.Ask(ctx, opts.question, query)
	if askErr != nil {
		writef(a.stderr, "ask failed: %v\n", askErr)
		return exitGeneric
	}
	return a.renderAskResult(global, askResult)
}

func (a *App) runAskSearchOnly(ctx context.Context, global globalOptions, question string, retriever model.Retriever, query model.SearchQuery) int {
	hits, searchErr := retriever.Search(ctx, query)
	if searchErr != nil {
		writef(a.stderr, "ask failed: %v\n", searchErr)
		return exitGeneric
	}
	if global.jsonOutput {
		indexingComplete := true
		if ic, err := retriever.IndexingComplete(ctx); err == nil {
			indexingComplete = ic
		}
		payload := map[string]interface{}{
			"question":          question,
			"answer":            "",
			"citations":         []interface{}{},
			"hits":              serializeHits(hits),
			"indexing_complete": indexingComplete,
		}
		if err := emitJSON(a.stdout, payload); err != nil {
			writef(a.stderr, "encode ask json: %v\n", err)
			return exitGeneric
		}
		return exitSuccess
	}
	if global.quiet {
		return exitSuccess
	}
	s := a.sty(false)
	writeln(a.stdout)
	writef(a.stdout, "  %s %s\n\n", s.sectionHeader("Search results"), s.dim(fmt.Sprintf("(%d hits)", len(hits))))
	for i, hit := range hits {
		snippet := strings.TrimSpace(hit.Snippet)
		if snippet == "" {
			snippet = "(no snippet)"
		}
		writef(a.stdout, "  %s %s  %s\n", s.Brand.Render(fmt.Sprintf("[%d]", i+1)), s.Cyan.Render(hit.RelPath), s.dim(fmt.Sprintf("score=%.4f", hit.Score)))
		writef(a.stdout, "      %s\n", s.dim(snippet))
	}
	writeln(a.stdout)
	return exitSuccess
}

func (a *App) renderAskResult(global globalOptions, askResult model.AskResult) int {
	if global.jsonOutput {
		payload := map[string]interface{}{
			"question":          askResult.Question,
			"answer":            askResult.Answer,
			"citations":         serializeCitations(askResult.Citations),
			"hits":              serializeHits(askResult.Hits),
			"indexing_complete": askResult.IndexingComplete,
		}
		if err := emitJSON(a.stdout, payload); err != nil {
			writef(a.stderr, "encode ask json: %v\n", err)
			return exitGeneric
		}
		return exitSuccess
	}
	if global.quiet {
		return exitSuccess
	}
	s := a.sty(false)
	writeln(a.stdout)
	writeln(a.stdout, askResult.Answer)
	if len(askResult.Citations) > 0 {
		writeln(a.stdout)
		writef(a.stdout, "  %s\n", s.sectionHeader("Citations"))
		for i, citation := range askResult.Citations {
			writef(a.stdout, "  %s %s  %s\n",
				s.Brand.Render(fmt.Sprintf("[%d]", i+1)),
				s.Cyan.Render(citation.RelPath),
				s.dim(fmt.Sprintf("chunk=%d span=%s", citation.ChunkID, formatSpan(citation.Span))),
			)
		}
	}
	writeln(a.stdout)
	return exitSuccess
}

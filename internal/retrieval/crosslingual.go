package retrieval

import (
	"context"
	"strings"
	"sync"
	"unicode"

	"github.com/dirstral/dir2mcp/internal/model"
)

// crossLingualAutoSentinel mirrors config.crossLingualAutoSentinel: the
// target-langs value meaning "expand into the corpus's detected languages
// (#267)" rather than an explicit pinned list. Duplicated here to avoid a
// retrieval→config dependency for a single constant.
const crossLingualAutoSentinel = "auto"

// crossLingualMaxVariants caps how many translated query variants a single
// search may issue, bounding the added embed/retrieval cost (and translator
// calls) regardless of how many languages the corpus records. The original
// query is always retrieved in addition to the variants. Together with the
// single HyDE generation this is the per-query LLM-call budget: at most
// 1 + crossLingualMaxVariants generations per uncached query, and zero on a
// cache hit (#444).
const crossLingualMaxVariants = 8

// crossLingualMaxTokens caps each translation completion's OUTPUT tokens (#444).
// A translated SEARCH QUERY is short by construction, so a tight cap is ample;
// it bounds cost/latency and defuses a crafted query that tries to make the
// translator emit a long completion. Applied only when the translator
// implements model.BoundedGenerator; otherwise the provider's default applies.
const crossLingualMaxTokens = 128

// crossLingualConcurrency bounds how many translation generations run at once
// (#444). The per-language translations are independent, so issuing them
// concurrently (rather than strictly serially) cuts expansion latency, while
// the bound keeps a single query from opening crossLingualMaxVariants
// simultaneous provider connections.
const crossLingualConcurrency = 4

// searchExpanded runs the HyDE/per-mode retrieval pipeline for the original
// query and, when cross-lingual query expansion (#325) is active, the per-mode
// pipeline for each translated query variant, then RRF-fuses the per-language
// result sets via the shared fusion primitive. When expansion is inactive
// (disabled, no translator, or no resolvable target languages) it is exactly one
// searchWithHyDE call on the original query, so the un-expanded path is
// unchanged.
//
// Expansion is best-effort and never fails the search on translation trouble: a
// target language whose translation errors (or returns blank, or equals the
// original query) is simply skipped. Only the BASE query's retrieval error
// propagates; a variant's retrieval error is logged and that variant dropped, so
// a flaky variant degrades recall rather than breaking the search.
func (s *Service) searchExpanded(ctx context.Context, query model.SearchQuery, k int) ([]model.SearchHit, error) {
	// The base (original-language) query runs through the full HyDE/per-mode
	// pipeline, so with cross-lingual expansion disabled this is byte-identical to
	// a plain searchWithHyDE call.
	base, err := s.searchWithHyDE(ctx, query, k)
	if err != nil {
		return nil, err
	}

	variants := s.crossLingualQueryVariants(ctx, query.Query)
	if len(variants) == 0 {
		return base, nil
	}

	lists := make([][]model.SearchHit, 0, len(variants)+1)
	lists = append(lists, base)
	for _, v := range variants {
		// Translated variants reuse the original query's filters/mode (so routing
		// and predicates match) but retrieve on the variant text. They go through
		// searchByMode (not searchWithHyDE) to bound the added cost to retrieval —
		// no extra generation per variant.
		hits, verr := s.searchByMode(ctx, v, k, query, true)
		if verr != nil {
			// A variant retrieval failure degrades gracefully: log and skip it
			// rather than failing the whole (cross-lingual) search.
			s.logf("cross-lingual: retrieval for a translated query variant failed, skipping: %v", verr)
			continue
		}
		lists = append(lists, hits)
	}
	if len(lists) == 1 {
		// Every variant dropped out: fall back to the un-fused base result so the
		// outcome matches a plain search rather than re-fusing a single list.
		return base, nil
	}
	return fuseRRFMulti(lists, k), nil
}

// crossLingualQueryVariants returns the translated query strings to additionally
// retrieve for, one per resolved target language, when cross-lingual expansion
// is active. It returns nil (no expansion) when the feature is disabled, no
// translator is wired, the query is blank, or no target languages resolve. The
// detected query language is skipped (no self-translation), as is any target
// whose translation fails, returns blank, or is unchanged from the input.
func (s *Service) crossLingualQueryVariants(ctx context.Context, queryStr string) []string {
	s.metaMu.RLock()
	enabled := s.crossLingualEnabled
	translator := s.crossLingualTranslator
	configured := append([]string(nil), s.crossLingualTargetLangs...)
	corpusFn := s.crossLingualCorpusLangsFn
	s.metaMu.RUnlock()

	q := strings.TrimSpace(queryStr)
	if !enabled || translator == nil || q == "" {
		return nil
	}

	targets := resolveCrossLingualTargets(configured, corpusFn, detectQueryLanguage(q))
	if len(targets) == 0 {
		return nil
	}

	// A repeated query (interactive MCP, the smoke gate) reuses the cached
	// variants instead of re-issuing up to crossLingualMaxVariants translation
	// generations (#444, F4). Keyed on (model, targets, query) so a model or
	// target-set change is never served stale.
	if cached, ok := s.expansionCache.getVariants(s.genModel, q, targets); ok {
		return cached
	}

	// Translate each target concurrently (bounded), preserving target order in
	// the results so dedup/RRF fusion stays deterministic (#444, F2). Each
	// generation is output-token-capped (#444, F3).
	translations := make([]string, len(targets))
	sem := make(chan struct{}, crossLingualConcurrency)
	var wg sync.WaitGroup
	for i, lang := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, lang string) {
			defer wg.Done()
			defer func() { <-sem }()
			translated, terr := boundedGenerate(ctx, translator, buildCrossLingualPrompt(q, lang), crossLingualMaxTokens)
			if terr != nil {
				// A per-language translation failure degrades gracefully (skip it),
				// never fails the search (#325).
				s.logf("cross-lingual: translating query into %q failed, skipping: %v", lang, terr)
				return
			}
			translations[i] = strings.TrimSpace(translated)
		}(i, lang)
	}
	wg.Wait()

	variants := make([]string, 0, len(targets))
	// Dedup translated variants: distinct target languages can yield the same
	// translation (or one equal to the query), and retrieving/fusing a duplicate
	// would double-count its evidence and skew the RRF ranking.
	seenVariants := make(map[string]struct{}, len(targets))
	for _, t := range translations {
		if t == "" || strings.EqualFold(t, q) {
			continue
		}
		key := strings.ToLower(t)
		if _, dup := seenVariants[key]; dup {
			continue
		}
		seenVariants[key] = struct{}{}
		variants = append(variants, t)
	}
	s.expansionCache.putVariants(s.genModel, q, targets, variants)
	return variants
}

// resolveCrossLingualTargets resolves the effective set of target languages to
// expand a query into. An empty configured list, or one containing the "auto"
// sentinel, contributes the corpus's detected languages (via corpusFn, #267);
// other configured entries are taken literally. The detected query language is
// excluded (matched on primary subtag) so the query is never re-translated into
// its own language. Results are primary-subtag-deduplicated, order-preserving
// (configured-explicit first, then corpus-detected), and capped at
// crossLingualMaxVariants.
func resolveCrossLingualTargets(configured []string, corpusFn func() []string, queryLang string) []string {
	queryPrimary := model.LanguagePrimarySubtag(queryLang)

	var corpus []string
	corpusLoaded := false
	loadCorpus := func() []string {
		if !corpusLoaded {
			corpusLoaded = true
			if corpusFn != nil {
				corpus = corpusFn()
			}
		}
		return corpus
	}

	// "auto" mode: an empty configured list means auto.
	wantAuto := len(configured) == 0
	candidates := make([]string, 0, len(configured)+4)
	for _, c := range configured {
		l := strings.ToLower(strings.TrimSpace(c))
		if l == "" {
			continue
		}
		if l == crossLingualAutoSentinel {
			wantAuto = true
			continue
		}
		candidates = append(candidates, l)
	}
	if wantAuto {
		for _, c := range loadCorpus() {
			if l := strings.ToLower(strings.TrimSpace(c)); l != "" {
				candidates = append(candidates, l)
			}
		}
	}

	out := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		primary := model.LanguagePrimarySubtag(c)
		if primary == "" {
			continue
		}
		// Skip the query's own language (no self-translation).
		if queryPrimary != "" && primary == queryPrimary {
			continue
		}
		if _, dup := seen[primary]; dup {
			continue
		}
		seen[primary] = struct{}{}
		out = append(out, c)
		if len(out) >= crossLingualMaxVariants {
			break
		}
	}
	return out
}

// buildCrossLingualPrompt builds a deterministic, general-purpose prompt that
// translates a SEARCH QUERY into targetLang. It mirrors the ingest translate
// prompt's contract (target-only, return the translation alone) but frames the
// input as a query so the model keeps it terse and search-appropriate rather
// than expanding it into prose.
func buildCrossLingualPrompt(query, targetLang string) string {
	var b strings.Builder
	b.WriteString("Translate the following search query into ")
	b.WriteString(targetLang)
	b.WriteString(". Preserve the meaning and keep it concise, as a search query. ")
	b.WriteString("Return only the translated query, with no preamble, quotes, or explanation.\n\n")
	b.WriteString(query)
	return b.String()
}

// detectQueryLanguage returns a best-effort BCP-47 primary subtag for the
// dominant script of the query, used ONLY to skip self-translation (translating
// a query into the language it is already in). It is intentionally a coarse,
// dependency-free script heuristic — not a full language identifier — covering
// the scripts dir2mcp's multilingual corpora most commonly mix (e.g. Latin vs
// Cyrillic). When the dominant script does not map to a single language (most
// notably Latin, shared by many languages) it returns "" so NO target is
// skipped — i.e. the query is expanded into every target, which is the safe
// recall-preserving default. The mapping never claims a specific language for an
// ambiguous script.
func detectQueryLanguage(query string) string {
	counts := map[string]int{}
	for _, r := range query {
		if !unicode.IsLetter(r) {
			continue
		}
		switch {
		case unicode.Is(unicode.Cyrillic, r):
			counts["ru"]++
		case unicode.Is(unicode.Hiragana, r), unicode.Is(unicode.Katakana, r):
			counts["ja"]++
		case unicode.Is(unicode.Hangul, r):
			counts["ko"]++
		case unicode.Is(unicode.Greek, r):
			counts["el"]++
		case unicode.Is(unicode.Arabic, r):
			counts["ar"]++
		case unicode.Is(unicode.Hebrew, r):
			counts["he"]++
		default:
			// Latin, Han, and other shared scripts are ambiguous: do not attribute
			// a language, so no target is skipped on their account. Han in
			// particular is shared by Chinese and Japanese (and historically
			// Korean), so mapping it to a single language would wrongly suppress a
			// target variant; a genuine self-translation is still caught later by
			// the query-equality check.
		}
	}
	best := ""
	bestN := 0
	for lang, n := range counts {
		if n > bestN {
			best, bestN = lang, n
		}
	}
	return best
}

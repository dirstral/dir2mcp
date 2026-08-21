package retrieval

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Question routing (issue #897).
//
// Nine retrieval configurations were measured end to end on one real corpus and
// none raised the overall score. Exactly one, HyDE, doubled the weakest question
// category (superlatives, 2/5 to 4/5) and made three others worse. HyDE writes a
// hypothetical answer and retrieves against it, which is right for a question
// that needs semantic reach and wrong for a question about ABSENCE: inventing
// plausible content is the opposite of what a negative control needs. One global
// flag therefore cannot serve five question shapes.
//
// The fix classifies the question and applies the profile measured to suit its
// shape. Classification is deterministic: an ordered table of patterns over the
// question text, first match wins. It costs no round trip, adds no failure mode
// on the critical path, and is testable without a live model.
//
// SAFETY PROPERTY OF THE SHIPPED TABLE. The two routes the shipped table turns
// HyDE ON for are matched LAST, so every earlier route leaves HyDE off and a
// misclassification degrades toward today's behaviour rather than toward more
// query transformation. This is a property of the shipped table, not of the
// classifier: an operator who names an earlier-matching route in
// retrieval.question_routing.hyde_routes gives that route HyDE and gives the
// property up.

// QuestionRoute names the shape of a question. The set is CLOSED: a closed enum
// is testable and can be pinned by a conformance fixture, while an open set
// defers the taxonomy and makes conformance vague. The five named shapes are the
// categories the #897 measurement graded; RouteDefault is the fallback for a
// question that matches none of them.
type QuestionRoute string

const (
	// RouteDefault is the fallback for an unclassifiable question. Its profile
	// always inherits the server's global settings, so a question that matches no
	// pattern is retrieved exactly as it is today.
	RouteDefault QuestionRoute = "default"

	// RouteNegativeControl asks whether something is ABSENT. HyDE is actively
	// harmful here (measured 7/8 to 6/8) because a hypothetical answer supplies
	// the very content the question asks the corpus to lack.
	RouteNegativeControl QuestionRoute = "negative_control"

	// RouteTimeScoped restricts the answer to a window of the source timeline.
	// Measured 6/6 to 4/6 under HyDE: nothing to gain, two to lose.
	RouteTimeScoped QuestionRoute = "time_scoped"

	// RouteEnumeration asks for a complete set or a count. Measured 5/6 to 4/6
	// under HyDE.
	RouteEnumeration QuestionRoute = "enumeration"

	// RouteSuperlative asks for an extreme (most, fastest, longest). Measured
	// 2/5 to 4/5 under HyDE: the single largest gain in the table.
	RouteSuperlative QuestionRoute = "superlative"

	// RoutePointLookup asks for one specific fact. Measured 11/12 to 12/12 under
	// HyDE.
	RoutePointLookup QuestionRoute = "point_lookup"
)

// RouteProfile is the COMPLETE set of retrieval settings a route may change. The
// surface is deliberately one field, and the type is the enforcement: a caller
// cannot express a change this struct has no field for.
//
// WHAT A PROFILE MAY NOT SET, and why:
//
//   - The system prompt and its injection guard (#885). composeSystemPrompt
//     appends the guard to whatever prompt is in force, at the one place that
//     also writes the document fences. A profile carries no prompt field, so no
//     route can reach that composition.
//   - The abstention decision (SPEC §9.4.3). The absolute evidence thresholds
//     are server constants in evidence.go and are not operator-configurable at
//     all; §9.4.3 also requires a server to DOCUMENT the threshold it ships per
//     retrieval mode, which a per-route override would make unstatable.
//   - The pruning floor retrieval.min_score (SPEC §9.4.3). Same reason: the
//     shipped default is a documented number, not a per-question one.
//   - k (SPEC §9.1). §9.1 binds the ADVERTISED default to behaviour: "The
//     `default` a server advertises for `k` MUST therefore be the value an
//     omitted field actually produces". A route-dependent k has no single such
//     value, so no advertised default could be honest. This is spec surface and
//     stays out until a dirstral-spec change lands.
//   - The rerank decision (SPEC §9.1.1). §9.1.1 fixes when reranking is active:
//     "auto-enabled when a rerank provider credential is present ... and
//     disabled otherwise", with rerank.enabled as the one override. A route-level
//     third input is spec surface.
//
// MMR lambda is absent for a different reason: it is un-spec'd and would be
// safe, but the #897 measurement varied HyDE, not lambda, so there is no data
// behind a per-route value.
type RouteProfile struct {
	// HyDE decides the HyDE query transform for this route. True forces it on,
	// false forces it off. A nil pointer inherits the server's global
	// retrieval.hyde.enabled, which is what RouteDefault always does.
	HyDE *bool
}

// questionRoutePatterns is the ordered classification table. First match wins,
// so the order IS the precedence rule for a question that carries more than one
// shape ("which pitcher never gave up a hit?" is superlative and negative).
//
// The order is: negative control, then the routes by ascending measured HyDE
// benefit. Negative control is first because its failure mode is fabrication
// rather than a miss, and fabrication is the worse outcome. The rest follow the
// table in #897, so under the SHIPPED profile table a question that reads as two
// shapes takes the more HyDE-averse one. A custom table can name an
// earlier-matching route and lose that.
var questionRoutePatterns = []struct {
	route    QuestionRoute
	patterns []*regexp.Regexp
}{
	{
		route: RouteNegativeControl,
		patterns: mustCompileAll(
			`\bnever\b`,
			`\bno\s+one\b`,
			`\bnobody\b`,
			`\bnothing\b`,
			`\bnone\b`,
			`\bnot\s+a\s+single\b`,
			`\bwithout\b`,
			`\bzero\b`,
			`\b(?:did|do|does|was|were|is|are|has|have|had|could|would|should|will|can)\s+not\b`,
			// Contractions: didn't, wasn't, weren't, isn't, hasn't, can't. Both the
			// ASCII apostrophe and the typographic one, because a question arrives
			// as a client typed it.
			`n['\x{2019}]t\b`,
			`\b(?:was|were|is|are)\s+there\s+any\b`,
			`\bany\b.*\bat\s+all\b`,
		),
	},
	{
		route: RouteTimeScoped,
		patterns: mustCompileAll(
			`\bbefore\b`,
			`\bafter\b`,
			`\bduring\b`,
			`\bbetween\b.*\band\b`,
			`\bat\s+the\s+(?:start|beginning|end)\b`,
			`\bby\s+the\s+(?:start|end)\b`,
			`\b(?:first|last|final)\s+(?:half|quarter|third)\b`,
			`\bin\s+the\s+(?:first|last)\s+\d+\s*(?:ms|milliseconds?|seconds?|minutes?|hours?)\b`,
			// A clock offset the caller typed, e.g. 1:24 or 01:24:00.
			`\b\d{1,2}:\d{2}(?::\d{2})?\b`,
			`\bearly\b`,
			`\blate\b`,
			`\bso\s+far\b`,
		),
	},
	{
		route: RouteEnumeration,
		patterns: mustCompileAll(
			`\blist\b`,
			`\benumerate\b`,
			`\bevery\b`,
			`\beach\b`,
			`\ball\s+(?:the|of)\b`,
			`\ball\s+\w+s\b`,
			`\bhow\s+many\b`,
			`\bwhich\s+ones\b`,
			`\bname\s+(?:all|every|the)\b`,
			`\bwhat\s+are\s+the\b`,
			`\bcount\b`,
		),
	},
	{
		route: RouteSuperlative,
		patterns: mustCompileAll(
			// "first" and "last" are deliberately absent: they read as superlatives
			// of order but collide with the time-scoped markers above, and the
			// collision would move questions toward MORE HyDE.
			`\b(?:most|least|best|worst|highest|lowest|longest|shortest|fastest|slowest|biggest|smallest|largest|greatest|strongest|weakest|hardest|easiest|top|maximum|minimum|deepest|widest|heaviest|lightest|oldest|newest|youngest|nearest|farthest|furthest)\b`,
			`\bmore\s+than\s+any\b`,
		),
	},
	{
		route: RoutePointLookup,
		patterns: mustCompileAll(
			// Anchored on purpose. An unanchored interrogative would swallow most
			// questions, and this route turns HyDE ON, so breadth here is the one
			// direction that costs something.
			`^(?:please\s+|can\s+you\s+tell\s+me\s+|tell\s+me\s+)?(?:who|whom|whose|what|when|where|which)\b`,
			`\bhow\s+(?:long|fast|far|old|tall|deep|wide|much)\b`,
		),
	},
}

// mustCompileAll compiles each pattern or panics. The patterns are package
// constants, so a bad one is a build-time defect, not a runtime input.
func mustCompileAll(exprs ...string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(exprs))
	for _, e := range exprs {
		out = append(out, regexp.MustCompile(e))
	}
	return out
}

// ClassifyQuestion assigns a question to exactly one route. It is deterministic:
// the table is an ordered slice, never a map, so the same question always yields
// the same route. A question that matches nothing is RouteDefault, which
// inherits the server's global settings.
func ClassifyQuestion(question string) QuestionRoute {
	q := strings.ToLower(strings.TrimSpace(question))
	if q == "" {
		return RouteDefault
	}
	for _, entry := range questionRoutePatterns {
		for _, re := range entry.patterns {
			if re.MatchString(q) {
				return entry.route
			}
		}
	}
	return RouteDefault
}

// QuestionRoutes returns the closed route set in classification order, with
// RouteDefault last. Callers use it to enumerate the vocabulary (config
// validation, error messages) without duplicating the list.
func QuestionRoutes() []QuestionRoute {
	out := make([]QuestionRoute, 0, len(questionRoutePatterns)+1)
	for _, entry := range questionRoutePatterns {
		out = append(out, entry.route)
	}
	return append(out, RouteDefault)
}

// ParseQuestionRoute resolves a configured route name against the closed set. It
// reports false for an unknown name and for RouteDefault, which is not
// configurable: the default route exists to guarantee that an unclassifiable
// question is retrieved exactly as it is today, and a table entry for it would
// break that guarantee.
func ParseQuestionRoute(name string) (QuestionRoute, bool) {
	got := QuestionRoute(strings.ToLower(strings.TrimSpace(name)))
	for _, entry := range questionRoutePatterns {
		if entry.route == got {
			return got, true
		}
	}
	return "", false
}

// ConfigurableQuestionRouteNames returns the route names an operator may name in
// retrieval.question_routing.hyde_routes, sorted, for a config error message.
func ConfigurableQuestionRouteNames() []string {
	out := make([]string, 0, len(questionRoutePatterns))
	for _, entry := range questionRoutePatterns {
		out = append(out, string(entry.route))
	}
	sort.Strings(out)
	return out
}

// QuestionRouteTable holds one profile per route. It is built once at
// construction and only read afterwards, so a query never mutates it.
type QuestionRouteTable struct {
	profiles map[QuestionRoute]RouteProfile
}

// defaultHyDERoutes is the shipped table: the routes the #897 measurement showed
// HyDE helps. Superlatives gained 2 and point lookups gained 1; enumeration,
// negative control and time-scoped each lost.
var defaultHyDERoutes = []QuestionRoute{RouteSuperlative, RoutePointLookup}

// DefaultQuestionRouteTable returns the shipped table. It is consulted only when
// question routing is enabled, which is opt-in, so it never changes the
// behaviour of a deployment that configures nothing.
func DefaultQuestionRouteTable() *QuestionRouteTable {
	return newTable(defaultHyDERoutes)
}

// NewQuestionRouteTable builds a table from the configured route names whose
// profile turns HyDE on. Every other classified route gets HyDE off; RouteDefault
// always inherits. An unknown or non-configurable name is an error rather than a
// silent no-op, because a typo that quietly disables the feature is the failure
// mode #624 already cost this project once.
func NewQuestionRouteTable(hydeRouteNames []string) (*QuestionRouteTable, error) {
	routes := make([]QuestionRoute, 0, len(hydeRouteNames))
	for _, name := range hydeRouteNames {
		if strings.TrimSpace(name) == "" {
			continue
		}
		route, ok := ParseQuestionRoute(name)
		if !ok {
			return nil, fmt.Errorf(
				"unknown question route %q: must be one of %s",
				name, strings.Join(ConfigurableQuestionRouteNames(), ", "))
		}
		routes = append(routes, route)
	}
	return newTable(routes), nil
}

// newTable materializes a profile for every classified route: HyDE on for the
// named ones, off for the rest. RouteDefault is left absent so Profile returns
// the inherit-everything profile for it.
func newTable(hydeRoutes []QuestionRoute) *QuestionRouteTable {
	on := make(map[QuestionRoute]bool, len(hydeRoutes))
	for _, r := range hydeRoutes {
		on[r] = true
	}
	profiles := make(map[QuestionRoute]RouteProfile, len(questionRoutePatterns))
	for _, entry := range questionRoutePatterns {
		hyde := on[entry.route]
		profiles[entry.route] = RouteProfile{HyDE: &hyde}
	}
	return &QuestionRouteTable{profiles: profiles}
}

// Profile returns the table entry for a route. A route with no entry (always
// RouteDefault) gets the inherit-everything profile, a RouteProfile whose HyDE
// is nil.
//
// The returned pointer is a fresh copy, never the table's own. One table serves
// every concurrent query, so handing out its pointer would let a caller rewrite
// a profile for the whole deployment through a value it was only meant to read.
func (t *QuestionRouteTable) Profile(route QuestionRoute) RouteProfile {
	if t == nil {
		return RouteProfile{}
	}
	profile := t.profiles[route]
	if profile.HyDE != nil {
		hyde := *profile.HyDE
		profile.HyDE = &hyde
	}
	return profile
}

// resolveRouteHyDE applies a route's profile to the server's global HyDE
// decision. With routing off, no table, or a route that inherits, the global
// value is returned unchanged, so the un-routed path is byte-identical.
func resolveRouteHyDE(routingEnabled bool, table *QuestionRouteTable, question string, global bool) bool {
	if !routingEnabled || table == nil {
		return global
	}
	if p := table.Profile(ClassifyQuestion(question)); p.HyDE != nil {
		return *p.HyDE
	}
	return global
}

// ResolveRouteProfile reports the route a question takes and the profile the
// server will actually apply to it, with every inherited field filled in from
// the server's global settings. It is the observable seam for a test: a caller
// can assert the routing decision without a live model deciding answer text.
//
// The route is NOT reported to an MCP client. Making it wire-visible is spec
// surface (SPEC §9.2 fixes the result structure) and needs a dirstral-spec
// change first, so it stays server-side here.
func (s *Service) ResolveRouteProfile(question string) (QuestionRoute, RouteProfile) {
	s.metaMu.RLock()
	routingEnabled := s.questionRoutingEnabled
	table := s.questionRoutes
	globalHyDE := s.hydeEnabled
	s.metaMu.RUnlock()

	if !routingEnabled || table == nil {
		return RouteDefault, RouteProfile{HyDE: &globalHyDE}
	}
	route := ClassifyQuestion(question)
	profile := table.Profile(route)
	if profile.HyDE == nil {
		profile.HyDE = &globalHyDE
	}
	return route, profile
}

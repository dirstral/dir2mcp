package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// Issue #897: nine retrieval configurations were measured on one real corpus and
// none raised the overall score. HyDE alone doubled the weakest category
// (superlatives, 2/5 to 4/5) while making three others worse, because inventing
// a plausible answer is the opposite of what a question about ABSENCE needs. One
// global flag cannot serve five question shapes.
//
// These tests pin the routing DECISION, not the answer text: which route a
// question takes and which profile that route resolves to. Answer quality needs
// a live model and is measured by the pilot harness, not here.

// routeQuestions897 maps each route to a question that must classify into it.
// One representative per route, so a pattern change that moves a shape fails.
var routeQuestions897 = map[retrieval.QuestionRoute]string{
	retrieval.RouteSuperlative:     "What was the fastest pitch of the game?",
	retrieval.RoutePointLookup:     "Who caught the third out?",
	retrieval.RouteNegativeControl: "Did any batter never reach base?",
	retrieval.RouteEnumeration:     "List every substitution.",
	retrieval.RouteTimeScoped:      "What happened during the rain delay?",
	retrieval.RouteDefault:         "Summarize the broadcast.",
}

// newRoutingService897 builds a one-chunk retrieval service with question
// routing enabled and the shipped table installed. The corpus content does not
// matter: every assertion here is about the routing decision.
func newRoutingService897(t *testing.T, gen model.Generator) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	svc.SetChunkMetadata(1, model.SearchHit{
		ChunkID: 1,
		RelPath: "game/broadcast.vtt",
		Snippet: "the play happened",
		Span:    model.Span{Kind: "lines", StartLine: 1, EndLine: 2},
	})
	svc.SetQuestionRouting(true, retrieval.DefaultQuestionRouteTable())
	return svc
}

// resolvedHyDE897 returns the route and the resolved HyDE decision, failing if
// the profile left HyDE unresolved. ResolveRouteProfile always fills an
// inherited field in, so a nil here is a defect in the resolver.
func resolvedHyDE897(t *testing.T, svc *retrieval.Service, question string) (retrieval.QuestionRoute, bool) {
	t.Helper()
	route, profile := svc.ResolveRouteProfile(question)
	if profile.HyDE == nil {
		t.Fatalf("route %q resolved to an unresolved profile: HyDE is nil", route)
	}
	return route, *profile.HyDE
}

// TestRouting897_SuperlativeUsesHyDE_NegativeControlDoesNot is the reported
// case. The two shapes the measurement puts on opposite sides of HyDE must land
// on opposite profiles under one configuration.
func TestRouting897_SuperlativeUsesHyDE_NegativeControlDoesNot(t *testing.T) {
	svc := newRoutingService897(t, &fakeGenerator{out: "hypothetical"})

	route, hyde := resolvedHyDE897(t, svc, routeQuestions897[retrieval.RouteSuperlative])
	if route != retrieval.RouteSuperlative {
		t.Fatalf("superlative question routed to %q, want %q", route, retrieval.RouteSuperlative)
	}
	if !hyde {
		t.Fatal("the superlative route must resolve to a profile with HyDE on")
	}

	route, hyde = resolvedHyDE897(t, svc, routeQuestions897[retrieval.RouteNegativeControl])
	if route != retrieval.RouteNegativeControl {
		t.Fatalf("negative control routed to %q, want %q", route, retrieval.RouteNegativeControl)
	}
	if hyde {
		t.Fatal("the negative-control route must resolve to a profile with HyDE off")
	}
}

// TestRouting897_ShippedTableMatchesTheMeasurement pins the whole shipped table
// against the #897 table: HyDE on for superlative and point lookup, off for
// enumeration, negative control and time-scoped, inherited for default.
func TestRouting897_ShippedTableMatchesTheMeasurement(t *testing.T) {
	svc := newRoutingService897(t, &fakeGenerator{out: "hypothetical"})

	want := map[retrieval.QuestionRoute]bool{
		retrieval.RouteSuperlative:     true,
		retrieval.RoutePointLookup:     true,
		retrieval.RouteEnumeration:     false,
		retrieval.RouteNegativeControl: false,
		retrieval.RouteTimeScoped:      false,
		// The default route inherits, and the service's global HyDE is off here.
		retrieval.RouteDefault: false,
	}
	for route, question := range routeQuestions897 {
		gotRoute, gotHyDE := resolvedHyDE897(t, svc, question)
		if gotRoute != route {
			t.Fatalf("question %q routed to %q, want %q", question, gotRoute, route)
		}
		if gotHyDE != want[route] {
			t.Fatalf("route %q resolved HyDE=%v, want %v", route, gotHyDE, want[route])
		}
	}
}

// TestRouting897_ClassificationIsDeterministic pins that the classifier is a
// function of the question alone. The table is an ordered slice, never a map, so
// a repeated call cannot pick a different first match.
func TestRouting897_ClassificationIsDeterministic(t *testing.T) {
	for route, question := range routeQuestions897 {
		first := retrieval.ClassifyQuestion(question)
		for i := 0; i < 50; i++ {
			if got := retrieval.ClassifyQuestion(question); got != first {
				t.Fatalf("question %q classified as %q then %q on call %d", question, first, got, i)
			}
		}
		if first != route {
			t.Fatalf("question %q classified as %q, want %q", question, first, route)
		}
	}
}

// TestRouting897_UnclassifiableFallsBackToDefault pins the fallback: a question
// that matches no pattern takes the default route and inherits the server's
// global HyDE setting rather than erroring or guessing.
func TestRouting897_UnclassifiableFallsBackToDefault(t *testing.T) {
	svc := newRoutingService897(t, &fakeGenerator{out: "hypothetical"})

	for _, question := range []string{
		"Summarize the broadcast.",
		"",
		"   ",
		"tell me about it",
	} {
		route, hyde := resolvedHyDE897(t, svc, question)
		if route != retrieval.RouteDefault {
			t.Fatalf("question %q routed to %q, want %q", question, route, retrieval.RouteDefault)
		}
		if hyde {
			t.Fatalf("the default route must inherit the global HyDE setting (off here) for %q", question)
		}
	}

	// With the global setting ON, the same default route must inherit ON.
	svc.SetHyDE(true, "fuse")
	if _, hyde := resolvedHyDE897(t, svc, "Summarize the broadcast."); !hyde {
		t.Fatal("the default route must inherit a global HyDE of on")
	}
}

// TestRouting897_DisabledIsTodaysBehaviour is the regression that matters most.
// An operator who configures nothing must get the pre-#897 path: the global HyDE
// decision serves every question, whatever shape it has.
func TestRouting897_DisabledIsTodaysBehaviour(t *testing.T) {
	// Global HyDE off, routing never enabled: a superlative must NOT get HyDE.
	svc := newRoutingService897(t, &fakeGenerator{out: "hypothetical"})
	svc.SetQuestionRouting(false, nil)
	route, hyde := resolvedHyDE897(t, svc, routeQuestions897[retrieval.RouteSuperlative])
	if route != retrieval.RouteDefault {
		t.Fatalf("with routing off no classification runs; got route %q", route)
	}
	if hyde {
		t.Fatal("with routing off a superlative must follow the global HyDE setting (off)")
	}

	// Global HyDE on, routing off: a negative control must STILL get HyDE, because
	// that is what the un-routed server does today.
	svc.SetHyDE(true, "fuse")
	if _, hyde := resolvedHyDE897(t, svc, routeQuestions897[retrieval.RouteNegativeControl]); !hyde {
		t.Fatal("with routing off a negative control must follow the global HyDE setting (on)")
	}
}

// TestRouting897_DefaultConfigLeavesRoutingOff pins the config default: nothing
// configured means routing off and no route table, so the shipped table is never
// consulted.
func TestRouting897_DefaultConfigLeavesRoutingOff(t *testing.T) {
	cfg := config.Default()
	if cfg.RetrievalQuestionRoutingEnabled {
		t.Fatal("retrieval.question_routing.enabled must default to false")
	}
	if len(cfg.RetrievalQuestionRoutingHyDERoutes) != 0 {
		t.Fatalf("retrieval.question_routing.hyde_routes must default to empty, got %v",
			cfg.RetrievalQuestionRoutingHyDERoutes)
	}
}

// TestRouting897_RouteDrivesTheGenerator pins that the resolved profile actually
// reaches retrieval, not just the resolver. A superlative runs the HyDE
// generation; a negative control does not, under one service and one call each.
func TestRouting897_RouteDrivesTheGenerator(t *testing.T) {
	for _, tc := range []struct {
		name        string
		question    string
		wantHyDEGen bool
	}{
		{"superlative", routeQuestions897[retrieval.RouteSuperlative], true},
		{"negative control", routeQuestions897[retrieval.RouteNegativeControl], false},
		{"enumeration", routeQuestions897[retrieval.RouteEnumeration], false},
		{"time scoped", routeQuestions897[retrieval.RouteTimeScoped], false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gen := &recordingGenerator{out: "a hypothetical passage"}
			svc := newRoutingService897(t, gen)

			if _, err := svc.Search(context.Background(), model.SearchQuery{Query: tc.question, Index: "text", K: 5}); err != nil {
				t.Fatalf("Search: %v", err)
			}
			if tc.wantHyDEGen && gen.calls == 0 {
				t.Fatalf("route with HyDE on must generate a hypothesis; calls=%d", gen.calls)
			}
			if !tc.wantHyDEGen && gen.calls != 0 {
				t.Fatalf("route with HyDE off must not generate a hypothesis; calls=%d", gen.calls)
			}
		})
	}
}

// TestRouting897_ProfileCannotWeakenTheInjectionGuard pins that no route can
// reach the #885 guard. RouteProfile has no prompt field, so the attempt cannot
// even be expressed; this asserts the consequence end to end, under the route
// that turns the most machinery on.
func TestRouting897_ProfileCannotWeakenTheInjectionGuard(t *testing.T) {
	const operator = "You answer questions about a baseball broadcast."

	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	gen := &fakeGenerator{out: "ok [game/broadcast.vtt]"}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	svc.SetChunkMetadata(1, model.SearchHit{
		ChunkID: 1,
		RelPath: "game/broadcast.vtt",
		Snippet: "the fastest pitch was 101 mph",
		Span:    model.Span{Kind: "lines", StartLine: 1, EndLine: 2},
	})
	svc.SetRAGSystemPrompt(operator)
	svc.SetQuestionRouting(true, retrieval.DefaultQuestionRouteTable())

	question := routeQuestions897[retrieval.RouteSuperlative]
	if route := retrieval.ClassifyQuestion(question); route != retrieval.RouteSuperlative {
		t.Fatalf("precondition: question must route to %q, got %q", retrieval.RouteSuperlative, route)
	}
	if _, err := svc.Ask(context.Background(), question, model.SearchQuery{K: 1}); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	system, _ := promptRegions880(t, gen.lastPrompt)
	if !strings.Contains(system, operator) {
		t.Fatalf("the operator rules were lost under routing: %q", system)
	}
	assertGuard885(t, system)
}

// TestRouting897_ProfileCannotWeakenAbstention pins SPEC §9.4.3 under routing.
// The route turns HyDE on, so routing changed what was retrieved; the absolute
// evidence threshold must still refuse to answer from it.
func TestRouting897_ProfileCannotWeakenAbstention(t *testing.T) {
	idx := index.NewHNSWIndex("")
	// Near-orthogonal to the query vector: cosine ~0.02, under the shipped
	// absolute threshold, while still being the best hit in the set.
	addVec(t, idx, 1, []float32{0.02, 1})

	gen := &fakeGenerator{out: "a confident, sourced-looking answer [game/weak.vtt]"}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	svc.SetChunkMetadata(1, model.SearchHit{
		ChunkID: 1,
		RelPath: "game/weak.vtt",
		Snippet: "barely related text",
		Span:    model.Span{Kind: "lines", StartLine: 1, EndLine: 2},
	})
	svc.SetQuestionRouting(true, retrieval.DefaultQuestionRouteTable())

	got, err := svc.Ask(context.Background(), routeQuestions897[retrieval.RouteSuperlative], model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(got.Citations) != 0 {
		t.Fatalf("abstention under routing must return an empty citations array, got %d", len(got.Citations))
	}
	if !strings.Contains(got.Answer, "Insufficient evidence") {
		t.Fatalf("expected an explicit insufficient-evidence answer under routing, got %q", got.Answer)
	}
	if len(got.Hits) == 0 {
		t.Fatal("abstention must still report the rejected candidates in hits")
	}
}

// TestRouting897_TableRejectsAnUnknownRoute pins the closed vocabulary. A typo is
// an error, not a silent no-op that disables routing for one question shape.
func TestRouting897_TableRejectsAnUnknownRoute(t *testing.T) {
	for _, name := range []string{"superlatives", "SUPER", "hyde", "min_score"} {
		if _, err := retrieval.NewQuestionRouteTable([]string{name}); err == nil {
			t.Fatalf("route name %q must be rejected", name)
		}
	}
	if _, err := retrieval.NewQuestionRouteTable([]string{"superlative", "time_scoped"}); err != nil {
		t.Fatalf("valid route names must be accepted: %v", err)
	}
	// A blank entry is padding in a YAML list, not a typo: it is skipped.
	if _, err := retrieval.NewQuestionRouteTable([]string{"", "  ", "superlative"}); err != nil {
		t.Fatalf("a blank entry must be skipped, not rejected: %v", err)
	}
}

// TestRouting897_DefaultRouteIsNotConfigurable pins that the fallback route
// cannot be named in the table. Its profile always inherits, which is what makes
// "an unclassifiable question behaves as it does today" a guarantee rather than a
// default someone can edit away.
func TestRouting897_DefaultRouteIsNotConfigurable(t *testing.T) {
	if _, ok := retrieval.ParseQuestionRoute(string(retrieval.RouteDefault)); ok {
		t.Fatal("the default route must not parse as a configurable route name")
	}
	if _, err := retrieval.NewQuestionRouteTable([]string{string(retrieval.RouteDefault)}); err == nil {
		t.Fatal("naming the default route in hyde_routes must be rejected")
	}
}

// TestRouting897_ConfigAndRetrievalVocabulariesAgree pins the two copies of the
// closed route set together. internal/retrieval imports internal/config, so
// config cannot import the enum back and validates against its own list; this
// test is the fence that stops the two drifting.
func TestRouting897_ConfigAndRetrievalVocabulariesAgree(t *testing.T) {
	fromConfig := config.QuestionRoutingRouteNames()
	fromRetrieval := retrieval.ConfigurableQuestionRouteNames()
	if strings.Join(fromConfig, ",") != strings.Join(fromRetrieval, ",") {
		t.Fatalf("route vocabularies drifted: config=%v retrieval=%v", fromConfig, fromRetrieval)
	}
	for _, name := range fromConfig {
		if _, ok := retrieval.ParseQuestionRoute(name); !ok {
			t.Fatalf("config accepts route %q that retrieval does not parse", name)
		}
	}
}

// TestRouting897_MisclassificationDegradesTowardTodaysBehaviour pins the
// classification order that gives the SHIPPED table its safety property: the two
// routes the shipped table turns HyDE on for are matched last, so a question
// carrying two shapes takes the more HyDE-averse one. The property belongs to
// that table, not to the classifier: a custom hyde_routes naming an
// earlier-matching route gives it up, which is why this test asserts the ORDER
// and the shipped-table test above asserts the profiles.
func TestRouting897_MisclassificationDegradesTowardTodaysBehaviour(t *testing.T) {
	for _, tc := range []struct {
		question string
		want     retrieval.QuestionRoute
	}{
		// Superlative wording plus an absence marker: absence wins.
		{"Which pitcher never gave up a hit?", retrieval.RouteNegativeControl},
		// Superlative wording plus a time marker: the time window wins.
		{"What was the fastest pitch during the first half?", retrieval.RouteTimeScoped},
		// Superlative wording plus an enumeration marker: enumeration wins.
		{"List the fastest pitches.", retrieval.RouteEnumeration},
	} {
		if got := retrieval.ClassifyQuestion(tc.question); got != tc.want {
			t.Fatalf("question %q classified as %q, want %q", tc.question, got, tc.want)
		}
	}
}

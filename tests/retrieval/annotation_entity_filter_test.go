package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// The recognition entity/event filter (dirstral-spec design 0004 §7).
//
// Why it exists rather than matching entity labels in the annotation text: the
// text approach was measured on a real corpus and degrades retrieval
// monotonically (design 0004 §6.1, 58.3% -> 47.8% -> 34.8% precision). Every
// annotation of a moment names BOTH clubs, so the label cannot discriminate
// between candidates; it only drags a team-scoped query onto whichever role
// ranks first. "Giants home run" came back as a Giants pitcher throwing balls.
//
// The fixture below is that situation exactly: one moment, reported twice, once
// keyed on the pitcher with the fielding club and once on the batter with the
// club at the plate. Role lives in annotation granularity because v1's wire
// shape has a flat entity array and one scalar event, so nothing inside a single
// annotation can say which id acted.

const (
	giantsID = "team:san-francisco-giants"
	natsID   = "team:washington-nationals"
	rayID    = "player:robbie-ray"
	crewsID  = "player:dylan-crews"
)

// addAnnotation upserts a vector whose payload carries a recognition
// annotation's attribution, mirroring what ingestion persists so the index-side
// filter (model.Filter.Match over the payload) sees the same thing retrieval's
// post-materialisation re-check does.
func addAnnotation(
	t *testing.T, idx *index.HNSWIndex, id uint64, vec []float32,
	event string, entities []string, startMS, endMS int,
) {
	t.Helper()
	span := model.Span{
		Kind: "time", StartMS: startMS, EndMS: endMS,
		Entities: entities, Event: event,
	}
	payload := model.IndexPayload{
		ChunkID: id, RelPath: "game.mp4", DocType: "video",
		StartMS: startMS, EndMS: endMS, Span: span,
	}
	if err := idx.Upsert(context.Background(), vec, payload); err != nil {
		t.Fatalf("Upsert(%d): %v", id, err)
	}
}

// annotationService builds the two-role fixture: chunk 1 is the pitch (keyed on
// the pitcher, fielding club), chunk 2 the at-bat (keyed on the batter, club at
// the plate). Chunk 3 is an ordinary text chunk with no attribution at all.
func annotationService(t *testing.T) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	addAnnotation(t, idx, 1, []float32{1, 0}, "pitch", []string{rayID, giantsID}, 20300, 28300)
	addAnnotation(t, idx, 2, []float32{0.99, 0.01}, "at_bat", []string{crewsID, natsID}, 20300, 28300)
	addVecP(t, idx, 3, []float32{0.9, 0.1}, "notes.md", "md")

	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetChunkMetadata(1, model.SearchHit{
		RelPath: "game.mp4", DocType: "video", Snippet: "Pitch: Robbie Ray to Dylan Crews",
		Span: model.Span{
			Kind: "time", StartMS: 20300, EndMS: 28300,
			Entities: []string{rayID, giantsID}, Event: "pitch",
		},
	})
	svc.SetChunkMetadata(2, model.SearchHit{
		RelPath: "game.mp4", DocType: "video", Snippet: "At bat: Dylan Crews vs Robbie Ray",
		Span: model.Span{
			Kind: "time", StartMS: 20300, EndMS: 28300,
			Entities: []string{crewsID, natsID}, Event: "at_bat",
		},
	})
	svc.SetChunkMetadata(3, model.SearchHit{
		RelPath: "notes.md", DocType: "md", Snippet: "an ordinary note",
		Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 3},
	})
	return svc
}

func searchIDs(t *testing.T, svc *retrieval.Service, q model.SearchQuery) []uint64 {
	t.Helper()
	q.Query = "moment"
	if q.K == 0 {
		q.K = 10
	}
	hits, err := svc.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	out := make([]uint64, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ChunkID)
	}
	return out
}

// TestEntityAndEventTogetherSelectOneRole is the whole point. The club is
// present on both annotations of the moment, so the club ALONE cannot express
// "the Giants batting" — and neither can any phrasing of the text. Conjoining
// the event does, because event records the role the id is acting in.
func TestEntityAndEventTogetherSelectOneRole(t *testing.T) {
	svc := annotationService(t)

	// The Giants field this half-inning, so they appear on the pitch cue only.
	if got := searchIDs(t, svc, model.SearchQuery{Entities: []string{giantsID}}); len(got) != 1 || got[0] != 1 {
		t.Fatalf("entities=[giants] = %v, want [1] (the pitch cue)", got)
	}
	// Pairing them with the batting role must select nothing: they are not at
	// the plate here. This is the case a text label gets wrong.
	if got := searchIDs(t, svc, model.SearchQuery{
		Entities: []string{giantsID}, Events: []string{"at_bat"},
	}); len(got) != 0 {
		t.Fatalf("entities=[giants] events=[at_bat] = %v, want none", got)
	}
	// The club actually at the plate, with the same event, does select.
	if got := searchIDs(t, svc, model.SearchQuery{
		Entities: []string{natsID}, Events: []string{"at_bat"},
	}); len(got) != 1 || got[0] != 2 {
		t.Fatalf("entities=[nationals] events=[at_bat] = %v, want [2]", got)
	}
}

// TestValuesWithinAFieldAreOr pins the OR half of the semantics.
func TestValuesWithinAFieldAreOr(t *testing.T) {
	svc := annotationService(t)
	got := searchIDs(t, svc, model.SearchQuery{Entities: []string{giantsID, natsID}})
	if len(got) != 2 {
		t.Fatalf("entities=[giants,nationals] = %v, want both annotations", got)
	}
	events := searchIDs(t, svc, model.SearchQuery{Events: []string{"pitch", "at_bat"}})
	if len(events) != 2 {
		t.Fatalf("events=[pitch,at_bat] = %v, want both annotations", events)
	}
}

// TestFieldsAreAndedTogether: entities AND events, never OR. Were it OR, the
// pitch cue would survive a filter that asks for the batting role.
func TestFieldsAreAndedTogether(t *testing.T) {
	svc := annotationService(t)
	got := searchIDs(t, svc, model.SearchQuery{
		Entities: []string{rayID},    // on the pitch cue
		Events:   []string{"at_bat"}, // on the OTHER cue
	})
	if len(got) != 0 {
		t.Fatalf("a filter satisfied by neither annotation returned %v", got)
	}
}

// TestAChunkWithNoAttributionNeverMatches mirrors the speaker filter's rule: a
// corpus with no recognition annotations returns nothing for a non-empty
// filter, rather than falling back to unfiltered results.
func TestAChunkWithNoAttributionNeverMatches(t *testing.T) {
	svc := annotationService(t)
	for _, q := range []model.SearchQuery{
		{Entities: []string{"team:nobody"}},
		{Events: []string{"touchdown"}},
	} {
		if got := searchIDs(t, svc, q); len(got) != 0 {
			t.Fatalf("%+v matched %v; the plain text chunk must not slip through", q, got)
		}
	}
}

// TestAnAbsentFilterIsAPassThrough is the compatibility guard: every caller
// that sends neither field must see exactly what it saw before.
func TestAnAbsentFilterIsAPassThrough(t *testing.T) {
	svc := annotationService(t)
	if got := searchIDs(t, svc, model.SearchQuery{}); len(got) != 3 {
		t.Fatalf("unfiltered search = %v, want all three chunks", got)
	}
}

// TestMatchingIsLiteralNotCaseFolded: ids and event strings are opaque tokens a
// backend declares, so folding case could collide two it considers distinct.
// This is a deliberate difference from the speaker filter, which IS folded.
func TestMatchingIsLiteralNotCaseFolded(t *testing.T) {
	svc := annotationService(t)
	if got := searchIDs(t, svc, model.SearchQuery{Entities: []string{"TEAM:SAN-FRANCISCO-GIANTS"}}); len(got) != 0 {
		t.Fatalf("a differently-cased id matched %v; ids are opaque tokens", got)
	}
	if got := searchIDs(t, svc, model.SearchQuery{Events: []string{"PITCH"}}); len(got) != 0 {
		t.Fatalf("a differently-cased event matched %v", got)
	}
}

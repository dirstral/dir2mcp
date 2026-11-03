package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// The source words must reach the translate path through BOTH callers (the
// inline transcription path and the two-phase derivation pass): a dropped
// argument would silently fall back to next-start spans, which this test would
// catch because the stored translated span would then end at 10000, not 1200.
func TestTranslatedTranscript_CarriesSpokenSourceEnds(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"inline", "derivation"} {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			st := &fakeIngestStore{}
			svc := mustNewIngestService(t, config.Config{StateDir: testutil.TempDir(t)}, st)
			svc.SetTranscriber(&fakeStructuredTranscriber{
				text: "[00:00] раз два\n[00:10] три",
				words: []model.TimedWord{
					{Word: "раз", StartMS: 0, EndMS: 500},
					{Word: "два", StartMS: 600, EndMS: 1200},
					{Word: "три", StartMS: 10000, EndMS: 10800},
				},
			})
			svc.SetTranslator(&fakeTranslator{}, "mistral", "m1", []string{"en"})
			doc := model.Document{DocID: 91, RelPath: "audio/gap.mp3", DocType: "audio"}
			content := []byte("audio-with-a-gap-" + path)

			var err error
			if path == "inline" {
				err = svc.GenerateTranscriptRepresentation(context.Background(), doc, content)
			} else {
				err = svc.DeriveTranscriptTranslationsForTest(context.Background(), doc, content)
			}
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}

			// The translated representation is the one whose meta records
			// source=translation; its chunks carry its rep id.
			var repID int64
			for i, rep := range st.reps {
				if strings.Contains(rep.MetaJSON, `"source":"translation"`) {
					repID = int64(i + 1)
					if !strings.Contains(rep.MetaJSON, `"span_rule":"source-spoken"`) {
						t.Fatalf("%s: translated meta lacks span_rule: %s", path, rep.MetaJSON)
					}
				}
			}
			if repID == 0 {
				t.Fatalf("%s: no translated representation stored; reps=%+v", path, st.reps)
			}
			var spans []model.Span
			for i, ch := range st.chunks {
				if ch.RepID == repID {
					spans = append(spans, st.spans[i])
				}
			}
			if len(spans) != 2 {
				t.Fatalf("%s: translated spans = %d, want 2: %+v", path, len(spans), spans)
			}
			if spans[0].StartMS != 0 || spans[0].EndMS != 1200 {
				t.Fatalf("%s: first translated span [%d,%d], want [0,1200] (spoken end, not the next line's start)", path, spans[0].StartMS, spans[0].EndMS)
			}
			if spans[1].StartMS != 10000 || spans[1].EndMS < 10800 {
				t.Fatalf("%s: second translated span [%d,%d], want start 10000 and end >= 10800", path, spans[1].StartMS, spans[1].EndMS)
			}
		})
	}
}

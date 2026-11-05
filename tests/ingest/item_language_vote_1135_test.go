package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// leadingPassageTranscript builds a `[mm:ss] text` transcript whose first
// leadBytes hold English lines (a jingle or intro decoded as English) and whose
// remaining lines, about three times as long, are Ukrainian speech. It is the
// shape of a three-window broadcast decode whose first window reported the
// wrong language (dir2mcp #1135).
func leadingPassageTranscript(leadBytes int) string {
	english := []string{
		"Welcome to the evening programme, stay with us for the news of the day.",
		"This broadcast is brought to you by our partners and our viewers.",
		"Music and headlines first, then the full interview after the break.",
	}
	ukrainian := []string{
		"Сьогодні ми говоримо про те, як змінилася країна за останні роки.",
		"Міністр відповів, що уряд готує нові рішення для підтримки людей.",
		"Це питання дуже важливе для кожної родини, яка живе в нашому місті.",
		"Ми продовжимо розмову після короткої перерви, залишайтеся з нами.",
	}
	var b strings.Builder
	sec := 0
	line := func(text string) {
		fmt.Fprintf(&b, "[%02d:%02d] %s\n", sec/60, sec%60, text)
		sec += 4
	}
	for i := 0; b.Len() < leadBytes; i++ {
		line(english[i%len(english)])
	}
	for i := 0; b.Len() < leadBytes*4; i++ {
		line(ukrainian[i%len(ukrainian)])
	}
	return b.String()
}

// TestTranscriptLanguage_LeadingPassageDoesNotDecideItem is the #1135
// regression: a transcript whose first 5 KiB are English and whose other three
// quarters are Ukrainian records the item language as Ukrainian. Before the fix
// the detector read the first 4 KiB only and recorded English.
func TestTranscriptLanguage_LeadingPassageDoesNotDecideItem(t *testing.T) {
	t.Parallel()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "broadcast.mp3"), "fake-audio")
	st := newRealStore(t)
	text := leadingPassageTranscript(5 * 1024)
	svc := sttServiceWithDetection(t, root, testutil.TempDir(t), st, "whisper", "whisper-large-v3", "", &fakeTranscriber{text: text})

	f := ingest.DiscoveredFile{RelPath: "broadcast.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(transcriptMetaFor(t, st, "broadcast.mp3")), &p); err != nil {
		t.Fatalf("meta not json: %v", err)
	}
	if p["language"] != "uk" {
		t.Errorf("language = %v, want uk: the English first window must not decide the item", p["language"])
	}
	if p["language_source"] != "detected" {
		t.Errorf("language_source = %v, want detected", p["language_source"])
	}
}

package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TestShiftTranscriptSpans_ShiftsAndClamps verifies the pure span shift used by
// the leading-silence trim (dir2mcp#258): every "time" span's bounds and word
// timestamps are reduced by the offset and clamped at 0.
func TestShiftTranscriptSpans_ShiftsAndClamps(t *testing.T) {
	t.Parallel()
	const transcript = "[00:00] intro\n[00:02] chapter one\n[00:05] chapter two"
	words := []model.TimedWord{
		{Word: "intro", StartMS: 0, EndMS: 800},
		{Word: "chapter", StartMS: 2000, EndMS: 2500},
		{Word: "two", StartMS: 5400, EndMS: 5800},
	}
	base := ingest.ChunkTranscriptByTimeWithWords(transcript, words)

	const offsetMS = 1500
	shifted := ingest.ShiftTranscriptSpans(base, offsetMS)

	if len(shifted) != len(base) {
		t.Fatalf("shift changed chunk count: %d vs %d", len(shifted), len(base))
	}
	for i := range base {
		if shifted[i].Text != base[i].Text {
			t.Errorf("chunk[%d] text changed by shift: %q vs %q", i, shifted[i].Text, base[i].Text)
		}
		if base[i].Span.Kind != "time" {
			continue
		}
		wantStart := clamp(base[i].Span.StartMS - offsetMS)
		wantEnd := clamp(base[i].Span.EndMS - offsetMS)
		if wantEnd <= wantStart {
			wantEnd = wantStart + 1
		}
		if shifted[i].Span.StartMS != wantStart || shifted[i].Span.EndMS != wantEnd {
			t.Errorf("chunk[%d] span = [%d,%d), want [%d,%d)", i,
				shifted[i].Span.StartMS, shifted[i].Span.EndMS, wantStart, wantEnd)
		}
		for w := range base[i].Span.Words {
			wantT := clamp(base[i].Span.Words[w].T - offsetMS)
			if shifted[i].Span.Words[w].T != wantT {
				t.Errorf("chunk[%d] word[%d] T = %d, want %d", i, w, shifted[i].Span.Words[w].T, wantT)
			}
		}
	}

	// First span started at 0 -> clamps to 0, never negative.
	if shifted[0].Span.StartMS < 0 {
		t.Errorf("first span start went negative: %d", shifted[0].Span.StartMS)
	}

	// The input must not be mutated by the exported wrapper.
	if base[1].Span.StartMS != 2000 {
		t.Errorf("input span was mutated: got %d, want 2000", base[1].Span.StartMS)
	}
}

func TestShiftTranscriptSpans_ZeroOffsetIsNoOp(t *testing.T) {
	t.Parallel()
	base := ingest.ChunkTranscriptByTime("[00:00] intro\n[00:02] chapter one")
	for _, off := range []int{0, -100} {
		got := ingest.ShiftTranscriptSpans(base, off)
		for i := range base {
			if got[i].Span.StartMS != base[i].Span.StartMS || got[i].Span.EndMS != base[i].Span.EndMS {
				t.Fatalf("offset %d shifted spans: %+v vs %+v", off, got[i].Span, base[i].Span)
			}
		}
	}
}

// TestGenerateTranscript_LeadingSilenceTrimEnabled exercises the full ingest
// path with the trim enabled and a stubbed detector reporting 2s of leading
// silence; every persisted time span must be shifted back by 2000ms (clamped).
func TestGenerateTranscript_LeadingSilenceTrimEnabled(t *testing.T) {
	t.Parallel()
	st := &fakeIngestStore{}
	root, content := mediaDocRoot(t)
	svc := mustNewIngestService(t, config.Config{
		StateDir:                t.TempDir(),
		RootDir:                 root,
		MediaTrimLeadingSilence: true,
	}, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] intro\n[00:02] chapter one\n[00:05] chapter two"})
	svc.DetectLeadingSilenceFunc = func(context.Context, string) (time.Duration, error) {
		return 2 * time.Second, nil
	}

	doc := model.Document{DocID: 101, RelPath: "audio/lecture.mp3", DocType: "audio"}
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, content); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation failed: %v", err)
	}

	if len(st.spans) != 3 {
		t.Fatalf("expected 3 spans, got %d", len(st.spans))
	}
	// Baseline spans are [0,2000) [2000,5000) [5000,6000); a 2000ms trim yields
	// [0,1) (clamped+widened) [0,3000) [3000,4000).
	want := [][2]int{{0, 1}, {0, 3000}, {3000, 4000}}
	for i, w := range want {
		if st.spans[i].StartMS != w[0] || st.spans[i].EndMS != w[1] {
			t.Errorf("span[%d] = [%d,%d), want [%d,%d)", i, st.spans[i].StartMS, st.spans[i].EndMS, w[0], w[1])
		}
	}
}

// TestGenerateTranscript_LeadingSilenceTrimDisabled asserts spans are untouched
// when the toggle is off, even if a detector would report silence.
func TestGenerateTranscript_LeadingSilenceTrimDisabled(t *testing.T) {
	t.Parallel()
	st := &fakeIngestStore{}
	root, content := mediaDocRoot(t)
	svc := mustNewIngestService(t, config.Config{
		StateDir: t.TempDir(),
		RootDir:  root,
		// MediaTrimLeadingSilence defaults to false.
	}, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] intro\n[00:02] chapter one\n[00:05] chapter two"})
	called := false
	svc.DetectLeadingSilenceFunc = func(context.Context, string) (time.Duration, error) {
		called = true
		return 2 * time.Second, nil
	}

	doc := model.Document{DocID: 102, RelPath: "audio/lecture.mp3", DocType: "audio"}
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, content); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation failed: %v", err)
	}
	if called {
		t.Error("detector was invoked while trim is disabled")
	}
	assertSpanWindows(t, st, [][2]int{{0, 2000}, {2000, 5000}, {5000, 6000}})
}

// TestGenerateTranscript_LeadingSilenceDetectorError asserts a detector error or
// absent ffmpeg (0 offset) leaves spans unchanged — graceful, never fatal.
func TestGenerateTranscript_LeadingSilenceDetectorError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		fn   func(context.Context, string) (time.Duration, error)
	}{
		{"detector error", func(context.Context, string) (time.Duration, error) {
			return 0, errors.New("ffmpeg blew up")
		}},
		{"tool absent / no silence", func(context.Context, string) (time.Duration, error) {
			return 0, nil
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := &fakeIngestStore{}
			root, content := mediaDocRoot(t)
			svc := mustNewIngestService(t, config.Config{
				StateDir:                t.TempDir(),
				RootDir:                 root,
				MediaTrimLeadingSilence: true,
			}, st)
			svc.SetTranscriber(&fakeTranscriber{text: "[00:00] intro\n[00:02] chapter one\n[00:05] chapter two"})
			svc.DetectLeadingSilenceFunc = tc.fn

			doc := model.Document{DocID: 103, RelPath: "audio/lecture.mp3", DocType: "audio"}
			if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, content); err != nil {
				t.Fatalf("GenerateTranscriptRepresentation failed: %v", err)
			}
			assertSpanWindows(t, st, [][2]int{{0, 2000}, {2000, 5000}, {5000, 6000}})
		})
	}
}

// mediaDocRoot creates a temp corpus root containing audio/lecture.mp3 so the
// service's CorpusFS.Localize resolves cleanly, and returns the root plus the
// file content used as the transcribe input.
func mediaDocRoot(t *testing.T) (string, []byte) {
	t.Helper()
	root := t.TempDir()
	content := []byte("fake-audio-bytes")
	mediaPath := filepath.Join(root, "audio", "lecture.mp3")
	if err := os.MkdirAll(filepath.Dir(mediaPath), 0o755); err != nil {
		t.Fatalf("mkdir media dir: %v", err)
	}
	if err := os.WriteFile(mediaPath, content, 0o644); err != nil {
		t.Fatalf("write media file: %v", err)
	}
	return root, content
}

func assertSpanWindows(t *testing.T, st *fakeIngestStore, want [][2]int) {
	t.Helper()
	if len(st.spans) != len(want) {
		t.Fatalf("expected %d spans, got %d", len(want), len(st.spans))
	}
	for i, w := range want {
		if st.spans[i].StartMS != w[0] || st.spans[i].EndMS != w[1] {
			t.Errorf("span[%d] = [%d,%d), want [%d,%d)", i, st.spans[i].StartMS, st.spans[i].EndMS, w[0], w[1])
		}
	}
}

func clamp(ms int) int {
	if ms < 0 {
		return 0
	}
	return ms
}

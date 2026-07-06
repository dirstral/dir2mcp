package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dirstral/dir2mcp/internal/avutil"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TranslateWindow is one decoded audio window: the offset (ms from the start of
// the whole recording) at which the window began, and the window-local translate
// result (its timestamps start at 0 for the window). Exported so the pure merge
// logic can be unit-tested from the tests/ tree (AGENTS.md: no new _test.go under
// internal/).
type TranslateWindow struct {
	StartMS int
	Res     model.TranscriptResult
}

// WhisperTranslateOverlapMS derives the overlap between consecutive decode windows
// from the window length: enough lookahead that a sentence straddling a boundary
// is fully decoded in the window it starts in, capped so the overlap never
// dominates the window. Deriving it (rather than exposing a second knob) keeps the
// public surface to a single media.translate.whisper_window_sec setting.
func WhisperTranslateOverlapMS(windowMS int) int {
	overlap := windowMS / 5
	if overlap > 10000 {
		overlap = 10000
	}
	if overlap < 0 {
		overlap = 0
	}
	return overlap
}

// MergeTranslateWindows stitches per-window translate results (each in
// window-local time) into one transcript in absolute time. Each window's segment
// lines and words are offset by the window's start, then de-duplicated against the
// overlap by keeping only those whose absolute start falls in the window's CORE
// [startMS, startMS+stepMS); the final window keeps everything through the end.
// The result is a segment-formatted transcript string (the same `[mm:ss] text`
// shape a single decode returns) plus the flat, time-ordered word list.
//
// It is a pure function of its inputs so the windowing/merge logic is unit-tested
// without a live transcriber.
func MergeTranslateWindows(windows []TranslateWindow, stepMS int) (string, []model.TimedWord) {
	if stepMS <= 0 {
		stepMS = 1
	}
	var lines []string
	var words []model.TimedWord
	for i, w := range windows {
		coreEnd := w.StartMS + stepMS
		last := i == len(windows)-1

		for _, wd := range w.Res.Words {
			abs := wd.StartMS + w.StartMS
			if abs < w.StartMS {
				continue
			}
			if !last && abs >= coreEnd {
				continue
			}
			words = append(words, model.TimedWord{
				Word:    wd.Word,
				StartMS: abs,
				EndMS:   wd.EndMS + w.StartMS,
			})
		}

		for _, line := range strings.Split(w.Res.Text, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			startMS, body, ok := parseTranscriptTimestamp(trimmed)
			if !ok {
				// A line with no timestamp only carries meaning in the first window;
				// later windows re-decode the same head text, so drop it as a dup.
				if i == 0 {
					lines = append(lines, trimmed)
				}
				continue
			}
			if strings.TrimSpace(body) == "" {
				continue
			}
			abs := startMS + w.StartMS
			if !last && abs >= coreEnd {
				continue
			}
			lines = append(lines, formatTimestampMarker(abs)+" "+body)
		}
	}
	sort.SliceStable(words, func(a, b int) bool { return words[a].StartMS < words[b].StartMS })
	return strings.Join(lines, "\n"), words
}

// translateStructuredWindowed is the media.translate.whisper_window_sec-aware
// wrapper around translateStructured. With no window configured (<= 0) it is a
// straight pass-through, so existing corpora are unchanged. With a window it
// decodes the audio in overlapping windows via Whisper's translate task and
// merges them, so timestamp drift cannot accumulate across a long recording.
// Windowing requires the structured (word-timing) transcriber; when the
// transcriber is text-only it transparently falls back to the single-pass decode.
func (s *Service) translateStructuredWindowed(ctx context.Context, doc model.Document, content []byte) (string, []model.TimedWord, error) {
	windowMS := s.cfg.MediaTranslateWhisperWindowSec * 1000
	st, ok := s.translateSTT.(model.StructuredTranscriber)
	if windowMS <= 0 || !ok {
		return s.translateStructured(ctx, doc, content)
	}
	stepMS := windowMS - WhisperTranslateOverlapMS(windowMS)
	if stepMS <= 0 {
		stepMS = windowMS
	}

	// avutil slices by file path; the audio arrives as bytes, so stage a temp file.
	tmp, err := os.CreateTemp("", "dir2mcp-xlate-*"+filepath.Ext(doc.RelPath))
	if err != nil {
		return "", nil, fmt.Errorf("stage audio for windowed translate: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return "", nil, fmt.Errorf("write staged audio: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", nil, fmt.Errorf("flush staged audio: %w", err)
	}

	dur, err := avutil.Duration(ctx, tmpPath)
	if err != nil {
		return "", nil, fmt.Errorf("probe audio duration for windowed translate: %w", err)
	}
	totalMS := int(dur.Milliseconds())
	if totalMS <= 0 {
		return s.translateStructured(ctx, doc, content)
	}

	var windows []TranslateWindow
	for start := 0; start < totalMS; start += stepMS {
		end := start + windowMS
		if end > totalMS {
			end = totalMS
		}
		seg, err := avutil.ExtractSegment(ctx, tmpPath, start, end)
		if err != nil {
			return "", nil, fmt.Errorf("extract translate window [%d,%d]ms: %w", start, end, err)
		}
		res, err := st.TranscribeStructured(ctx, doc.RelPath, seg)
		if err != nil {
			return "", nil, fmt.Errorf("translate window [%d,%d]ms: %w", start, end, err)
		}
		windows = append(windows, TranslateWindow{StartMS: start, Res: res})
	}

	text, words := MergeTranslateWindows(windows, stepMS)
	return text, words, nil
}

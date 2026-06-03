// Package avutil isolates audio/video media handling behind a small surface so
// the rest of the codebase does not depend on the ffmpeg/ffprobe binaries
// directly. It mirrors internal/pdfutil's role for PDFs: duration probing for
// time-window chunking (SPEC 8.1.7) and segment extraction so a media chunk
// embeds exactly the bytes its `time` span cites.
//
// ffprobe/ffmpeg are external binaries; when they are absent the callers treat
// it as a graceful skip (keep the text path) rather than a hard failure, so
// ErrToolNotFound is returned distinctly.
package avutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrToolNotFound indicates the required external binary (ffprobe/ffmpeg) is not
// on PATH. Callers use it to distinguish "tool unavailable" (skip media, keep
// the text path) from a genuine decode error.
var ErrToolNotFound = errors.New("avutil: required media tool not found on PATH")

// msToSeconds formats a millisecond offset as a fractional-second string with
// millisecond precision, the form ffmpeg's -ss/-to accept.
func msToSeconds(ms int) string {
	return strconv.FormatFloat(float64(ms)/1000.0, 'f', 3, 64)
}

// Duration returns the media duration at path, probed via ffprobe. It returns
// ErrToolNotFound when ffprobe is not installed.
func Duration(ctx context.Context, path string) (time.Duration, error) {
	bin, err := exec.LookPath("ffprobe")
	if err != nil {
		return 0, ErrToolNotFound
	}
	cmd := exec.CommandContext(ctx, bin,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		"--", path,
	)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("ffprobe %q: %w: %s", path, err, strings.TrimSpace(stderr.String()))
	}
	raw := strings.TrimSpace(out.String())
	if raw == "" || raw == "N/A" {
		return 0, fmt.Errorf("ffprobe %q: no duration reported", path)
	}
	secs, perr := strconv.ParseFloat(raw, 64)
	if perr != nil {
		return 0, fmt.Errorf("ffprobe %q: parse duration %q: %w", path, raw, perr)
	}
	if secs <= 0 {
		return 0, fmt.Errorf("ffprobe %q: non-positive duration %g", path, secs)
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// ExtractSegment cuts the half-open window [startMS, endMS) from the media at
// path and returns the resulting container bytes. The output preserves the
// source container (stream copy, no re-encode) so the MIME type is unchanged
// and extraction is deterministic. It returns ErrToolNotFound when ffmpeg is
// not installed.
func ExtractSegment(ctx context.Context, path string, startMS, endMS int) ([]byte, error) {
	if startMS < 0 || endMS <= startMS {
		return nil, fmt.Errorf("avutil: invalid segment [%d,%d) for %q", startMS, endMS, path)
	}
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, ErrToolNotFound
	}

	tmpDir, err := os.MkdirTemp("", "dir2mcp-avseg-")
	if err != nil {
		return nil, fmt.Errorf("avutil: temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Keep the source extension so ffmpeg infers the matching muxer and the
	// extracted clip stays the same container/MIME as the source.
	ext := filepath.Ext(path)
	if ext == "" {
		ext = ".bin"
	}
	outPath := filepath.Join(tmpDir, "segment"+ext)

	cmd := exec.CommandContext(ctx, bin,
		"-nostdin",
		"-v", "error",
		"-ss", msToSeconds(startMS),
		"-to", msToSeconds(endMS),
		"-i", path,
		"-c", "copy",
		"-y", outPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg segment %q [%d,%d): %w: %s", path, startMS, endMS, err, strings.TrimSpace(stderr.String()))
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("avutil: read extracted segment: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("avutil: ffmpeg produced an empty segment for %q [%d,%d)", path, startMS, endMS)
	}
	return data, nil
}

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
	"encoding/json"
	"errors"
	"fmt"
	neturl "net/url"
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

// ErrNoAudioStream indicates the media at the given path carries no audio stream,
// so there is nothing to transcribe. Callers treat it as a graceful "no
// transcript" (not a hard failure): a silent film or a screen recording with no
// audio track simply produces no transcript representation (issue #495).
var ErrNoAudioStream = errors.New("avutil: media has no audio stream")

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

// MediaInfo holds the subset of ffprobe track metadata used to package a media
// presentation (SPEC §8.6.10 SMIL). It is best-effort: any field may be zero/
// empty when ffprobe does not report it, and callers MUST fail open (omit the
// metadata-dependent output) rather than treat a missing field as an error.
type MediaInfo struct {
	// Container is the demuxer/format name reported by ffprobe (e.g. "mov,mp4,
	// m4a,3gp,3g2,mj2"). Empty when unknown.
	Container string
	// VideoCodec / AudioCodec are the codec_name of the first video/audio stream
	// (e.g. "h264", "aac"). Empty when the corresponding stream is absent.
	VideoCodec string
	AudioCodec string
	// BitRateBPS is the overall container bit rate in bits per second, 0 when not
	// reported.
	BitRateBPS int
	// Width / Height are the first video stream's pixel dimensions, 0 for
	// audio-only media or when not reported.
	Width  int
	Height int
	// AudioStreams enumerates every audio stream in the container in ffprobe
	// order (issue #567). It is empty for media with no audio. AudioCodec mirrors
	// the first entry's codec for backward compatibility; consumers that must
	// account for additional tracks — multi-language dubs / a music-&-effects
	// track in broadcast/proxy media — read this slice instead of only AudioCodec.
	AudioStreams []AudioStream
}

// AudioStream describes a single audio stream discovered in a media container.
// It is the per-track census used to detect multi-track media (issue #567): the
// transcription path feeds only the first audio stream to STT, so a container
// bundling an original mix, per-language dubs, and a music-&-effects track would
// otherwise be transcribed from track 0 alone with no signal that the rest
// existed. All fields are best-effort — any may be zero/empty when ffprobe does
// not report it — and callers MUST fail open rather than treat a missing field
// as an error.
type AudioStream struct {
	// Index is the absolute ffprobe stream index within the container (across all
	// stream types), as reported by ffprobe. The position within
	// MediaInfo.AudioStreams gives the audio-relative order instead (0 == the
	// first audio stream, the one ffmpeg selects by default and the only one
	// currently transcribed; it is what `-map 0:a:0` selects).
	Index int
	// CodecName is the stream's codec_name (e.g. "aac"), empty when unreported.
	CodecName string
	// Channels is the channel count (1 mono, 2 stereo, …), 0 when unreported.
	Channels int
	// Language is the declared language metadata tag (ffprobe stream tag
	// `language`, e.g. "eng", "rus", "und"), empty when the container carries no
	// per-track language tag. It is only the tag the container declares — never
	// inferred or detected here — so it stays general-purpose (no hardcoded
	// language assumptions).
	Language string
	// Title is the declared stream `title` tag (e.g. "Music & Effects",
	// "Commentary") when present, empty otherwise. A common broadcast hint about a
	// track's role.
	Title string
}

// AudioStreamCount reports how many audio streams the probed media carries.
func (m MediaInfo) AudioStreamCount() int { return len(m.AudioStreams) }

// HasMultipleAudioStreams reports whether the media carries more than one audio
// stream — the multi-track case where transcribing only the first audio stream
// (the current behavior) silently drops the others (issue #567).
func (m MediaInfo) HasMultipleAudioStreams() bool { return len(m.AudioStreams) > 1 }

// HasVideo reports whether the probed media carries a video stream with usable
// dimensions, the precondition for emitting video width/height in SMIL.
func (m MediaInfo) HasVideo() bool {
	return m.Width > 0 && m.Height > 0
}

// ffprobeStream is the per-stream shape decoded from ffprobe -show_streams JSON.
type ffprobeStream struct {
	Index     int    `json:"index"`
	CodecType string `json:"codec_type"`
	CodecName string `json:"codec_name"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Channels  int    `json:"channels"`
	Tags      struct {
		Language string `json:"language"`
		Title    string `json:"title"`
	} `json:"tags"`
}

// ffprobeFormat is the container shape decoded from ffprobe -show_format JSON.
type ffprobeFormat struct {
	FormatName string `json:"format_name"`
	BitRate    string `json:"bit_rate"`
}

// ffprobeOutput is the top-level JSON ffprobe emits with -of json.
type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

// ProbeMediaInfo probes container/codec/bitrate/dimension metadata for the media
// at path via ffprobe's JSON output. It returns ErrToolNotFound when ffprobe is
// not installed so callers can fail open (omit SMIL, keep text subtitles per
// SPEC §8.6.10). A successful probe with some fields unreported yields a partial
// MediaInfo rather than an error: SMIL packaging is best-effort. The probe is
// deterministic for a given input.
func ProbeMediaInfo(ctx context.Context, path string) (MediaInfo, error) {
	bin, err := exec.LookPath("ffprobe")
	if err != nil {
		return MediaInfo{}, ErrToolNotFound
	}
	cmd := exec.CommandContext(ctx, bin,
		"-v", "error",
		"-show_entries", "format=format_name,bit_rate:stream=index,codec_type,codec_name,width,height,channels:stream_tags=language,title",
		"-of", "json",
		"--", path,
	)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return MediaInfo{}, fmt.Errorf("ffprobe %q: %w: %s", path, err, strings.TrimSpace(stderr.String()))
	}
	return parseMediaInfo(out.Bytes())
}

// parseMediaInfo decodes ffprobe -of json output into a MediaInfo. It is split
// out (and exported via ParseMediaInfo) so the JSON mapping can be unit-tested
// from the tests/ tree against captured ffprobe output without the binary. The
// first video stream wins for the codec/dimension fields, and AudioCodec mirrors
// the first audio stream, but every audio stream is enumerated into AudioStreams
// (in ffprobe order) so multi-track media is no longer silently reduced to track
// 0 (issue #567).
func parseMediaInfo(raw []byte) (MediaInfo, error) {
	var probed ffprobeOutput
	if err := json.Unmarshal(raw, &probed); err != nil {
		return MediaInfo{}, fmt.Errorf("avutil: parse ffprobe json: %w", err)
	}
	info := MediaInfo{Container: strings.TrimSpace(probed.Format.FormatName)}
	if br := strings.TrimSpace(probed.Format.BitRate); br != "" && br != "N/A" {
		if n, err := strconv.Atoi(br); err == nil && n > 0 {
			info.BitRateBPS = n
		}
	}
	for _, s := range probed.Streams {
		switch strings.ToLower(strings.TrimSpace(s.CodecType)) {
		case "video":
			if info.VideoCodec == "" {
				info.VideoCodec = strings.TrimSpace(s.CodecName)
				if s.Width > 0 && s.Height > 0 {
					info.Width = s.Width
					info.Height = s.Height
				}
			}
		case "audio":
			as := AudioStream{
				Index:     s.Index,
				CodecName: strings.TrimSpace(s.CodecName),
				Channels:  s.Channels,
				Language:  strings.TrimSpace(s.Tags.Language),
				Title:     strings.TrimSpace(s.Tags.Title),
			}
			info.AudioStreams = append(info.AudioStreams, as)
			if info.AudioCodec == "" {
				info.AudioCodec = as.CodecName
			}
		}
	}
	return info, nil
}

// ParseMediaInfo is the exported counterpart of parseMediaInfo, exposed so tests
// in the tests/ tree can verify ffprobe JSON decoding without the ffprobe binary.
func ParseMediaInfo(ffprobeJSON []byte) (MediaInfo, error) {
	return parseMediaInfo(ffprobeJSON)
}

// DefaultSilenceThresholdDB is the noise floor (in dBFS) below which audio is
// treated as silence by DetectLeadingSilence. It mirrors livevtt's
// archive_transcriber default (-40 dB): conservative enough that low-level room
// tone is not mistaken for speech, loose enough that genuine dead air is caught.
const DefaultSilenceThresholdDB = -40.0

// DefaultSilenceMinDuration is the minimum continuous silence (seconds) ffmpeg's
// silencedetect must observe before reporting a silence period. A short floor
// avoids treating sub-half-second gaps as leading silence.
const DefaultSilenceMinDuration = 0.5

// maxLeadingSilenceTrim bounds how much leading silence DetectLeadingSilence is
// willing to report. Mirrors livevtt's "< 5s" guard: trimming only the brief
// dead air typical before speech starts, never a long musical/ident intro that
// is legitimately part of the media. A detected leading silence at or beyond
// this bound is treated as "no leading silence" (return 0) so timestamps are
// never shifted by a large, likely-wrong amount.
const maxLeadingSilenceTrim = 5 * time.Second

// silenceProbeTimeout bounds the silencedetect pass independently of any caller
// deadline, so a pathological input cannot hang ingest. silencedetect must
// decode the whole stream, so this is more generous than a metadata probe.
const silenceProbeTimeout = 60 * time.Second

// DetectLeadingSilence reports the duration of silence at the very start of the
// media at path, detected via ffmpeg's `silencedetect` filter. It returns 0 when
// the media starts with speech, when ffmpeg reports no leading silence, when the
// detected leading silence is implausibly long (>= maxLeadingSilenceTrim), or
// when ffmpeg is unavailable — i.e. callers can treat any non-positive result as
// "do not trim" and never need to special-case the tool being absent.
//
// It is graceful by contract: a missing ffmpeg binary returns (0, nil), not an
// error, so leading-silence trimming silently degrades to a no-op rather than
// failing ingest. A genuine ffmpeg execution failure is returned so the caller
// can log it, but such callers still treat it as "no trim".
//
// thresholdDB and minDurationSec are the silencedetect `noise`/`d` parameters;
// non-positive values fall back to the package defaults. Detection is
// deterministic for a given input and parameters.
func DetectLeadingSilence(ctx context.Context, path string, thresholdDB, minDurationSec float64) (time.Duration, error) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		// Tool absent: graceful no-op, never fatal (mirrors Duration's
		// ErrToolNotFound contract but folded into a 0 so silence trimming is
		// purely best-effort).
		return 0, nil
	}
	if thresholdDB >= 0 {
		thresholdDB = DefaultSilenceThresholdDB
	}
	if minDurationSec <= 0 {
		minDurationSec = DefaultSilenceMinDuration
	}

	// Always cap the probe at silenceProbeTimeout: apply it when the caller has
	// no deadline, OR when the caller's deadline is further out than the probe
	// cap, so a long-lived ingest context cannot let ffmpeg run unbounded. A
	// nearer caller deadline is left in force (it is the stricter bound).
	probeCtx := ctx
	if deadline, hasDeadline := ctx.Deadline(); !hasDeadline || time.Until(deadline) > silenceProbeTimeout {
		var cancel context.CancelFunc
		probeCtx, cancel = context.WithTimeout(ctx, silenceProbeTimeout)
		defer cancel()
	}

	filter := fmt.Sprintf("silencedetect=noise=%sdB:d=%s",
		strconv.FormatFloat(thresholdDB, 'f', -1, 64),
		strconv.FormatFloat(minDurationSec, 'f', -1, 64),
	)
	cmd := exec.CommandContext(probeCtx, bin,
		"-nostdin",
		"-i", path,
		"-af", filter,
		"-f", "null",
		"-",
	)
	// silencedetect writes its report to stderr; stdout is the (discarded) null
	// muxer output.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("ffmpeg silencedetect %q: %w: %s", path, err, strings.TrimSpace(stderr.String()))
	}
	return parseLeadingSilence(stderr.String()), nil
}

// parseLeadingSilence extracts the leading-silence duration from ffmpeg
// silencedetect stderr output. silencedetect emits lines such as:
//
//	[silencedetect @ 0x..] silence_start: 0
//	[silencedetect @ 0x..] silence_end: 1.234 | silence_duration: 1.234
//
// Leading silence is present only when a silence period starts at (or
// effectively at) the very beginning of the stream; the trim amount is that
// period's `silence_end`. We scan for the earliest silence_start and, when it is
// at the start, take its matching silence_end. A silence period that does not
// begin at the start is ignored (it is a mid-stream gap, not leading silence).
// Returns 0 when no qualifying leading silence is found or it is implausibly
// long (>= maxLeadingSilenceTrim).
func parseLeadingSilence(stderr string) time.Duration {
	const (
		startTag = "silence_start:"
		endTag   = "silence_end:"
	)
	// startEpsilonSec tolerates ffmpeg reporting the first silence_start as a
	// tiny positive offset (e.g. 0.0001) rather than exactly 0.
	const startEpsilonSec = 0.05

	leadingActive := false
	for _, line := range strings.Split(stderr, "\n") {
		if idx := strings.Index(line, startTag); idx >= 0 {
			val, ok := parseFloatField(line[idx+len(startTag):])
			if !ok {
				continue
			}
			// Only the first silence period matters for leading silence, and
			// only if it starts at the very beginning.
			if val <= startEpsilonSec {
				leadingActive = true
			} else if !leadingActive {
				// First silence period starts mid-stream -> no leading silence.
				return 0
			}
			continue
		}
		if !leadingActive {
			continue
		}
		if idx := strings.Index(line, endTag); idx >= 0 {
			val, ok := parseFloatField(line[idx+len(endTag):])
			if !ok || val <= 0 {
				return 0
			}
			d := time.Duration(val * float64(time.Second))
			if d >= maxLeadingSilenceTrim {
				return 0
			}
			return d
		}
	}
	return 0
}

// ParseLeadingSilence is the exported counterpart of parseLeadingSilence,
// exposed for tests in the tests/ tree so leading-silence parsing can be
// verified against captured ffmpeg silencedetect output without requiring the
// ffmpeg binary.
func ParseLeadingSilence(silencedetectStderr string) time.Duration {
	return parseLeadingSilence(silencedetectStderr)
}

// parseFloatField reads the first whitespace-delimited token from s as a float.
// It tolerates a trailing "| silence_duration: ..." continuation by reading only
// the first field.
func parseFloatField(s string) (float64, bool) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// ExtractSegment cuts the window [startMS, endMS) from the media at a local
// filesystem path and returns the resulting container bytes. It uses stream copy
// (no re-encode) so the MIME type is unchanged, extraction is fast, and the
// output is deterministic for a given input.
//
// Accuracy: audio is cut sample-accurately, but for video stream copy can only
// start at the nearest preceding keyframe, so the clip is keyframe-aligned and
// may begin slightly before startMS (approximate, not frame-exact). This is
// acceptable for embedding — a window is a coarse retrieval unit, not a
// frame-precise clip — and avoids the cost/quality loss of re-encoding. Callers
// that need frame-exact cuts must re-encode. Window lengths are chosen below the
// per-modality caps (SPEC 8.1.7) so keyframe drift cannot push a clip over the
// cap.
//
// It returns ErrToolNotFound when ffmpeg is not installed.
func ExtractSegment(ctx context.Context, path string, startMS, endMS int) ([]byte, error) {
	// A local path: infer the container extension from the path so the extracted
	// clip keeps the source muxer/MIME.
	return extractSegment(ctx, path, filepath.Ext(path), startMS, endMS)
}

// ExtractSegmentURL cuts the window [startMS, endMS) from media at an http(s)
// URL (e.g. an S3 presigned GetObject URL) and returns the container bytes. It
// is the range-read counterpart of ExtractSegment: ffmpeg opens the URL over its
// http protocol and, when the server advertises byte-range support (S3 does),
// seeks to the requested window so only the bytes around [startMS, endMS) plus
// the container's index are fetched — the whole object is NOT downloaded. This
// is the audio/video analogue of CorpusFS.Open's range GETs, which is why the
// worker prefers it over Localize (a whole-object download) for S3-backed media.
//
// srcExt supplies the container extension (e.g. ".mp4") so ffmpeg writes the clip
// with the matching muxer, since a presigned URL's path/query is not a reliable
// muxer hint. An empty srcExt falls back to ".bin". Behavior, accuracy, and the
// ErrToolNotFound contract otherwise match ExtractSegment.
func ExtractSegmentURL(ctx context.Context, url, srcExt string, startMS, endMS int) ([]byte, error) {
	if !isHTTPURL(url) {
		// Never echo the raw input here: although a non-http input has reached
		// this guard, the same redaction discipline as the rest of this file
		// applies so a leaked credential-bearing URL (e.g. with a wrong scheme)
		// cannot surface in an error or log line.
		return nil, fmt.Errorf("avutil: ExtractSegmentURL requires an http(s) URL, got %q", redactInput(url))
	}
	return extractSegment(ctx, url, srcExt, startMS, endMS)
}

// ExtractSegmentPreview cuts [startMS, endMS) from a local media file and
// RE-ENCODES it to fit under maxBytes, for dir2mcp_open_media_clip's max_bytes
// contract (SPEC §15.11, spec 0.54.0; dir2mcp #878). It is the reduced-fidelity
// counterpart of ExtractSegment: where that stream-copies at source bitrate,
// this one budgets a bitrate from the byte ceiling and the span duration.
//
// The returned string names the rendition for a human (codec, resolution,
// bitrate) and becomes the response's `preview` field verbatim, whose PRESENCE
// tells the caller the bytes are not a source-fidelity cut.
//
// The budget spends 92% of maxBytes, leaving headroom for container overhead
// and encoder rate-control drift; single-pass rate control is approximate, so
// the CALLER must still verify the result fits and refuse when it does not.
// Bitrates are floored (video 80 kbps, audio 24 kbps) because below those the
// output is noise that happens to fit; when the floor does not fit the span,
// the result is over maxBytes and the caller's verification refuses it, which
// keeps "cannot fit" a caller-visible CLIP_TOO_LARGE rather than a garbage
// clip. Video previews are additionally capped at 480p (h264 + aac in mp4,
// +faststart); audio previews are aac in m4a. It returns ErrToolNotFound when
// ffmpeg is not installed.
func ExtractSegmentPreview(ctx context.Context, path string, startMS, endMS, maxBytes int, video bool) ([]byte, string, error) {
	if startMS < 0 || endMS <= startMS {
		return nil, "", fmt.Errorf("avutil: invalid segment [%d,%d) for %q", startMS, endMS, redactInput(path))
	}
	if maxBytes <= 0 {
		return nil, "", fmt.Errorf("avutil: preview needs a positive byte budget, got %d", maxBytes)
	}
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, "", ErrToolNotFound
	}

	durS := float64(endMS-startMS) / 1000.0
	budgetBits := float64(maxBytes) * 8 * 0.92

	tmpDir, err := os.MkdirTemp("", "dir2mcp-avprev-")
	if err != nil {
		return nil, "", fmt.Errorf("avutil: temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	var args []string
	var outPath, rendition string
	if video {
		audioBps := 64_000
		videoBps := int(budgetBits/durS) - audioBps
		if videoBps < 80_000 {
			// Try to save the video by shrinking the audio before giving up on
			// the floor; a tiny budget still produces a floored (over-budget)
			// encode, which the caller's size check then refuses.
			audioBps = 32_000
			videoBps = int(budgetBits/durS) - audioBps
			if videoBps < 80_000 {
				videoBps = 80_000
			}
		}
		outPath = filepath.Join(tmpDir, "preview.mp4")
		args = []string{
			"-nostdin", "-v", "error",
			"-ss", msToSeconds(startMS), "-to", msToSeconds(endMS),
			"-i", path,
			"-vf", "scale=-2:'min(480,ih)'",
			"-c:v", "libx264", "-preset", "veryfast",
			"-b:v", strconv.Itoa(videoBps),
			"-maxrate", strconv.Itoa(videoBps),
			"-bufsize", strconv.Itoa(2 * videoBps),
			"-c:a", "aac", "-b:a", strconv.Itoa(audioBps),
			"-movflags", "+faststart",
			"-y", outPath,
		}
		rendition = fmt.Sprintf("h264 <=480p ~%dkbps + aac %dkbps, re-encoded to fit max_bytes", videoBps/1000, audioBps/1000)
	} else {
		audioBps := int(budgetBits / durS)
		if audioBps < 24_000 {
			audioBps = 24_000
		}
		if audioBps > 192_000 {
			audioBps = 192_000
		}
		outPath = filepath.Join(tmpDir, "preview.m4a")
		args = []string{
			"-nostdin", "-v", "error",
			"-ss", msToSeconds(startMS), "-to", msToSeconds(endMS),
			"-i", path,
			"-vn",
			"-c:a", "aac", "-b:a", strconv.Itoa(audioBps),
			"-movflags", "+faststart",
			"-y", outPath,
		}
		rendition = fmt.Sprintf("aac ~%dkbps, re-encoded to fit max_bytes", audioBps/1000)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, "", fmt.Errorf("ffmpeg preview %q [%d,%d): %w: %s", redactInput(path), startMS, endMS, err, redactStderr(stderr.String(), path))
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		return nil, "", fmt.Errorf("avutil: read preview: %w", err)
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("avutil: ffmpeg produced an empty preview for %q [%d,%d)", redactInput(path), startMS, endMS)
	}
	return data, rendition, nil
}

// ExtractAudioTrack demuxes the audio stream of the media at a local filesystem
// path and re-encodes it into a compact mono 16 kHz AAC (.m4a) clip suitable for
// speech-to-text, returning the container bytes. The video stream is dropped
// (-vn), so a video container (.mp4/.mov) yields an audio-only clip that STT
// providers accept — the audio track of a video is transcribed exactly like a
// standalone audio file (issue #495).
//
// Re-encoding (rather than a stream copy) makes the output codec/container
// deterministic regardless of the source audio codec (aac, opus, ac3, …), and
// mono 16 kHz mirrors the preprocessing speech models apply internally, keeping
// the clip small enough for provider upload limits without hurting recognition.
//
// It returns ErrToolNotFound when ffprobe/ffmpeg are not installed and
// ErrNoAudioStream when the media carries no audio track (nothing to transcribe).
func ExtractAudioTrack(ctx context.Context, path string) ([]byte, error) {
	return extractAudioTrack(ctx, path, -1)
}

// ExtractAudioTrackIndex demuxes a SPECIFIC audio stream — selected by its
// 0-based AUDIO-relative index (0 == the first audio stream, the one
// ExtractAudioTrack picks by default) — into the same compact mono 16 kHz AAC
// (.m4a) clip ExtractAudioTrack produces, so each audio track of a multi-track
// container (an original mix plus per-language dubs, a music-&-effects track)
// can be transcribed independently (SPEC §8.6.12, issue #567).
//
// The index maps to ffmpeg's `-map 0:a:<audioIndex>` stream selector, which is
// audio-relative (matching MediaInfo.AudioStreams ordering), NOT the absolute
// container stream index. An audioIndex past the container's audio-stream count
// is reported as ErrNoAudioStream (the track does not exist) rather than an
// opaque ffmpeg failure, so a caller can degrade that track gracefully. It
// returns ErrToolNotFound when ffprobe/ffmpeg are not installed.
func ExtractAudioTrackIndex(ctx context.Context, path string, audioIndex int) ([]byte, error) {
	if audioIndex < 0 {
		return nil, fmt.Errorf("avutil: negative audio track index %d for %q", audioIndex, path)
	}
	return extractAudioTrack(ctx, path, audioIndex)
}

// extractAudioTrack is the shared implementation behind ExtractAudioTrack (the
// default/first audio stream, audioIndex < 0 ⇒ ffmpeg's `-vn` default mapping)
// and ExtractAudioTrackIndex (a specific audio-relative stream, audioIndex >= 0
// ⇒ `-map 0:a:<audioIndex>`). Both emit an identical mono 16 kHz AAC clip; the
// only difference is stream selection.
func extractAudioTrack(ctx context.Context, path string, audioIndex int) ([]byte, error) {
	// Probe first so a video with no audio track — or a requested track index past
	// the audio-stream count — is reported as the distinct ErrNoAudioStream (a
	// graceful "no transcript") instead of an opaque ffmpeg "does not contain any
	// stream" failure. A probe error other than a missing tool is not fatal here:
	// fall through and let ffmpeg surface the real error.
	if info, err := ProbeMediaInfo(ctx, path); err != nil {
		if errors.Is(err, ErrToolNotFound) {
			return nil, ErrToolNotFound
		}
	} else if audioIndex >= 0 {
		if audioIndex >= len(info.AudioStreams) {
			return nil, ErrNoAudioStream
		}
	} else if strings.TrimSpace(info.AudioCodec) == "" {
		return nil, ErrNoAudioStream
	}

	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, ErrToolNotFound
	}

	tmpDir, err := os.MkdirTemp("", "dir2mcp-avaudio-")
	if err != nil {
		return nil, fmt.Errorf("avutil: temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	outPath := filepath.Join(tmpDir, "audio.m4a")

	args := []string{
		"-nostdin",
		"-v", "error",
		"-i", path,
	}
	if audioIndex >= 0 {
		// Select exactly the requested audio-relative stream (0:a:<N>) and drop
		// video; without an explicit map ffmpeg would pick its default audio stream.
		args = append(args, "-map", fmt.Sprintf("0:a:%d", audioIndex))
	} else {
		// Default selection: drop video, let ffmpeg choose the default audio stream
		// (byte-for-byte the historical ExtractAudioTrack behavior).
		args = append(args, "-vn")
	}
	args = append(args,
		"-ac", "1",
		"-ar", "16000",
		"-c:a", "aac",
		"-y", outPath,
	)

	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg extract audio %q: %w: %s", path, err, strings.TrimSpace(stderr.String()))
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("avutil: read extracted audio: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("avutil: ffmpeg produced an empty audio track for %q", path)
	}
	return data, nil
}

// isHTTPURL reports whether s is an http or https URL, the only schemes ffmpeg
// can range-seek for ExtractSegmentURL.
func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// redactInput returns a log/error-safe rendering of an extractSegment input.
// Local filesystem paths are returned unchanged, but for an http(s) URL the
// entire query string is stripped before the value is ever placed in an error
// message or log line. An S3 (or any) presigned URL carries its credentials and
// signature in the query (X-Amz-Signature, X-Amz-Credential, …); dropping the
// query preserves enough for diagnostics (scheme + host + path) while ensuring
// the secret never leaks into errors that are logged or persisted as a failure
// reason (CLAUDE.md: never log secrets/raw sensitive payloads). If the URL fails
// to parse it is replaced wholesale with a placeholder rather than risking a
// partial leak.
func redactInput(input string) string {
	if !isHTTPURL(input) {
		return input
	}
	u, err := neturl.Parse(input)
	if err != nil {
		return "[redacted-url]"
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.User = nil
	return u.String()
}

// redactStderr makes ffmpeg's stderr safe to embed in a wrapped error. Some
// ffmpeg builds echo the input URL in protocol errors (e.g. "Server returned
// 403 Forbidden" with the full URL), which would reintroduce the signed query.
// It replaces both the full input URL and its raw query string with the redacted
// form so neither a whole-URL nor a query-only echo can leak the signature.
func redactStderr(stderr, input string) string {
	out := strings.TrimSpace(stderr)
	if !isHTTPURL(input) {
		return out
	}
	out = strings.ReplaceAll(out, input, redactInput(input))
	if u, err := neturl.Parse(input); err == nil && u.RawQuery != "" {
		out = strings.ReplaceAll(out, u.RawQuery, "[redacted]")
	}
	return out
}

// extractSegment is the shared ffmpeg stream-copy implementation behind both the
// local-path (ExtractSegment) and http-URL (ExtractSegmentURL) entry points. The
// input string is handed to ffmpeg's -i, which accepts both a filesystem path and
// an http(s) URL; the only difference is how the container extension is derived
// for the output muxer, so callers pass it explicitly via srcExt.
func extractSegment(ctx context.Context, input, srcExt string, startMS, endMS int) ([]byte, error) {
	// safeInput is the only rendering of input that may appear in an error or log
	// line: for an http(s) URL it has the credential-bearing query stripped (see
	// redactInput) so a presigned URL's signature never leaks.
	safeInput := redactInput(input)
	if startMS < 0 || endMS <= startMS {
		return nil, fmt.Errorf("avutil: invalid segment [%d,%d) for %q", startMS, endMS, safeInput)
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
	ext := srcExt
	if ext == "" {
		ext = ".bin"
	}
	outPath := filepath.Join(tmpDir, "segment"+ext)

	cmd := exec.CommandContext(ctx, bin,
		"-nostdin",
		"-v", "error",
		"-ss", msToSeconds(startMS),
		"-to", msToSeconds(endMS),
		"-i", input,
		"-c", "copy",
		"-y", outPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg segment %q [%d,%d): %w: %s", safeInput, startMS, endMS, err, redactStderr(stderr.String(), input))
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("avutil: read extracted segment: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("avutil: ffmpeg produced an empty segment for %q [%d,%d)", safeInput, startMS, endMS)
	}
	return data, nil
}

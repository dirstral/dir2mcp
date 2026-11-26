package shots

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
)

// ErrToolNotFound means the ffmpeg detector has no ffmpeg on PATH (SPEC
// §8.10.1): one warning at startup and no detection for the run, never a
// per-document failure.
var ErrToolNotFound = errors.New("shots: ffmpeg not found on PATH")

// MaxResponseBytes bounds the /shots response that the daemon reads (SPEC
// §8.10.3). It is far above the cuts of a long broadcast: 300 cuts an hour for
// 24 hours is a few kilobytes.
const MaxResponseBytes = 16 << 20

// Result is one detection: the identity of the detector that produced it and
// its candidate cuts in milliseconds. CutsToShots turns the cuts into shots.
type Result struct {
	Identity Identity
	CutsMS   []int
}

// Detector finds the candidate cuts of the video at absPath.
type Detector interface {
	Detect(ctx context.Context, absPath string) (Result, error)
	// LocalIdentity is the part of the shot identity that the configuration
	// alone decides (SPEC §8.10.6), for the re-detection check.
	LocalIdentity() Identity
}

// FFmpeg is the built-in detector (SPEC §8.10.2): the scene score of the
// ffmpeg select filter on a 360-pixel decode.
type FFmpeg struct {
	bin       string
	version   string
	threshold float64
	minShotS  float64
}

// NewFFmpeg finds ffmpeg and reads its version once. It returns
// ErrToolNotFound when ffmpeg is not on PATH.
func NewFFmpeg(ctx context.Context, threshold, minShotS float64) (*FFmpeg, error) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, ErrToolNotFound
	}
	out, err := exec.CommandContext(ctx, bin, "-hide_banner", "-version").Output()
	if err != nil {
		return nil, fmt.Errorf("shots: read ffmpeg version: %w", err)
	}
	return &FFmpeg{bin: bin, version: ParseFFmpegVersion(string(out)), threshold: threshold, minShotS: minShotS}, nil
}

// ParseFFmpegVersion returns the version token of `ffmpeg -version` output
// ("ffmpeg version 6.1.1-3ubuntu5 Copyright ..." gives "6.1.1-3ubuntu5"), or
// the whole first line when the format is unexpected, so a change still
// changes the identity.
func ParseFFmpegVersion(out string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	fields := strings.Fields(first)
	if len(fields) >= 3 && fields[0] == "ffmpeg" && fields[1] == "version" {
		return fields[2]
	}
	return strings.TrimSpace(first)
}

// LocalIdentity returns the full ffmpeg identity: every field is local.
func (f *FFmpeg) LocalIdentity() Identity {
	return Identity{
		Detector: DetectorFFmpeg, Version: f.version,
		Threshold: f.threshold, MinShotS: f.minShotS, Height: FFmpegHeight,
	}
}

// FilterGraph is the -vf argument of the detection decode. The select filter
// passes only the frames at or above the threshold, so metadata=print writes
// one short record for each candidate cut, not one for every frame.
func FilterGraph(threshold float64) string {
	return fmt.Sprintf("scale=-2:%d,select='gte(scene,%s)',metadata=print:file=-",
		FFmpegHeight, strconv.FormatFloat(threshold, 'f', -1, 64))
}

// Detect decodes the video once and returns the candidate cuts.
func (f *FFmpeg) Detect(ctx context.Context, absPath string) (Result, error) {
	cmd := exec.CommandContext(ctx, f.bin,
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-i", absPath, "-an", "-sn", "-dn",
		"-vf", FilterGraph(f.threshold),
		"-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("shots: ffmpeg pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("shots: start ffmpeg: %w", err)
	}
	cuts, parseErr := ParseSceneMetadata(stdout)
	waitErr := cmd.Wait()
	if waitErr != nil {
		return Result{}, fmt.Errorf("shots: ffmpeg: %w: %s", waitErr, lastLine(stderr.String()))
	}
	if parseErr != nil {
		return Result{}, fmt.Errorf("shots: read ffmpeg output: %w", parseErr)
	}
	return Result{Identity: f.LocalIdentity(), CutsMS: SecondsToMS(cuts)}, nil
}

// ParseSceneMetadata reads the output of metadata=print after the select
// filter: each "frame:N pts:P pts_time:T" record is one selected frame, so
// each T is a candidate cut in seconds.
func ParseSceneMetadata(r io.Reader) ([]float64, error) {
	var cuts []float64
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "frame:") {
			continue
		}
		for _, field := range strings.Fields(line) {
			v, ok := strings.CutPrefix(field, "pts_time:")
			if !ok {
				continue
			}
			if t, err := strconv.ParseFloat(v, 64); err == nil {
				cuts = append(cuts, t)
			}
		}
	}
	return cuts, sc.Err()
}

// lastLine keeps an ffmpeg error short: its last non-empty line, at most 300
// bytes.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if len(last) > 300 {
		last = last[:300]
	}
	return last
}

// Serve is the served detector (SPEC §8.10.3): POST {base_url}/shots.
type Serve struct {
	endpoint string
	minShotS float64
	client   *http.Client
}

// NewServe builds a served detector. Like the recognition client it carries no
// client-level timeout: the per-document bound arrives on the context (SPEC
// §8.10.3, the same rules as §8.9.5).
func NewServe(baseURL string, minShotS float64) *Serve {
	return &Serve{endpoint: ShotsEndpoint(baseURL), minShotS: minShotS, client: &http.Client{}}
}

// ShotsEndpoint appends /shots to the PATH of base_url and keeps its query, so
// http://host:8766?token=abc becomes http://host:8766/shots?token=abc. A
// string join would put "/shots" inside the query. A base_url that does not
// parse (config validation rejects it) falls back to the plain join.
func ShotsEndpoint(baseURL string) string {
	raw := strings.TrimSpace(baseURL)
	u, err := url.Parse(raw)
	if err != nil {
		return strings.TrimRight(raw, "/") + "/shots"
	}
	return u.JoinPath("shots").String()
}

// LocalIdentity returns the fields of a served identity that the
// configuration decides; name and version come from the response.
func (s *Serve) LocalIdentity() Identity {
	return Identity{Detector: DetectorServe, MinShotS: s.minShotS}
}

// serveResponse is the wire shape (spec/shots/response.schema.json). The
// pointers tell an absent or null field from an empty one: `cuts: []` is a
// valid answer (one shot), a missing `cuts` is not.
type serveResponse struct {
	Detector *struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"detector"`
	Cuts *[]float64 `json:"cuts"`
}

// Detect asks the backend for the cuts of the video at absPath.
func (s *Serve) Detect(ctx context.Context, absPath string) (Result, error) {
	payload, err := json.Marshal(map[string]string{"path": absPath})
	if err != nil {
		return Result{}, fmt.Errorf("shots: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(payload))
	if err != nil {
		return Result{}, fmt.Errorf("shots: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		// A *url.Error repeats the request URL, and a base_url can carry a
		// token in its query. Report the operation and the cause only.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return Result{}, fmt.Errorf("shots: backend request: %s: %w", uerr.Op, uerr.Err)
		}
		return Result{}, fmt.Errorf("shots: backend request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// No body echo: a backend error can hold local paths (SPEC §8.9.3).
		return Result{}, fmt.Errorf("shots: backend returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("shots: read response: %w", err)
	}
	if len(body) > MaxResponseBytes {
		return Result{}, fmt.Errorf("shots: response larger than %d bytes", MaxResponseBytes)
	}
	var wire serveResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return Result{}, fmt.Errorf("shots: decode response: %w", err)
	}
	// A 200 that is null, or that lacks the detector identity or the cuts, is
	// a malformed answer, not "no cuts": storing it as one shot would record a
	// current identity, and no later scan would retry it.
	if wire.Detector == nil || strings.TrimSpace(wire.Detector.Name) == "" ||
		strings.TrimSpace(wire.Detector.Version) == "" {
		return Result{}, errors.New("shots: response has no detector name and version")
	}
	if wire.Cuts == nil {
		return Result{}, errors.New("shots: response has no cuts array")
	}
	id := s.LocalIdentity()
	id.Name = strings.TrimSpace(wire.Detector.Name)
	id.Version = strings.TrimSpace(wire.Detector.Version)
	return Result{Identity: id, CutsMS: SecondsToMS(*wire.Cuts)}, nil
}

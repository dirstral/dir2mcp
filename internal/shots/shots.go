// Package shots detects the shots of a video and derives its keyframes (SPEC
// §8.10, spec 0.103.0). A shot is the time range of one camera take. Shots are
// structure, not text: this package produces time ranges and identities, never
// chunks.
//
// The package holds the parts that do not touch the index: the cut-to-shot
// rule (§8.10.4), the keyframe rule (§8.10.5), the shot identity (§8.10.6) and
// the two detectors (§8.10.2 ffmpeg, §8.10.3 serve). The ingest service
// decides when to detect and persists the result.
package shots

import (
	"encoding/json"
	"math"
	"sort"
)

// Shot is one shot of a video: [StartMS, EndMS), numbered from 0 in time order.
type Shot struct {
	Index   int
	StartMS int
	EndMS   int
}

// Detector kinds (media.shots.detector).
const (
	DetectorOff    = "off"
	DetectorFFmpeg = "ffmpeg"
	DetectorServe  = "serve"
)

// FFmpegHeight is the decode height of the ffmpeg detector (§8.10.2). It is
// part of the shot identity, so a change re-detects every video.
const FFmpegHeight = 360

// Identity is the shot identity (SPEC §8.10.6). For ffmpeg every field is
// known from the configuration and the tool; for serve, Name and Version come
// from the response.
type Identity struct {
	Detector  string  `json:"detector"`
	Name      string  `json:"name,omitempty"`
	Version   string  `json:"version,omitempty"`
	Threshold float64 `json:"threshold,omitempty"`
	MinShotS  float64 `json:"min_shot_s"`
	Height    int     `json:"height,omitempty"`
}

// JSON renders the identity in its stored form. Field order is fixed by the
// struct, so equal identities give equal strings.
func (id Identity) JSON() string {
	b, err := json.Marshal(id)
	if err != nil {
		// A struct of strings and numbers cannot fail to marshal; keep the
		// signature simple for callers.
		return ""
	}
	return string(b)
}

// ParseIdentity reads a stored identity. A malformed value reads as the zero
// identity, which matches no configuration and so causes a new detection.
func ParseIdentity(raw string) Identity {
	var id Identity
	if err := json.Unmarshal([]byte(raw), &id); err != nil {
		return Identity{}
	}
	return id
}

// LocalFieldsMatch reports whether the stored identity agrees with the fields
// that the configuration alone decides (SPEC §8.10.6): the detector kind and
// min_shot_s in both modes, and for ffmpeg also the tool version, the
// threshold and the decode height. The name and version of a served detector
// are not compared: they are known only from a response.
func (id Identity) LocalFieldsMatch(want Identity) bool {
	if id.Detector != want.Detector || !sameFloat(id.MinShotS, want.MinShotS) {
		return false
	}
	if want.Detector == DetectorFFmpeg {
		return id.Version == want.Version && sameFloat(id.Threshold, want.Threshold) && id.Height == want.Height
	}
	return true
}

func sameFloat(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// SecondsToMS converts cut times in seconds to milliseconds, rounding to the
// nearest millisecond (SPEC §8.10.4). A value that is not finite is dropped
// here; the range checks belong to CutsToShots.
func SecondsToMS(cuts []float64) []int {
	out := make([]int, 0, len(cuts))
	for _, c := range cuts {
		if math.IsNaN(c) || math.IsInf(c, 0) {
			continue
		}
		// Clamp before the conversion so a huge value cannot overflow int;
		// it is then dropped as not less than the duration.
		c = math.Max(-math.MaxInt32, math.Min(c, math.MaxInt32))
		out = append(out, int(math.Round(c*1000)))
	}
	return out
}

// CutsToShots applies SPEC §8.10.4 to cut times in milliseconds:
//
//  1. keep only cuts with 0 < cut < duration;
//  2. sort them and remove repeats;
//  3. keep a cut only when it is at least minShotMS after the previous kept
//     cut (the first is compared with 0) and at least minShotMS before the
//     end;
//  4. cover [0, duration) with shots that start at 0 and at each kept cut.
//
// A non-positive duration gives no shots: the duration is required.
func CutsToShots(cutsMS []int, durationMS, minShotMS int) []Shot {
	if durationMS <= 0 {
		return nil
	}
	minShotMS = max(minShotMS, 0)
	inside := make([]int, 0, len(cutsMS))
	for _, c := range cutsMS {
		if c > 0 && c < durationMS {
			inside = append(inside, c)
		}
	}
	sort.Ints(inside)
	kept := make([]int, 0, len(inside))
	prev := 0
	for i, c := range inside {
		if i > 0 && c == inside[i-1] {
			continue
		}
		if c-prev < minShotMS || durationMS-c < minShotMS {
			continue
		}
		kept = append(kept, c)
		prev = c
	}
	out := make([]Shot, 0, len(kept)+1)
	start := 0
	for _, c := range kept {
		out = append(out, Shot{Index: len(out), StartMS: start, EndMS: c})
		start = c
	}
	return append(out, Shot{Index: len(out), StartMS: start, EndMS: durationMS})
}

// Keyframes returns the keyframe times of a shot (SPEC §8.10.5): k =
// ceil(L / gap) equal parts, at least one, each represented by its middle,
// clamped to the last millisecond of the shot.
func Keyframes(s Shot, gapMS int) []int {
	length := s.EndMS - s.StartMS
	if length <= 0 {
		return nil
	}
	k := 1
	if gapMS > 0 {
		k = max((length+gapMS-1)/gapMS, 1)
	}
	out := make([]int, 0, k)
	for i := range k {
		t := s.StartMS + int(math.Round((float64(i)+0.5)*float64(length)/float64(k)))
		out = append(out, min(t, s.EndMS-1))
	}
	return out
}

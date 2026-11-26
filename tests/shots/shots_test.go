package tests

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/shots"
)

// SPEC §8.10 (spec 0.103.0): the cut-to-shot rule, the keyframe rule, the shot
// identity and the two detectors.

func TestCutsToShots_CoversTheVideoWithNoGap(t *testing.T) {
	got := shots.CutsToShots([]int{6000, 2000, 2000, 9000}, 10000, 500)
	want := []shots.Shot{{Index: 0, StartMS: 0, EndMS: 2000}, {Index: 1, StartMS: 2000, EndMS: 6000}, {Index: 2, StartMS: 6000, EndMS: 9000}, {Index: 3, StartMS: 9000, EndMS: 10000}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shots = %v, want %v", got, want)
	}
}

func TestCutsToShots_MinimumShotLength(t *testing.T) {
	// 300 is too near 0; 2100 is too near the kept 2000; 9800 is too near the
	// end. The walk compares with the previous KEPT cut, so 2600 (600 after
	// 2000) is kept even though 2100 came between them.
	got := shots.CutsToShots([]int{300, 2000, 2100, 2600, 9800}, 10000, 500)
	want := []shots.Shot{{Index: 0, StartMS: 0, EndMS: 2000}, {Index: 1, StartMS: 2000, EndMS: 2600}, {Index: 2, StartMS: 2600, EndMS: 10000}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shots = %v, want %v", got, want)
	}
}

func TestCutsToShots_BoundaryCutsAfterRoundingAreDropped(t *testing.T) {
	// SPEC §8.10.4 step 1: with min_shot_s 0, cuts that round to 0 or to the
	// duration would make zero-length shots.
	cuts := shots.SecondsToMS([]float64{0.0004, 0.9996, 0.5})
	got := shots.CutsToShots(cuts, 1000, 0)
	want := []shots.Shot{{Index: 0, StartMS: 0, EndMS: 500}, {Index: 1, StartMS: 500, EndMS: 1000}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shots = %v, want %v", got, want)
	}
	for _, s := range got {
		if s.EndMS <= s.StartMS {
			t.Fatalf("empty shot %v", s)
		}
	}
}

func TestCutsToShots_NoCutIsOneShotAndNoDurationIsNone(t *testing.T) {
	if got := shots.CutsToShots(nil, 5000, 500); !reflect.DeepEqual(got, []shots.Shot{{Index: 0, StartMS: 0, EndMS: 5000}}) {
		t.Fatalf("no cut: %v, want one shot over the whole video", got)
	}
	if got := shots.CutsToShots([]int{1000}, 0, 500); got != nil {
		t.Fatalf("no duration: %v, want no shots (the duration is required)", got)
	}
}

func TestSecondsToMS_DropsNonFiniteAndRounds(t *testing.T) {
	got := shots.SecondsToMS([]float64{1.0004, 1.0006, math.NaN(), math.Inf(1), 2.5})
	want := []int{1000, 1001, 2500}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ms = %v, want %v", got, want)
	}
}

func TestKeyframes(t *testing.T) {
	for _, tc := range []struct {
		name string
		shot shots.Shot
		gap  int
		want []int
	}{
		{"short shot: its middle", shots.Shot{StartMS: 1000, EndMS: 3000}, 60000, []int{2000}},
		{"long shot: one per gap", shots.Shot{StartMS: 0, EndMS: 150000}, 60000, []int{25000, 75000, 125000}},
		{"1 ms shot: clamped inside", shots.Shot{StartMS: 7, EndMS: 8}, 1000, []int{7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := shots.Keyframes(tc.shot, tc.gap)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("keyframes = %v, want %v", got, tc.want)
			}
			for _, k := range got {
				if k < tc.shot.StartMS || k >= tc.shot.EndMS {
					t.Fatalf("keyframe %d outside [%d, %d)", k, tc.shot.StartMS, tc.shot.EndMS)
				}
			}
		})
	}
}

func TestIdentity_LocalFieldsMatch(t *testing.T) {
	ff := shots.Identity{Detector: "ffmpeg", Version: "6.1.1", Threshold: 0.2, MinShotS: 0.5, Height: 360}
	if !shots.ParseIdentity(ff.JSON()).LocalFieldsMatch(ff) {
		t.Fatal("a stored ffmpeg identity does not match itself")
	}
	for name, other := range map[string]shots.Identity{
		"threshold": {Detector: "ffmpeg", Version: "6.1.1", Threshold: 0.3, MinShotS: 0.5, Height: 360},
		"version":   {Detector: "ffmpeg", Version: "7.0", Threshold: 0.2, MinShotS: 0.5, Height: 360},
		"min shot":  {Detector: "ffmpeg", Version: "6.1.1", Threshold: 0.2, MinShotS: 1, Height: 360},
		"detector":  {Detector: "serve", MinShotS: 0.5},
	} {
		if ff.LocalFieldsMatch(other) {
			t.Fatalf("a changed %s still matches", name)
		}
	}
	// A served identity: name and version are known only from a response, so
	// they never decide a re-detection; min_shot_s does.
	stored := shots.Identity{Detector: "serve", Name: "transnetv2", Version: "1", MinShotS: 0.5}
	if !stored.LocalFieldsMatch(shots.Identity{Detector: "serve", MinShotS: 0.5}) {
		t.Fatal("a served identity must not compare name and version")
	}
	if stored.LocalFieldsMatch(shots.Identity{Detector: "serve", MinShotS: 2}) {
		t.Fatal("a changed min_shot_s must re-detect served shots too")
	}
	if shots.ParseIdentity("not json").LocalFieldsMatch(ff) {
		t.Fatal("a malformed stored identity must cause a new detection")
	}
}

func TestParseFFmpegVersion(t *testing.T) {
	if got := shots.ParseFFmpegVersion("ffmpeg version 6.1.1-3ubuntu5 Copyright (c) 2000-2023\nbuilt with gcc"); got != "6.1.1-3ubuntu5" {
		t.Fatalf("version = %q", got)
	}
	if got := shots.ParseFFmpegVersion("something else\n"); got != "something else" {
		t.Fatalf("fallback = %q, want the first line", got)
	}
}

func TestParseSceneMetadata(t *testing.T) {
	out := "frame:0    pts:51200   pts_time:2.04\nlavfi.scene_score=0.912000\nframe:1    pts:153600  pts_time:6.12\nlavfi.scene_score=0.400000\n"
	got, err := shots.ParseSceneMetadata(strings.NewReader(out))
	if err != nil || !reflect.DeepEqual(got, []float64{2.04, 6.12}) {
		t.Fatalf("cuts = %v err=%v", got, err)
	}
}

// TestFFmpegDetector_FindsAHardCut runs the real ffmpeg on a generated video
// with one hard cut at 2 s (red to blue). It proves the filter graph works on
// a real decode, not only that the parser reads a fixed text.
func TestFFmpegDetector_FindsAHardCut(t *testing.T) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	path := filepath.Join(t.TempDir(), "cut.mp4")
	gen := exec.Command(bin, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=red:s=320x240:d=2:r=25",
		"-f", "lavfi", "-i", "color=c=blue:s=320x240:d=2:r=25",
		"-filter_complex", "[0:v][1:v]concat=n=2:v=1:a=0[v]", "-map", "[v]",
		"-pix_fmt", "yuv420p", path)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generate test video: %v: %s", err, out)
	}
	d, err := shots.NewFFmpeg(context.Background(), 0.2, 0.5)
	if err != nil {
		t.Fatalf("NewFFmpeg: %v", err)
	}
	res, err := d.Detect(context.Background(), path)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	got := shots.CutsToShots(res.CutsMS, 4000, 500)
	if len(got) != 2 || abs(got[1].StartMS-2000) > 40 {
		t.Fatalf("shots = %v, want two shots split at about 2000 ms", got)
	}
	if res.Identity.Detector != "ffmpeg" || res.Identity.Version == "" || res.Identity.Height != 360 {
		t.Fatalf("identity = %+v", res.Identity)
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func TestServeDetector(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/shots" || r.Method != http.MethodPost {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"detector":{"name":"transnetv2","version":"1.0.5"},"cuts":[1.5,4.0004]}`))
	}))
	defer ok.Close()
	res, err := shots.NewServe(ok.URL+"/", 0.5).Detect(context.Background(), "/abs/a.mp4")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !reflect.DeepEqual(res.CutsMS, []int{1500, 4000}) || res.Identity.Name != "transnetv2" ||
		res.Identity.Version != "1.0.5" || res.Identity.Detector != "serve" || res.Identity.MinShotS != 0.5 {
		t.Fatalf("result = %+v", res)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "/secret/local/path exploded", http.StatusBadGateway)
	}))
	defer bad.Close()
	_, err = shots.NewServe(bad.URL, 0.5).Detect(context.Background(), "/abs/a.mp4")
	if err == nil || strings.Contains(err.Error(), "/secret/local/path") {
		t.Fatalf("a non-200 must fail without echoing the body: %v", err)
	}

	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"detector":{"name":"x","version":"1"},"cuts":[`))
		chunk := []byte(strings.Repeat("1.0,", 1<<16))
		for written := 0; written <= shots.MaxResponseBytes; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
		_, _ = w.Write([]byte("1.0]}"))
	}))
	defer huge.Close()
	_, err = shots.NewServe(huge.URL, 0.5).Detect(context.Background(), "/abs/a.mp4")
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("a response over the bound must fail (SPEC §8.10.3): %v", err)
	}
}

func TestNewFFmpeg_MissingToolIsDistinct(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := shots.NewFFmpeg(context.Background(), 0.2, 0.5); !errors.Is(err, shots.ErrToolNotFound) {
		t.Fatalf("err = %v, want ErrToolNotFound", err)
	}
}

// TestServeDetector_MalformedAnswersFail: a 200 without the detector identity
// or the cuts is not "no cuts" (it would be stored as one current shot and
// never retried); `cuts: []` stays a valid answer.
func TestServeDetector_MalformedAnswersFail(t *testing.T) {
	for name, body := range map[string]string{
		"null":        `null`,
		"empty":       `{}`,
		"no detector": `{"cuts":[1.0]}`,
		"no version":  `{"detector":{"name":"x"},"cuts":[1.0]}`,
		"no cuts":     `{"detector":{"name":"x","version":"1"}}`,
		"null cuts":   `{"detector":{"name":"x","version":"1"},"cuts":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			if _, err := shots.NewServe(srv.URL, 0.5).Detect(context.Background(), "/abs/a.mp4"); err == nil {
				t.Fatalf("%s: want an error", body)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"detector":{"name":"x","version":"1"},"cuts":[]}`))
	}))
	defer srv.Close()
	res, err := shots.NewServe(srv.URL, 0.5).Detect(context.Background(), "/abs/a.mp4")
	if err != nil || len(res.CutsMS) != 0 {
		t.Fatalf("cuts: [] must be a valid answer: %+v, %v", res, err)
	}
}

// TestServeDetector_TransportErrorHidesTheURL: a base_url can carry a token
// in its query, and the error reaches the log and the run manifest.
func TestServeDetector_TransportErrorHidesTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close() // nothing listens now: the request fails in transport
	_, err := shots.NewServe(base+"/?token=SECRET123", 0.5).Detect(context.Background(), "/abs/a.mp4")
	if err == nil {
		t.Fatal("want a transport error")
	}
	if strings.Contains(err.Error(), "SECRET123") || strings.Contains(err.Error(), base) {
		t.Fatalf("the transport error repeats the URL: %v", err)
	}
}

func TestShotsEndpoint_KeepsTheQuery(t *testing.T) {
	for in, want := range map[string]string{
		"http://127.0.0.1:8766":           "http://127.0.0.1:8766/shots",
		"http://127.0.0.1:8766/":          "http://127.0.0.1:8766/shots",
		"http://host/api/":                "http://host/api/shots",
		"http://127.0.0.1:8766?token=abc": "http://127.0.0.1:8766/shots?token=abc",
		"http://host/api/?token=abc&x=1":  "http://host/api/shots?token=abc&x=1",
	} {
		if got := shots.ShotsEndpoint(in); got != want {
			t.Errorf("ShotsEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

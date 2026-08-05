package tests

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/corpusfs"
)

// #736: TTML export's optional SMIL companion probed filepath.Join(RootDir,
// rel_path) directly, so an S3 corpus (which has no local file at that path)
// silently produced no SMIL even though the object is readable through the
// configured CorpusFS — and, because SMIL probe failures fail open, the command
// still exited 0. These tests exercise the real `export` command end to end
// against a network-free S3 stub plus a stub ffprobe, and measure what was
// actually probed rather than only whether a file appeared.

// exportFakeS3 is a network-free stub of the S3 client surface the corpus
// backend depends on. It records the keys GetObject was asked for so a test can
// prove the export actually fetched the media through the CorpusFS.
type exportFakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	gets    []string
}

func (f *exportFakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	prefix := aws.ToString(in.Prefix)
	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(false)}
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, body := range f.objects {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		out.Contents = append(out.Contents, s3types.Object{
			Key:  aws.String(key),
			Size: aws.Int64(int64(len(body))),
		})
	}
	return out, nil
}

func (f *exportFakeS3) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(body)))}, nil
}

func (f *exportFakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	key := aws.ToString(in.Key)
	f.mu.Lock()
	body, ok := f.objects[key]
	f.gets = append(f.gets, key)
	f.mu.Unlock()
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func (f *exportFakeS3) getKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.gets...)
}

// useFakeS3Corpus points the corpus-source factory at a network-free stub client
// serving objs (keyed by full object key) and supplies the AWS credentials the
// s3 source kind requires, so `export` builds a real S3-backed CorpusFS over the
// stub.
func useFakeS3Corpus(t *testing.T, objs map[string][]byte) *exportFakeS3 {
	t.Helper()
	stub := &exportFakeS3{objects: objs}
	restore := corpusfs.SetS3ClientBuilderForTest(func(context.Context, corpusfs.Config) (corpusfs.S3API, error) {
		return stub, nil
	})
	t.Cleanup(restore)
	t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	return stub
}

// writeS3TTMLConfig writes a config enabling the TTML+SMIL surface over an s3
// corpus source under the given key prefix.
func writeS3TTMLConfig(t *testing.T, dir, prefix string) string {
	t.Helper()
	path := filepath.Join(dir, "dir2mcp.yaml")
	lines := []string{
		"media_subtitles_ttml_enabled: true",
		"media_subtitles_smil_enabled: true",
		"source_kind: s3",
		"source_s3_bucket: bkt",
		"source_s3_region: us-east-1",
		"source_s3_prefix: " + prefix,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// stubFFprobeOnPATH installs a stub `ffprobe` at the front of PATH that records
// the path argument of every invocation (one per line, in recordFile) and only
// succeeds when that path exists and is non-empty — exactly like the real
// ffprobe, which cannot probe a file that is not there. It reports a small video
// so the rendered SMIL is distinguishable. It returns the record file path.
func stubFFprobeOnPATH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "probed.txt")
	script := `#!/bin/sh
target=""
for arg in "$@"; do target="$arg"; done
printf '%s\n' "$target" >> ` + record + `
if [ ! -s "$target" ]; then
  echo "stub ffprobe: no such media: $target" >&2
  exit 1
fi
cat <<'JSON'
{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":320,"height":240},
{"index":1,"codec_type":"audio","codec_name":"aac","channels":2}],
"format":{"format_name":"mov,mp4,m4a","bit_rate":"800000"}}
JSON
`
	if err := os.WriteFile(filepath.Join(dir, "ffprobe"), []byte(script), 0o755); err != nil {
		t.Fatalf("write ffprobe stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return record
}

// probedPaths returns the paths the stub ffprobe was invoked with.
func probedPaths(t *testing.T, record string) []string {
	t.Helper()
	data, err := os.ReadFile(record)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read probe record: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// TestExportSMILProbesMediaThroughCorpusFS is the #736 regression: with an S3
// corpus, the SMIL companion must be produced from the media fetched through the
// CorpusFS. It measures three things the old code got wrong: the object was
// fetched at all, the path handed to ffprobe was the localized copy (not
// RootDir/rel_path, which does not exist), and the SMIL landed on disk.
func TestExportSMILProbesMediaThroughCorpusFS(t *testing.T) {
	tmp := t.TempDir()
	const rel = "videos/game.mp4"
	seedExportStore(t, filepath.Join(tmp, ".dir2mcp"), rel, `{"language":"en"}`)
	cfgPath := writeS3TTMLConfig(t, tmp, "corpus/")
	stub := useFakeS3Corpus(t, map[string][]byte{
		"corpus/" + rel: []byte("fake-media-bytes"),
	})
	record := stubFFprobeOnPATH(t)
	outPath := filepath.Join(tmp, "out", "game.ttml")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(),
			[]string{"--config", cfgPath, "export", "--format", "ttml",
				"--lang", "en", "--out", outPath, rel})
		if code != 0 {
			t.Fatalf("export exit = %d, stderr=%s", code, stderr.String())
		}
	})

	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("TTML not written: %v", err)
	}

	// The media must have been fetched through the CorpusFS.
	if got := stub.getKeys(); len(got) == 0 {
		t.Fatalf("media was never fetched from the corpus source (GetObject never called); stderr=%s", stderr.String())
	} else if got[0] != "corpus/"+rel {
		t.Fatalf("fetched key = %q, want %q", got[0], "corpus/"+rel)
	}

	// ...and the probe must have run against that localized copy, never against
	// the (nonexistent) local RootDir/rel_path.
	probed := probedPaths(t, record)
	if len(probed) != 1 {
		t.Fatalf("ffprobe invocations = %v, want exactly 1", probed)
	}
	if probed[0] == filepath.Join(tmp, rel) {
		t.Fatalf("probed the local RootDir path %q; an S3 corpus has no file there", probed[0])
	}
	if !strings.Contains(probed[0], "corpus-cache") {
		t.Fatalf("probed %q, want a localized copy under the state dir's corpus-cache", probed[0])
	}

	smilPath := strings.TrimSuffix(outPath, ".ttml") + ".smil"
	smil, err := os.ReadFile(smilPath)
	if err != nil {
		t.Fatalf("SMIL not written for an S3 corpus: %v (stderr=%s)", err, stderr.String())
	}
	for _, want := range []string{`<video src="game.mp4"`, `width="320"`, `content="h264"`} {
		if !strings.Contains(string(smil), want) {
			t.Fatalf("SMIL missing %q in:\n%s", want, smil)
		}
	}

	// The localized download is temporary: nothing may be left behind.
	cacheDir := filepath.Join(tmp, ".dir2mcp", "corpus-cache")
	if entries, err := os.ReadDir(cacheDir); err == nil && len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("localized media not cleaned up; %s still holds %v", cacheDir, names)
	}
}

// TestExportSMILLocalCorpusProbesInRootFile pins that routing the probe through
// the CorpusFS did not change local behavior: a local corpus is probed at its
// real in-root path (no copy, no download) and still yields SMIL.
func TestExportSMILLocalCorpusProbesInRootFile(t *testing.T) {
	tmp := t.TempDir()
	const rel = "media/talk.mp4"
	seedExportStore(t, filepath.Join(tmp, ".dir2mcp"), rel, `{"language":"en"}`)
	cfgPath := writeTTMLConfig(t, tmp, true)
	if err := os.MkdirAll(filepath.Join(tmp, "media"), 0o755); err != nil {
		t.Fatalf("mkdir media: %v", err)
	}
	mediaPath := filepath.Join(tmp, rel)
	if err := os.WriteFile(mediaPath, []byte("fake-media-bytes"), 0o644); err != nil {
		t.Fatalf("write media: %v", err)
	}
	record := stubFFprobeOnPATH(t)
	outPath := filepath.Join(tmp, "out", "talk.ttml")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(),
			[]string{"--config", cfgPath, "export", "--format", "ttml",
				"--lang", "en", "--out", outPath, rel})
		if code != 0 {
			t.Fatalf("export exit = %d, stderr=%s", code, stderr.String())
		}
	})

	probed := probedPaths(t, record)
	if len(probed) != 1 {
		t.Fatalf("ffprobe invocations = %v, want exactly 1", probed)
	}
	// The local file itself is probed. EvalSymlinks resolution (e.g. /var ->
	// /private/var on darwin) means the paths are compared after resolution.
	wantReal, err := filepath.EvalSymlinks(mediaPath)
	if err != nil {
		t.Fatalf("resolve media path: %v", err)
	}
	gotReal, err := filepath.EvalSymlinks(probed[0])
	if err != nil {
		t.Fatalf("resolve probed path %q: %v", probed[0], err)
	}
	if gotReal != wantReal {
		t.Fatalf("probed %q, want the in-root local media %q", gotReal, wantReal)
	}
	smil, err := os.ReadFile(strings.TrimSuffix(outPath, ".ttml") + ".smil")
	if err != nil {
		t.Fatalf("SMIL not written for a local corpus: %v (stderr=%s)", err, stderr.String())
	}
	if !strings.Contains(string(smil), `<video src="talk.mp4"`) {
		t.Fatalf("SMIL references the wrong media:\n%s", smil)
	}
}

// TestExportSMILUnprobeableMediaWarnsAndFailsOpen pins that a genuine probe
// failure on media that IS readable (corrupt file, ffprobe absent) still fails
// open — the export succeeds, the TTML stands, SMIL is omitted — and that the
// reason is reported distinctly from "could not fetch the media".
func TestExportSMILUnprobeableMediaWarnsAndFailsOpen(t *testing.T) {
	tmp := t.TempDir()
	const rel = "media/corrupt.mp4"
	seedExportStore(t, filepath.Join(tmp, ".dir2mcp"), rel, `{"language":"en"}`)
	cfgPath := writeTTMLConfig(t, tmp, true)
	if err := os.MkdirAll(filepath.Join(tmp, "media"), 0o755); err != nil {
		t.Fatalf("mkdir media: %v", err)
	}
	// An empty file exists but the stub ffprobe cannot make sense of it.
	if err := os.WriteFile(filepath.Join(tmp, rel), nil, 0o644); err != nil {
		t.Fatalf("write media: %v", err)
	}
	stubFFprobeOnPATH(t)
	outPath := filepath.Join(tmp, "out", "corrupt.ttml")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(),
			[]string{"--config", cfgPath, "export", "--format", "ttml",
				"--lang", "en", "--out", outPath, rel})
		if code != 0 {
			t.Fatalf("a corrupt media file must not fail the export; exit=%d stderr=%s", code, stderr.String())
		}
	})
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("TTML must still be written: %v", err)
	}
	if _, err := os.Stat(strings.TrimSuffix(outPath, ".ttml") + ".smil"); err == nil {
		t.Fatalf("SMIL must be omitted when the media cannot be probed")
	}
	if !strings.Contains(stderr.String(), "media metadata unavailable") {
		t.Fatalf("probe failure must be reported as a metadata problem, got stderr=%q", stderr.String())
	}
}

// TestExportSMILUnfetchableMediaWarnsAndFailsOpen pins the fail-open contract
// with a diagnostic: when the media cannot be read from the corpus source, the
// TTML is still written and the command still exits 0, but the operator is told
// why SMIL is missing — including under --json/--quiet, where the old
// stdout-only note was suppressed entirely. stdout must stay machine-safe.
func TestExportSMILUnfetchableMediaWarnsAndFailsOpen(t *testing.T) {
	tmp := t.TempDir()
	const rel = "videos/missing.mp4"
	seedExportStore(t, filepath.Join(tmp, ".dir2mcp"), rel, `{"language":"en"}`)
	cfgPath := writeS3TTMLConfig(t, tmp, "corpus/")
	// The bucket holds a different object, so localizing rel fails.
	useFakeS3Corpus(t, map[string][]byte{"corpus/videos/other.mp4": []byte("x")})
	record := stubFFprobeOnPATH(t)
	outPath := filepath.Join(tmp, "out", "missing.ttml")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(),
			[]string{"--config", cfgPath, "--json", "--quiet", "export", "--format", "ttml",
				"--lang", "en", "--out", outPath, rel})
		if code != 0 {
			t.Fatalf("export must fail open on an unreadable media object; exit=%d stderr=%s", code, stderr.String())
		}
	})

	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("TTML must still be written: %v", err)
	}
	if _, err := os.Stat(strings.TrimSuffix(outPath, ".ttml") + ".smil"); err == nil {
		t.Fatalf("SMIL must be omitted when the media cannot be read")
	}
	if probed := probedPaths(t, record); len(probed) != 0 {
		t.Fatalf("nothing should have been probed when the media could not be fetched, got %v", probed)
	}
	if out := strings.TrimSpace(stdout.String()); out != "" && !strings.HasPrefix(out, "{") {
		t.Fatalf("stdout must stay machine-safe under --json, got %q", out)
	}
	if !strings.Contains(strings.ToLower(stderr.String()), "smil") {
		t.Fatalf("operator got no diagnostic explaining the missing SMIL; stderr=%q", stderr.String())
	}
}

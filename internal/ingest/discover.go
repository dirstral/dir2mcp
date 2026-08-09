package ingest

import (
	"context"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/corpusfs"
)

// DefaultMaxFileSizeBytes returns the default ingest file-size cap.
func DefaultMaxFileSizeBytes() int64 {
	return corpusfs.DefaultMaxFileSizeBytes()
}

// DiscoveredFile holds metadata collected during file system discovery. It is
// an alias of corpusfs.DiscoveredFile so discovery can run against any CorpusFS
// backend (local filesystem, NFS path, or S3) without callers changing.
type DiscoveredFile = corpusfs.DiscoveredFile

// MediaVariantSelect names the policy used to pick one rendition from a group
// of media variants that share a normalized name (spec §8.6.5).
type MediaVariantSelect string

const (
	// MediaVariantSelectBest picks the highest-quality rendition: highest
	// detected resolution, then largest size, then lexically-lowest path.
	MediaVariantSelectBest MediaVariantSelect = "best"
	// MediaVariantSelectFirst picks the lexically-lowest path in the group,
	// ignoring resolution and size. Still deterministic.
	MediaVariantSelectFirst MediaVariantSelect = "first"
)

// MediaVariantOptions controls media multi-rendition grouping and selection
// during discovery (spec §8.6.5, config key `media.variants`).
//
// When Group is false (the default) discovery is unchanged: every rendition is
// returned. When Group is true, discovered media files are grouped by their
// normalized name (rendition markers stripped) and a single canonical rendition
// per group is kept; non-canonical renditions are dropped so downstream
// ingestion never duplicates chunks/embeddings across renditions of the same
// logical media.
type MediaVariantOptions struct {
	// Group enables variant grouping/dedup. Disabled by default.
	Group bool
	// Select chooses the canonical rendition within a group. Empty defaults to
	// MediaVariantSelectBest.
	Select MediaVariantSelect
}

// MediaVariantOptionsFromConfig resolves media-variant discovery behavior from
// config (spec §8.6.5, keys media.variants.group / media.variants.select).
// Disabled by default; an empty/unknown select falls back to "best".
func MediaVariantOptionsFromConfig(cfg config.Config) MediaVariantOptions {
	sel := MediaVariantSelect(strings.ToLower(strings.TrimSpace(cfg.MediaVariantsSelect)))
	if sel != MediaVariantSelectBest && sel != MediaVariantSelectFirst {
		sel = MediaVariantSelectBest
	}
	return MediaVariantOptions{
		Group:  cfg.MediaVariantsGroup,
		Select: sel,
	}
}

// DiscoverOptions controls optional discovery behavior.
type DiscoverOptions struct {
	MaxSizeBytes   int64
	UseGitIgnore   bool
	FollowSymlinks bool
	MediaVariants  MediaVariantOptions
	// ExcludeDirs is the directory ignore list (config `ingest.exclude_dirs`,
	// SPEC §7.1, issue #773). nil keeps the default list. A non-nil value
	// REPLACES the default list in full. `.dir2mcp` is always kept.
	//
	// The initial scan and the file watcher both read this one value, so the
	// watcher cannot judge a directory differently from the scan (#716).
	ExcludeDirs []string
	// ScanCache, when non-nil, is the optional directory-discovery cache (issue
	// #267 item 5) passed through to the local-filesystem walker. nil = disabled
	// (a full re-walk every run). See corpusfs.ScanCache.
	ScanCache corpusfs.ScanCache
	// OnOversize, when non-nil, is invoked once for each regular file excluded at
	// discovery solely because its size exceeds MaxSizeBytes. It makes the
	// size-cap drop observable instead of silent (issue #497); the file is still
	// excluded. See corpusfs.Options.OnOversize.
	OnOversize func(relPath string, size int64)
	// OnUnsafeKey, when non-nil, is invoked once for each object key excluded
	// because its corpus-relative name is not a usable rel_path (#735). Only
	// the S3 backend can produce these: a local walk cannot hand back a path
	// above the directory it was asked to walk, but a bucket is untrusted
	// input and its keys are whatever someone put there.
	OnUnsafeKey func(key string, err error)
	// OnSkippedSymlink, when non-nil, is invoked once for each entry dropped
	// because it is a symlink and FollowSymlinks is false (#781). The entry is
	// still dropped; this only makes the drop visible, so a corpus made of links
	// is distinguishable from an empty one. See corpusfs.Options.OnSkippedSymlink.
	OnSkippedSymlink func(relPath string)
}

// DefaultDiscoverOptions returns discovery defaults used by ingestion.
func DefaultDiscoverOptions() DiscoverOptions {
	return DiscoverOptions{
		MaxSizeBytes:   corpusfs.DefaultMaxFileSizeBytes(),
		UseGitIgnore:   false,
		FollowSymlinks: false,
		MediaVariants: MediaVariantOptions{
			Group:  false,
			Select: MediaVariantSelectBest,
		},
	}
}

// corpusfsOptions converts ingest DiscoverOptions to corpusfs.Options.
func (o DiscoverOptions) corpusfsOptions() corpusfs.Options {
	return corpusfs.Options{
		MaxSizeBytes:     o.MaxSizeBytes,
		UseGitIgnore:     o.UseGitIgnore,
		FollowSymlinks:   o.FollowSymlinks,
		ExcludeDirs:      o.ExcludeDirs,
		ScanCache:        o.ScanCache,
		OnOversize:       o.OnOversize,
		OnUnsafeKey:      o.OnUnsafeKey,
		OnSkippedSymlink: o.OnSkippedSymlink,
	}
}

// ExcludedDirs resolves the directory ignore set these options select. The
// watcher calls it so it applies the very list the scan applies.
func (o DiscoverOptions) ExcludedDirs() corpusfs.ExcludedDirSet {
	return corpusfs.ResolveExcludedDirs(o.ExcludeDirs)
}

// DiscoverFiles walks rootDir and returns regular files that pass default
// discovery policies (skip symlinks, known heavy dirs, and over-limit files).
//
// It runs against a local-filesystem CorpusFS so existing callers keep working
// unchanged; the walk logic itself now lives in internal/corpusfs.
func DiscoverFiles(ctx context.Context, rootDir string, maxSizeBytes int64) ([]DiscoveredFile, error) {
	options := DefaultDiscoverOptions()
	options.MaxSizeBytes = maxSizeBytes
	return DiscoverFilesWithOptions(ctx, rootDir, options)
}

// DiscoverFilesWithOptions walks rootDir and returns regular files that match
// discovery policies and caller-provided options, via a local CorpusFS backend.
//
// When options.MediaVariants.Group is enabled, media renditions that share a
// normalized name are collapsed to a single canonical file (spec §8.6.5).
func DiscoverFilesWithOptions(ctx context.Context, rootDir string, options DiscoverOptions) ([]DiscoveredFile, error) {
	files, err := corpusfs.NewLocalFS(rootDir).Walk(ctx, rootDir, options.corpusfsOptions())
	if err != nil {
		return nil, err
	}
	return SelectMediaVariants(files, options.MediaVariants), nil
}

// mediaVariantExtensions is the set of media file extensions (audio + video)
// that participate in variant grouping. Mirrors the audio/video sets used by
// ingest classification plus common container/stream formats produced by
// multi-rendition archives. Membership is what makes a file a "media rendition"
// for §8.6.5; all other files are passed through untouched.
var mediaVariantExtensions = map[string]bool{
	// audio
	".mp3": true, ".wav": true, ".m4a": true, ".flac": true,
	".aac": true, ".ogg": true, ".opus": true,
	// video / containers / streams
	".mp4": true, ".mov": true, ".mkv": true, ".m4v": true,
	".webm": true, ".flv": true, ".ts": true, ".avi": true,
	".wmv": true, ".mpg": true, ".mpeg": true,
}

// renditionMarkerPattern matches one rendition marker embedded in a filename
// stem, bounded by a separator (`.`, `_`, `-`) on the left and a separator or
// the stem boundary on the right. It is deterministic and intentionally generic
// (no corpus-specific tokens):
//
//   - resolution markers: 180p..4320p, plus 2k/4k/8k and 720i/1080i
//     (e.g. clip.1080p.mp4, clip_720P.mp4, clip-480i.mp4, clip.4k.mp4)
//   - bitrate markers: a 2-5 digit number followed by k/kbps/kb/m/mbps
//     (e.g. clip.128k.mp3, clip_2500kbps.mp4, clip-12m.mp4)
//   - codec/quality suffixes: common audio/video codec and quality tokens
//     (e.g. clip.h264.mp4, clip_hevc.mp4, clip-aac.m4a, clip.hd.mp4)
//
// Group 1 captures the trailing boundary character (a separator, or empty at the
// stem end). Replacement substitutes the captured boundary back so that
// adjacent markers (e.g. ".1080p.h264") are still each strippable on a later
// pass — RE2 has no lookahead, so the trailing separator must be matched but is
// restored to act as a shared boundary.
var renditionMarkerPattern = regexp.MustCompile(
	`(?i)[._-](?:` +
		`\d{3,4}[pi]` + // 1080p, 720i, 480P
		`|[248]k` + // 2k, 4k, 8k
		`|\d{2,5}(?:kbps|kb|k|mbps|mb|m)` + // 128k, 2500kbps, 12m
		`|h\.?26[45]|x26[45]|hevc|avc|av1|vp9|vp8|mpeg4|xvid|divx` + // video codecs
		`|aac|mp3|ac3|eac3|opus|flac|dts` + // audio codecs
		`|hd|fhd|uhd|sd|hq|lq|sq` + // quality tiers
		`)([._-]|$)`,
)

// normalizeVariantName returns the rendition-independent grouping key for a
// relative path: the directory plus the filename with all rendition markers
// stripped from the stem (extension preserved, lowercased). Files with no
// detectable markers normalize to their own (lowercased) path, so they form
// singleton groups and are never merged with anything else.
func normalizeVariantName(relPath string) string {
	dir := path.Dir(relPath)
	base := path.Base(relPath)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	// Repeatedly strip markers, restoring the captured trailing boundary ($1) so
	// runs of adjacent markers (".1080p.h264.aac") fully collapse across passes.
	prev := ""
	for stem != prev {
		prev = stem
		stem = renditionMarkerPattern.ReplaceAllString(stem, "$1")
	}
	stem = strings.Trim(stem, "._-")

	normalized := stem + strings.ToLower(ext)
	if dir != "." && dir != "" {
		normalized = dir + "/" + normalized
	}
	return strings.ToLower(normalized)
}

// extractResolution returns the highest detected resolution height in a
// filename (e.g. 1080 for "clip.1080p.mp4"; "4k" maps to 2160, "2k" to 1440,
// "8k" to 4320). Returns 0 when no resolution marker is present.
func extractResolution(relPath string) int {
	base := strings.ToLower(path.Base(relPath))
	best := 0
	for _, m := range resolutionPattern.FindAllStringSubmatch(base, -1) {
		var h int
		switch {
		case m[1] != "":
			h, _ = strconv.Atoi(m[1])
		case m[2] != "":
			switch m[2] {
			case "2":
				h = 1440
			case "4":
				h = 2160
			case "8":
				h = 4320
			}
		}
		if h > best {
			best = h
		}
	}
	return best
}

// resolutionPattern matches resolution markers and captures either the numeric
// pixel height (group 1) or the k-shorthand digit (group 2), bounded by
// separators or the stem boundary.
var resolutionPattern = regexp.MustCompile(`(?i)[._-](?:(\d{3,4})[pi]|([248])k)(?:[._-]|$)`)

// isMediaVariantFile reports whether a relative path is a media rendition
// eligible for variant grouping.
func isMediaVariantFile(relPath string) bool {
	return mediaVariantExtensions[strings.ToLower(path.Ext(relPath))]
}

// SelectMediaVariants applies media multi-rendition dedup (spec §8.6.5) to the
// discovered files. When opts.Group is false the input is returned unchanged
// (preserving order). When enabled, media renditions sharing a normalized name
// are grouped and a single canonical rendition per group is kept; non-media and
// single-rendition files are unaffected. Output order matches the input order
// of the surviving files, so the result stays deterministic for a sorted walk.
func SelectMediaVariants(files []DiscoveredFile, opts MediaVariantOptions) []DiscoveredFile {
	if !opts.Group || len(files) == 0 {
		return files
	}
	selectPolicy := opts.Select
	if selectPolicy == "" {
		selectPolicy = MediaVariantSelectBest
	}

	// winners maps normalized group key -> index (into files) of the canonical
	// rendition chosen so far.
	winners := make(map[string]int)
	for i := range files {
		if !isMediaVariantFile(files[i].RelPath) {
			continue
		}
		key := normalizeVariantName(files[i].RelPath)
		cur, ok := winners[key]
		if !ok || variantBetter(files[i], files[cur], selectPolicy) {
			winners[key] = i
		}
	}

	out := make([]DiscoveredFile, 0, len(files))
	for i := range files {
		if !isMediaVariantFile(files[i].RelPath) {
			out = append(out, files[i])
			continue
		}
		key := normalizeVariantName(files[i].RelPath)
		if winners[key] == i {
			out = append(out, files[i])
		}
	}
	return out
}

// variantBetter reports whether candidate should replace the current canonical
// rendition under the given selection policy. Comparison is total and
// deterministic.
func variantBetter(candidate, current DiscoveredFile, policy MediaVariantSelect) bool {
	if policy == MediaVariantSelectFirst {
		return candidate.RelPath < current.RelPath
	}
	// best: highest resolution, then largest size, then lexically-lowest path.
	cr, rr := extractResolution(candidate.RelPath), extractResolution(current.RelPath)
	if cr != rr {
		return cr > rr
	}
	if candidate.SizeBytes != current.SizeBytes {
		return candidate.SizeBytes > current.SizeBytes
	}
	return candidate.RelPath < current.RelPath
}

package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// sidecarExtensions are the subtitle sidecar formats recognised next to a media
// file (spec §8.6.4). They are checked case-insensitively.
var sidecarExtensions = []string{".vtt", ".srt", ".ttml"}

// sidecarSource is the meta_json source value recorded on a sidecar-derived
// transcript representation. Sidecar transcripts are authored, not
// model-derived, so they carry no STT provider/model derivation identity and are
// NOT invalidated by an STT model change (spec §8.6.7).
const sidecarSource = "sidecar"

// sidecarFile is a discovered subtitle sidecar for a media document: its corpus
// rel_path, the language tag parsed from the filename (empty when the sidecar is
// undifferentiated, e.g. "clip.vtt"), the lowercased extension, and the sidecar
// file's modification time used for the mtime freshness gate (§7.6).
type sidecarFile struct {
	RelPath   string
	Lang      string
	Ext       string
	MTimeUnix int64
}

// transcriptMeta is the meta_json shape persisted on a transcript
// representation (spec §5.2). For sidecar transcripts Source is "sidecar" and
// the STT provider/model fields are omitted (omitempty) so the representation
// carries no model derivation identity (§8.6.7).
type transcriptMeta struct {
	Source     string `json:"source,omitempty"`
	Language   string `json:"language,omitempty"`
	Timestamps bool   `json:"timestamps"`
	Format     string `json:"format,omitempty"`
}

// setSidecarIndex records the mtime of every discovered file by rel_path so the
// transcript path can detect media siblings (sidecars) and gate ingestion on
// their freshness without an extra filesystem stat. It is set once per scan from
// the same walk that drives discovery, so it works for any CorpusFS backend.
// PathExcludes are honoured here so an operator who excludes e.g. "**/*.vtt"
// never has those files read or persisted as transcripts (the exclude contract).
func (s *Service) setSidecarIndex(files []DiscoveredFile) {
	idx := make(map[string]int64, len(files))
	for _, f := range files {
		if matchesAnyPathExclude(f.RelPath, s.cfg.PathExcludes) {
			continue
		}
		idx[f.RelPath] = f.MTimeUnix
	}
	s.sidecarMu.Lock()
	s.sidecarIndex = idx
	s.sidecarMu.Unlock()
}

// sidecarsEnabled reports whether subtitle sidecar ingestion is active. It
// defaults to true (spec default media.sidecars.enabled: true) and is disabled
// only by an explicit opt-out.
func (s *Service) sidecarsEnabled() bool {
	return !s.cfg.MediaSidecarsDisabled
}

// findSidecars returns the subtitle sidecars sitting next to the media document,
// resolved through the corpus FS so it works for any backend. A sidecar matches
// when its path is "<media-without-ext>.<ext>" (undifferentiated) or
// "<media-without-ext>.<lang>.<ext>" (per-language). Results are sorted by
// (language, extension) for deterministic selection. When the scan-built index
// is unavailable (e.g. a direct GenerateTranscriptRepresentation call in tests),
// it falls back to walking the corpus once.
func (s *Service) findSidecars(ctx context.Context, mediaRelPath string) []sidecarFile {
	index := s.sidecarIndexOrWalk(ctx)
	if len(index) == 0 {
		return nil
	}
	base := stripExt(mediaRelPath)
	// mediaExt is the media file's own extension token (e.g. "mp3"), lowercased
	// and without the leading dot. A sidecar whose single tail token equals it —
	// "clip.mp3.vtt" for media "clip.mp3" — is the media filename plus a subtitle
	// extension, not a language-tagged sidecar, so it must be rejected.
	mediaExt := strings.TrimPrefix(strings.ToLower(path.Ext(mediaRelPath)), ".")
	var out []sidecarFile
	for relPath, mtime := range index {
		ext := strings.ToLower(path.Ext(relPath))
		if !isSidecarExt(ext) {
			continue
		}
		// stem is the sidecar path without its subtitle extension. tail is the
		// remainder after the media base. Only the documented shapes bind:
		//   "<base><ext>"        → tail == ""        (no language)
		//   "<base>.<lang><ext>" → tail == ".<lang>" (single token, no extra dots)
		// Anything else — an extra dotted segment ("clip.notes.en.vtt") or the
		// media extension reused as the token ("clip.mp3.vtt") for media
		// "clip.mp3" — is NOT this media's sidecar and is skipped, so it cannot
		// wrongly suppress STT.
		stem := strings.TrimSuffix(relPath, ext)
		if !strings.HasPrefix(stem, base) {
			continue
		}
		tail := strings.TrimPrefix(stem, base)
		lang := ""
		switch {
		case tail == "":
			// undifferentiated sidecar, no language tag
		case strings.HasPrefix(tail, ".") && !strings.Contains(tail[1:], "."):
			lang = strings.TrimSpace(tail[1:])
			if lang == "" {
				continue
			}
			if mediaExt != "" && strings.EqualFold(lang, mediaExt) {
				// e.g. "clip.mp3.vtt" for media "clip.mp3": the token is the
				// media's own extension, not a language tag.
				continue
			}
		default:
			continue
		}
		out = append(out, sidecarFile{RelPath: relPath, Lang: lang, Ext: ext, MTimeUnix: mtime})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Lang != out[j].Lang {
			return out[i].Lang < out[j].Lang
		}
		return out[i].Ext < out[j].Ext
	})
	return out
}

// sidecarIndexOrWalk returns the scan-built sidecar index, falling back to a
// one-shot corpus walk when none was set (direct/standalone calls). The walk
// result is not cached so a standalone call always sees current mtimes.
func (s *Service) sidecarIndexOrWalk(ctx context.Context) map[string]int64 {
	s.sidecarMu.RLock()
	idx := s.sidecarIndex
	s.sidecarMu.RUnlock()
	if idx != nil {
		return idx
	}
	files, err := s.corpusFS().Walk(ctx, s.cfg.RootDir, DiscoverOptionsFromConfig(s.cfg).corpusfsOptions())
	if err != nil {
		s.getLogger().Printf("sidecar walk for %s failed: %v", s.cfg.RootDir, err)
		return nil
	}
	out := make(map[string]int64, len(files))
	for _, f := range files {
		// Honour PathExcludes here too so excluded files (e.g. "**/*.vtt") are
		// never used as sidecars on the standalone fallback path.
		if matchesAnyPathExclude(f.RelPath, s.cfg.PathExcludes) {
			continue
		}
		out[f.RelPath] = f.MTimeUnix
	}
	return out
}

// sidecarFingerprint returns a stable fingerprint of the media document's
// sidecars (their sorted rel_paths and mtimes). Folding it into the media
// document's content hash makes the incremental gate (§7.6) re-process the media
// whenever a sidecar is added, removed, or modified — even though the media
// bytes are unchanged. Returns "" when sidecars are disabled or the document is
// not a media type with any sidecar.
func (s *Service) sidecarFingerprint(ctx context.Context, relPath, docType string) string {
	if !s.sidecarsEnabled() || !isSidecarMediaType(docType) {
		return ""
	}
	sidecars := s.findSidecars(ctx, relPath)
	if len(sidecars) == 0 {
		return ""
	}
	parts := make([]string, 0, len(sidecars))
	for _, sc := range sidecars {
		parts = append(parts, fmt.Sprintf("%s@%d", sc.RelPath, sc.MTimeUnix))
	}
	return strings.Join(parts, "\n")
}

// ingestSidecarTranscripts ingests the media document's subtitle sidecars as its
// transcript representation(s), one representation per language, and reports
// whether any were ingested. When it returns true the caller MUST skip STT — an
// authored sidecar is authoritative over machine transcription (§8.6.4). Sidecar
// transcripts bypass the output quality gate: they are authored text, not
// model-derived output, so quarantining them as degenerate model output would be
// wrong (§8.6.6/§8.6.7). A parse failure for one sidecar is logged and skipped;
// remaining sidecars (and, if none parse, STT) still proceed.
func (s *Service) ingestSidecarTranscripts(ctx context.Context, doc model.Document) (bool, error) {
	if !s.sidecarsEnabled() || !isSidecarMediaType(doc.DocType) {
		return false, nil
	}
	groups := s.collectSidecarCues(ctx, doc)
	if len(groups) == 0 {
		return false, nil
	}
	langs := make([]string, 0, len(groups))
	for lang := range groups {
		langs = append(langs, lang)
	}
	sort.Strings(langs)

	ingested := false
	for _, lang := range langs {
		segments := chunkSubtitleCues(groups[lang])
		if len(segments) == 0 {
			continue
		}
		if err := s.persistSidecarTranscript(ctx, doc, lang, groups[lang], segments); err != nil {
			return ingested, err
		}
		s.addRepresentations(1)
		ingested = true
	}
	return ingested, nil
}

// collectSidecarCues parses every sidecar of the media document and groups the
// resulting cues by language. A per-language sidecar (clip.en.vtt) contributes
// to its own language; a bilingual TTML contributes to each xml:lang it carries.
// Each language's cues are sorted by start time for stable, deterministic spans.
func (s *Service) collectSidecarCues(ctx context.Context, doc model.Document) map[string][]subtitle.Cue {
	groups := map[string][]subtitle.Cue{}
	for _, sc := range s.findSidecars(ctx, doc.RelPath) {
		content, err := s.readSidecar(ctx, sc.RelPath)
		if err != nil {
			s.getLogger().Printf("sidecar read %s: %v", sc.RelPath, err)
			continue
		}
		for lang, cues := range parseSidecar(sc, content) {
			groups[lang] = append(groups[lang], cues...)
		}
	}
	for lang := range groups {
		cues := groups[lang]
		sort.SliceStable(cues, func(i, j int) bool {
			if cues[i].StartMS != cues[j].StartMS {
				return cues[i].StartMS < cues[j].StartMS
			}
			return cues[i].EndMS < cues[j].EndMS
		})
		groups[lang] = cues
	}
	return groups
}

// parseSidecar parses one sidecar file into language-keyed cue sets. VTT/SRT
// carry a single (file-level) language taken from the filename; TTML may be
// bilingual and is split per xml:lang, with the filename language used as a
// fallback for any cues that declare none.
func parseSidecar(sc sidecarFile, content string) map[string][]subtitle.Cue {
	out := map[string][]subtitle.Cue{}
	switch sc.Ext {
	case ".ttml":
		groups, err := subtitle.ParseTTMLByLang(content)
		if err != nil {
			return out
		}
		for _, g := range groups {
			lang := g.Lang
			if lang == "" {
				lang = sc.Lang
			}
			out[lang] = append(out[lang], g.Cues...)
		}
	case ".srt":
		if cues, err := subtitle.ParseSRT(content); err == nil {
			out[sc.Lang] = cues
		}
	default: // .vtt
		if cues, err := subtitle.ParseVTT(content); err == nil {
			out[sc.Lang] = cues
		}
	}
	return out
}

// persistSidecarTranscript writes one language's sidecar cues as a transcript
// representation plus its time-spanned chunks. The chunks are inserted healthy
// (no quarantine decision) so they embed normally; sidecar text is never
// screened by the quality gate (§8.6.6/§8.6.7). meta_json records source=sidecar
// and the language so retrieval and re-derivation treat it as authored.
func (s *Service) persistSidecarTranscript(ctx context.Context, doc model.Document, lang string, cues []subtitle.Cue, segments []chunkSegment) error {
	meta := transcriptMeta{Source: sidecarSource, Language: lang, Timestamps: true}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal sidecar transcript meta: %w", err)
	}
	rep := model.Representation{
		DocID:       doc.DocID,
		RepType:     TranscriptRepType(lang),
		RepHash:     sidecarRepHash(lang, cues),
		MetaJSON:    string(metaJSON),
		CreatedUnix: time.Now().Unix(),
		Deleted:     false,
	}
	repID, err := s.repGen.store.UpsertRepresentation(ctx, rep)
	if err != nil {
		return fmt.Errorf("upsert sidecar transcript representation: %w", err)
	}
	// quarantineDecision{} = insert healthy chunks; sidecar text bypasses the gate.
	if err := s.repGen.upsertChunksForRepresentation(ctx, repID, "text", segments, quarantineDecision{}); err != nil {
		return fmt.Errorf("persist sidecar transcript chunks: %w", err)
	}
	return nil
}

// sidecarRepHash derives a stable rep_hash from the language and cue content so
// an unchanged sidecar dedups across re-ingests and a changed one produces a new
// representation identity.
func sidecarRepHash(lang string, cues []subtitle.Cue) string {
	var b strings.Builder
	b.WriteString(lang)
	b.WriteByte('\n')
	for _, c := range cues {
		fmt.Fprintf(&b, "%d-%d:%s\n", c.StartMS, c.EndMS, c.Text)
	}
	return computeRepHash([]byte(b.String()))
}

// readSidecar reads a sidecar file's bytes through the corpus FS and normalises
// them to valid UTF-8 with LF line endings.
func (s *Service) readSidecar(ctx context.Context, relPath string) (string, error) {
	rc, err := s.corpusFS().Open(ctx, relPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	return string(NormalizeUTF8(raw)), nil
}

// IngestSidecarTranscripts exposes sidecar transcript ingestion for tests. It
// returns whether any sidecar was ingested (true means STT should be skipped).
func (s *Service) IngestSidecarTranscripts(ctx context.Context, doc model.Document) (bool, error) {
	return s.ingestSidecarTranscripts(ctx, doc)
}

// isSidecarMediaType reports whether a sidecar transcript may stand in for STT
// on this doc type. Audio and video are the media types that carry transcripts
// (§8.6.4).
func isSidecarMediaType(docType string) bool {
	return docType == "audio" || docType == "video"
}

// isSidecarExt reports whether ext (lowercased, with leading dot) is a
// recognised subtitle sidecar extension.
func isSidecarExt(ext string) bool {
	for _, e := range sidecarExtensions {
		if ext == e {
			return true
		}
	}
	return false
}

// stripExt removes the final extension from a path, preserving directories.
func stripExt(p string) string {
	return strings.TrimSuffix(p, path.Ext(p))
}

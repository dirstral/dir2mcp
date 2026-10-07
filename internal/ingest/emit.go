package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/internal/subexport"
	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// Subtitle write-back (SPEC §8.6.14).
//
// Once a media document's transcript representations exist, the pipeline
// renders the configured subtitle formats and writes them beside the media (or
// under media.subtitles.emit.dir). The bytes are exactly what `export` produces
// for the same document, language and format: both go through
// internal/subexport.
//
// The load-bearing rule is OWNERSHIP. A file this code wrote is an output, not
// an authored sidecar. It is recorded (rel_path, size, mtime, content hash) in
// the store, and sidecar discovery (§8.6.4) and the sidecar fingerprint (§7.6)
// skip a recorded file whose size and mtime are unchanged. So writing a VTT
// beside a video never changes the video's document identity, never re-ingests
// the VTT as the video's "authored" transcript, never bypasses the §8.6.6 gate,
// and never blocks a re-derivation when the STT or translation identity changes
// (§8.6.7). A recorded file that HAS changed on disk — someone fixed a cue — is
// no longer owned: the record is dropped and from then on it is an authored
// sidecar with §8.6.4 precedence.

// emittedArtifactStore is the optional store capability write-back needs: the
// df-003 §5.6 ownership rows. The SQLite store implements it; a store that does
// not disables write-back (logged once) rather than failing ingest.
type emittedArtifactStore interface {
	UpsertEmittedArtifact(ctx context.Context, a store.EmittedArtifact) error
	DeleteEmittedArtifact(ctx context.Context, relPath string) error
	EmittedArtifactsForDoc(ctx context.Context, docID int64) ([]store.EmittedArtifact, error)
	AllEmittedArtifacts(ctx context.Context) ([]store.EmittedArtifact, error)
}

// manifestErrSubtitleWriteFailed (§14.4) classifies a §8.6.14 artifact that
// could not be written. Non-fatal: the transcript stays indexed.
const manifestErrSubtitleWriteFailed = "SUBTITLE_WRITE_FAILED"

// emitFileMode is the mode of a written subtitle file before the umask. These
// files are for players and editors, so unlike the state directory they are
// world-readable by default.
const emitFileMode = 0o644

// ownedArtifact is the in-memory projection of one df-003 §5.6 row.
type ownedArtifact struct {
	DocID         int64
	Format        string
	Lang          string
	SizeBytes     int64
	MTimeUnix     int64
	ContentSHA256 string
}

// sidecarStat is what the scan-built sidecar index records per file: the two
// cheap stat fields the §8.6.4 mtime gate and the §8.6.14 ownership test need.
type sidecarStat struct {
	MTimeUnix int64
	SizeBytes int64
}

// emitEnabled reports whether write-back is configured on.
func (s *Service) emitEnabled() bool {
	return s != nil && s.cfg.MediaSubtitlesEmitEnabled
}

// loadOwnedArtifacts replaces the in-memory ownership index with the store's
// rows. Called once per scan, after discovery, so every sidecar lookup in the
// scan sees the same ownership picture. It runs whether or not write-back is
// currently enabled: ownership is a property of the record, not of the flag, so
// an operator who enables write-back, runs a scan and then disables it does not
// find the files it wrote re-ingested as authored transcripts. A store without
// the capability leaves the index empty, which is correct: nothing is owned.
func (s *Service) loadOwnedArtifacts(ctx context.Context) {
	as, ok := s.store.(emittedArtifactStore)
	if !ok {
		s.ownedMu.Lock()
		s.ownedLoaded = true
		s.ownedMu.Unlock()
		return
	}
	rows, err := as.AllEmittedArtifacts(ctx)
	if err != nil {
		s.getLogger().Printf("subtitle write-back: load owned artifacts: %v (treating none as owned this scan)", err)
		rows = nil
	}
	// A row applies only under the output root it was written under (SPEC
	// §8.6.14). Sidecar discovery runs over the corpus, so it sees only the rows
	// written beside the media (root ""): a row written under a separate `dir`
	// never excludes, and is never dropped by, an in-corpus file that shares its
	// corpus-relative path. Write-back sees the rows of the current root. Rows
	// of an earlier non-empty root describe no file at all.
	root := s.emitOutputRoot()
	inCorpus := make(map[string]ownedArtifact)
	outRoot := make(map[string]ownedArtifact)
	for _, r := range rows {
		rec := ownedArtifact{DocID: r.DocID, Format: r.Format, Lang: r.Lang,
			SizeBytes: r.SizeBytes, MTimeUnix: r.MTimeUnix, ContentSHA256: r.ContentSHA256}
		switch r.OutputRoot {
		case "":
			inCorpus[r.RelPath] = rec
		case root:
			outRoot[r.RelPath] = rec
		}
	}
	s.ownedMu.Lock()
	s.ownedArtifacts = inCorpus
	s.outRootOwned = outRoot
	s.ownedLoaded = true
	s.ownedMu.Unlock()
}

// ownedIndexLocked returns the ownership index for an output root: the
// in-corpus index (rows written beside the media, plus marker adoptions) for
// root "", else the index of the current emit.dir. The caller holds ownedMu
// for writing.
func (s *Service) ownedIndexLocked(root string) map[string]ownedArtifact {
	if root == "" {
		if s.ownedArtifacts == nil {
			s.ownedArtifacts = make(map[string]ownedArtifact)
		}
		return s.ownedArtifacts
	}
	if s.outRootOwned == nil {
		s.outRootOwned = make(map[string]ownedArtifact)
	}
	return s.outRootOwned
}

// ensureOwnedLoaded loads the ownership index on first use so a standalone
// call (outside Run) still honors ownership.
func (s *Service) ensureOwnedLoaded(ctx context.Context) {
	s.ownedMu.RLock()
	loaded := s.ownedLoaded
	s.ownedMu.RUnlock()
	if !loaded {
		s.loadOwnedArtifacts(ctx)
	}
}

// lookupOwned returns the ownership record for relPath under an output root
// ("" = in the corpus), if any. The map is read under the lock: the scan loop
// and the filesystem watcher may consult it while a write-back adds to it.
func (s *Service) lookupOwned(ctx context.Context, root, relPath string) (ownedArtifact, bool) {
	s.ensureOwnedLoaded(ctx)
	s.ownedMu.RLock()
	defer s.ownedMu.RUnlock()
	idx := s.ownedArtifacts
	if root != "" {
		idx = s.outRootOwned
	}
	rec, ok := idx[relPath]
	return rec, ok
}

// rememberOwned records a freshly written artifact in the index of the root it
// was written under.
func (s *Service) rememberOwned(a store.EmittedArtifact) {
	s.ownedMu.Lock()
	defer s.ownedMu.Unlock()
	s.ownedIndexLocked(a.OutputRoot)[a.RelPath] = ownedArtifact{DocID: a.DocID, Format: a.Format, Lang: a.Lang,
		SizeBytes: a.SizeBytes, MTimeUnix: a.MTimeUnix, ContentSHA256: a.ContentSHA256}
}

// forgetOwned drops an artifact from the index of its output root and from the
// store: the file changed on disk, so it is authored now. A marker adoption that
// was never persisted (DocID 0) has no row, so the store is not touched for it:
// a row at that rel_path, if any, belongs to another root.
func (s *Service) forgetOwned(ctx context.Context, root, relPath string) {
	s.ownedMu.Lock()
	idx := s.ownedIndexLocked(root)
	rec, had := idx[relPath]
	delete(idx, relPath)
	s.ownedMu.Unlock()
	if !had || rec.DocID == 0 {
		return
	}
	if as, ok := s.store.(emittedArtifactStore); ok {
		if err := as.DeleteEmittedArtifact(ctx, relPath); err != nil {
			s.getLogger().Printf("subtitle write-back: drop ownership of %s: %v", relPath, err)
		}
	}
}

// isOwnedSidecar reports whether relPath is a subtitle file this pipeline wrote
// and that is unchanged since (size and mtime match the record). It is the
// §8.6.14 exclusion applied by sidecar discovery, and it applies regardless of
// whether write-back is currently enabled (see loadOwnedArtifacts). The test is
// a stat, deliberately: discovery runs over every file on every scan and must
// not read subtitle bytes; the content hash is verified where it matters, right
// before a rewrite (writeArtifact). A recorded file whose stat no longer matches
// has been edited: it stops being owned here and now, and the caller treats it
// as an authored sidecar.
func (s *Service) isOwnedSidecar(ctx context.Context, relPath string, st sidecarStat) bool {
	rec, ok := s.lookupOwned(ctx, "", relPath)
	if !ok {
		// No ownership row. The file may still be dir2mcp's own output whose row
		// was lost with the state database: an intact provenance marker proves it.
		return s.adoptMarkedSidecar(ctx, relPath, st)
	}
	if rec.MTimeUnix == st.MTimeUnix && rec.SizeBytes == st.SizeBytes {
		return true
	}
	s.getLogger().Printf("subtitle write-back: %s changed on disk since it was written; it is an authored sidecar from now on", relPath)
	s.forgetOwned(ctx, "", relPath)
	return false
}

// adoptMarkedSidecar is the recovery half of the ownership rule (SPEC §8.6.14).
// A subtitle file with no ownership row is read for a provenance marker; when
// the marker's hash matches the body, the file is dir2mcp output nobody has
// changed, so it is treated as owned (in memory now; writeArtifact persists the
// row for the document it belongs to). This is what keeps a lost, reset or
// rebuilt state database from turning every written file into an "authored"
// transcript that would never be re-derived. An unmarked file (authored, or
// legacy output of another tool) costs one small head read per process per
// stat; a marked but edited file fails the hash and stays authored.
func (s *Service) adoptMarkedSidecar(ctx context.Context, relPath string, st sidecarStat) bool {
	s.ownedMu.RLock()
	prev, checked := s.unownedChecked[relPath]
	s.ownedMu.RUnlock()
	if checked && prev == st {
		return false
	}
	data, ok := s.readMarkedSidecar(ctx, relPath)
	if !ok || !subtitle.ReadProvenance(data).Intact {
		s.ownedMu.Lock()
		if s.unownedChecked == nil {
			s.unownedChecked = make(map[string]sidecarStat)
		}
		s.unownedChecked[relPath] = st
		s.ownedMu.Unlock()
		return false
	}
	sum := sha256.Sum256(data)
	s.ownedMu.Lock()
	// DocID 0 marks an adoption not yet persisted; writeArtifact records it.
	// Adoption is a discovery fact, so it goes into the in-corpus index.
	s.ownedIndexLocked("")[relPath] = ownedArtifact{SizeBytes: st.SizeBytes, MTimeUnix: st.MTimeUnix,
		ContentSHA256: hex.EncodeToString(sum[:])}
	s.ownedMu.Unlock()
	return true
}

// readMarkedSidecar returns the whole file only when its head carries the
// marker tag; an unmarked file is never read past ProvenanceHeadBytes.
func (s *Service) readMarkedSidecar(ctx context.Context, relPath string) ([]byte, bool) {
	rc, err := s.corpusFS().Open(ctx, relPath)
	if err != nil {
		return nil, false
	}
	defer func() { _ = rc.Close() }()
	head := make([]byte, subtitle.ProvenanceHeadBytes)
	n, _ := io.ReadFull(rc, head)
	if !subtitle.MayCarryProvenance(head[:n]) {
		return nil, false
	}
	rest, err := io.ReadAll(io.LimitReader(rc, s.sourceReadCapBytes()))
	if err != nil {
		return nil, false
	}
	return append(head[:n], rest...), true
}

// subtitleRenderer builds the shared renderer once per service. A compile
// failure is a configuration problem the loader should have caught; it is
// logged once and disables write-back rather than failing every document.
func (s *Service) subtitleRenderer() (*subexport.Renderer, bool) {
	s.emitRendererOnce.Do(func() {
		r, err := subexport.NewRenderer(s.cfg)
		if err != nil {
			s.getLogger().Printf("subtitle write-back disabled: %v", err)
			return
		}
		s.emitRenderer = &r
	})
	return s.emitRenderer, s.emitRenderer != nil
}

// emitSubtitles is the §8.6.14 entry point, called once per media document
// after ALL of its transcript representations for the run are persisted: at the
// end of single-pass processing (including the unchanged-document path, so
// enabling write-back on an already-indexed corpus fills in the files), and at
// the end of the two-phase derivation pass. The transcription pass writes
// nothing: its translations do not exist yet. Every failure here is per-document
// and non-fatal.
func (s *Service) emitSubtitles(ctx context.Context, doc model.Document) {
	if !s.emitEnabled() || s.activePass == passTranscription {
		return
	}
	if doc.DocID <= 0 || doc.Status != "ok" || !isSidecarMediaType(doc.DocType) {
		return
	}
	ts, okTS := s.store.(subexport.Store)
	as, okAS := s.store.(emittedArtifactStore)
	if !okTS || !okAS {
		s.emitWarnOnce.Do(func() {
			s.getLogger().Printf("subtitle write-back disabled: store %T cannot read transcripts or record artifacts", s.store)
		})
		return
	}
	if !s.emitTargetWritable() {
		return
	}
	renderer, ok := s.subtitleRenderer()
	if !ok {
		return
	}
	reps, err := ts.TranscriptRepresentations(ctx, doc.RelPath)
	if err != nil || len(reps) == 0 {
		return
	}

	plan := emitPlan{
		doc:       doc,
		stem:      s.emitStem(doc.RelPath),
		langs:     s.emitLanguages(reps),
		bound:     s.boundSidecars(ctx, doc.RelPath),
		primary:   s.primaryTranscriptLanguage(reps),
		secondary: s.emitSecondaryLanguage(reps),
	}
	for _, format := range sortedCopy(s.cfg.MediaSubtitlesEmitFormats) {
		if format == "ttml" {
			s.emitTTML(ctx, ts, as, renderer, plan)
			continue
		}
		for _, lang := range plan.langs {
			s.emitTimed(ctx, ts, as, renderer, plan, format, lang)
		}
	}
}

// emitPlan is the per-document input to the per-artifact writers.
type emitPlan struct {
	doc  model.Document
	stem string
	// langs are the transcript languages to write VTT/SRT for, sorted.
	langs []string
	// bound is the set of UNOWNED sidecars already bound to the document, keyed
	// by boundKey(ext, lang); "ttml" is additionally present when any TTML binds.
	bound map[string]struct{}
	// primary/secondary are the TTML languages: the source transcript and the
	// first configured translation target the document has (empty = none).
	primary, secondary string
}

// emitTargetWritable reports whether there is a filesystem to write into: the
// corpus itself for a local/NFS source, or an explicit output root otherwise.
// The loader already rejects an s3 source with no dir; this is the runtime
// guard for a non-local CorpusFS injected another way.
func (s *Service) emitTargetWritable() bool {
	if strings.TrimSpace(s.cfg.MediaSubtitlesEmitDir) != "" {
		return true
	}
	if _, local := s.corpusFS().(*corpusfs.LocalFS); local {
		return true
	}
	s.emitWarnOnce.Do(func() {
		s.getLogger().Printf("subtitle write-back disabled: the corpus source has no writable filesystem; set media.subtitles.emit.dir")
	})
	return false
}

// emitStem returns the filename stem written beside the media: the media's own
// stem, or, when renditions are grouped (§8.6.5), the group stem with rendition
// markers stripped — so one set of files serves every rendition and binds back
// under §8.6.4's normalized-base pass. Case is preserved (the grouping KEY is
// lower-cased, a filename must not be).
func (s *Service) emitStem(relPath string) string {
	base := path.Base(relPath)
	stem := strings.TrimSuffix(base, path.Ext(base))
	if !s.mediaVariantsGrouped() || !isMediaVariantFile(relPath) {
		return stem
	}
	if g := stripRenditionMarkers(stem); g != "" {
		return g
	}
	return stem
}

// stripRenditionMarkers removes every rendition marker from a filename stem,
// preserving case. It is normalizeVariantName's stem rule without the
// lower-casing, because the result names a file rather than keying a group.
func stripRenditionMarkers(stem string) string {
	prev := ""
	for stem != prev {
		prev = stem
		stem = renditionMarkerPattern.ReplaceAllString(stem, "$1")
	}
	return strings.Trim(stem, "._-")
}

// emitLanguages lists the transcript languages to write VTT/SRT for: every
// language the document has a transcript in, restricted by
// media.subtitles.emit.languages when set, sorted for deterministic emission.
// An untagged transcript contributes "" (written in the undifferentiated
// `<stem>.<ext>` shape).
func (s *Service) emitLanguages(reps []store.TranscriptRepresentation) []string {
	allow := map[string]struct{}{}
	for _, l := range s.cfg.MediaSubtitlesEmitLanguages {
		allow[strings.ToLower(strings.TrimSpace(l))] = struct{}{}
	}
	seen := map[string]struct{}{}
	var out []string
	for _, rep := range reps {
		lang := strings.ToLower(subexport.RepLanguage(rep.MetaJSON))
		if _, dup := seen[lang]; dup {
			continue
		}
		if len(allow) > 0 {
			if _, ok := allow[lang]; !ok {
				continue
			}
		}
		seen[lang] = struct{}{}
		out = append(out, lang)
	}
	sort.Strings(out)
	return out
}

// primaryTranscriptLanguage returns the TTML primary language: the document's
// source (non-translation) transcript. When several non-translation transcripts
// exist — an archive whose source AND translated subtitles were both authored
// sidecars carries no translation provenance — the pinned STT language
// (media.stt / SetTranscriptLanguage) decides, so a `ru` corpus with authored
// `.ru.vtt` and `.en.vtt` still packages Russian as primary. Falls back to the
// first representation's language when every transcript is a translation.
func (s *Service) primaryTranscriptLanguage(reps []store.TranscriptRepresentation) string {
	pinned := strings.ToLower(strings.TrimSpace(s.transcriptLanguage))
	first := ""
	for _, rep := range reps {
		if subexport.RepIsTranslation(rep.MetaJSON) {
			continue
		}
		lang := subexport.RepLanguage(rep.MetaJSON)
		if pinned != "" && strings.ToLower(lang) == pinned {
			return lang
		}
		if first == "" {
			first = lang
		}
	}
	if first != "" {
		return first
	}
	if len(reps) > 0 {
		return subexport.RepLanguage(reps[0].MetaJSON)
	}
	return ""
}

// emitSecondaryLanguage returns the TTML secondary language: the first
// configured translation target (media.translate.target_langs, as resolved onto
// the service) the document actually has a transcript for, excluding the
// primary. Empty means monolingual TTML.
func (s *Service) emitSecondaryLanguage(reps []store.TranscriptRepresentation) string {
	primary := strings.ToLower(s.primaryTranscriptLanguage(reps))
	have := map[string]struct{}{}
	for _, rep := range reps {
		have[strings.ToLower(subexport.RepLanguage(rep.MetaJSON))] = struct{}{}
	}
	for _, target := range s.translateTargetLangs {
		t := strings.ToLower(strings.TrimSpace(target))
		if t == "" || t == primary {
			continue
		}
		if _, ok := have[t]; ok {
			return t
		}
	}
	return ""
}

// boundSidecars returns the UNOWNED sidecars currently bound to the media
// document (findSidecars already excludes owned files), keyed by extension and
// language, plus a bare "ttml" key when any TTML binds. Under `if_missing` a
// bound sidecar means the (format, language) is present and is not written.
func (s *Service) boundSidecars(ctx context.Context, relPath string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, sc := range s.findSidecars(ctx, relPath) {
		out[boundKey(sc.Ext, sc.Lang)] = struct{}{}
		if sc.Ext == ".ttml" {
			out["ttml"] = struct{}{}
		}
	}
	return out
}

func boundKey(ext, lang string) string {
	return strings.ToLower(ext) + "|" + strings.ToLower(strings.TrimSpace(lang))
}

// ownedForDoc returns the ownership record of one artifact path of a document
// under the current output root: a row recorded for this document, or a marker
// adoption (DocID 0) not yet tied to a document. It is one map lookup per
// artifact, so the cost of a document does not grow with the corpus. Marker
// adoptions are in the in-corpus index, so they apply only when write-back
// writes beside the media.
func (s *Service) ownedForDoc(ctx context.Context, docID int64, relOut string) (ownedArtifact, bool) {
	rec, ok := s.lookupOwned(ctx, s.emitOutputRoot(), relOut)
	if !ok || (rec.DocID != docID && rec.DocID != 0) {
		return ownedArtifact{}, false
	}
	return rec, true
}

// emitRelPath is the corpus-relative path of an artifact: the media's directory,
// the (group) stem, the §8.6.4 language token when present, and the extension.
func emitRelPath(doc model.Document, stem, lang, ext string) string {
	name := stem
	if lang != "" {
		name += "." + lang
	}
	name += ext
	dir := path.Dir(doc.RelPath)
	if dir == "." || dir == "" {
		return name
	}
	return dir + "/" + name
}

// emitOutputRoot is the normalized media.subtitles.emit.dir recorded on every
// ownership row: "" for beside-the-media, else the cleaned configured path.
func (s *Service) emitOutputRoot() string {
	root := strings.TrimSpace(s.cfg.MediaSubtitlesEmitDir)
	if root == "" {
		return ""
	}
	return filepath.Clean(root)
}

// emitAbsPath maps a corpus-relative artifact path to the filesystem path it is
// written to: inside the corpus root by default, else mirrored under
// media.subtitles.emit.dir.
func (s *Service) emitAbsPath(relOut string) string {
	root := s.emitOutputRoot()
	if root == "" {
		root = s.cfg.RootDir
	}
	return filepath.Join(root, filepath.FromSlash(relOut))
}

// emitTimed writes one VTT/SRT for one language, unless an unowned sidecar of
// that format and language already binds to the document.
func (s *Service) emitTimed(ctx context.Context, ts subexport.Store, as emittedArtifactStore, r *subexport.Renderer, plan emitPlan, format, lang string) {
	ext := "." + format
	if _, present := plan.bound[boundKey(ext, lang)]; present {
		return
	}
	relOut := emitRelPath(plan.doc, plan.stem, lang, ext)
	if s.ownedArtifactKept(ctx, as, plan, relOut, format, lang) {
		return
	}
	rendered, err := r.RenderTimed(ctx, ts, plan.doc.RelPath, lang, format)
	if err != nil {
		if !errors.Is(err, subexport.ErrNoCues) {
			s.getLogger().Printf("subtitle write-back: render %s %s for %s: %v", format, lang, plan.doc.RelPath, err)
		}
		return
	}
	if format == "vtt" {
		// The provenance marker (SPEC §8.6.14) lets this file prove it is dir2mcp
		// output if the state database is ever lost. SRT has no comment syntax.
		rendered = subtitle.StampVTT(rendered)
	}
	s.writeArtifact(ctx, as, plan, relOut, format, lang, []byte(rendered))
}

// emitTTML writes the document's (bilingual) TTML unless any unowned TTML
// already binds to it.
func (s *Service) emitTTML(ctx context.Context, ts subexport.Store, as emittedArtifactStore, r *subexport.Renderer, plan emitPlan) {
	if _, present := plan.bound["ttml"]; present {
		return
	}
	relOut := emitRelPath(plan.doc, plan.stem, "", ".ttml")
	if s.ownedArtifactKept(ctx, as, plan, relOut, "ttml", "") {
		return
	}
	rendered, _, err := r.RenderTTML(ctx, ts, plan.doc.RelPath, plan.primary, plan.secondary)
	if err != nil {
		if !errors.Is(err, subexport.ErrNoCues) {
			s.getLogger().Printf("subtitle write-back: render ttml for %s: %v", plan.doc.RelPath, err)
		}
		return
	}
	s.writeArtifact(ctx, as, plan, relOut, "ttml", "", []byte(subtitle.StampTTML(rendered)))
}

// ownedArtifactKept reports, before any render, that the `if_missing` policy
// keeps an artifact as it is: the file exists and this pipeline owns it. The
// render would only be discarded, so it is skipped. A marker adoption is still
// persisted on the way. Under `refresh`, or when the file is gone or not owned,
// it returns false and writeArtifact decides after the render.
func (s *Service) ownedArtifactKept(ctx context.Context, as emittedArtifactStore, plan emitPlan, relOut, format, lang string) bool {
	if s.cfg.MediaSubtitlesEmitPolicy == "refresh" {
		return false
	}
	s.persistAdoption(ctx, as, plan, relOut, format, lang)
	if _, owned := s.ownedForDoc(ctx, plan.doc.DocID, relOut); !owned {
		return false
	}
	_, err := os.Stat(s.emitAbsPath(relOut))
	return err == nil
}

// writeArtifact applies the overwrite policy and, when a write is due, writes
// the bytes atomically, records ownership, and labels the manifest output. It
// never writes over a file this pipeline did not write.
func (s *Service) writeArtifact(ctx context.Context, as emittedArtifactStore, plan emitPlan, relOut, format, lang string, data []byte) {
	abs := s.emitAbsPath(relOut)
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	s.persistAdoption(ctx, as, plan, relOut, format, lang)
	if !s.artifactWriteDue(ctx, plan, relOut, abs, sha) {
		return
	}
	if err := writeSubtitleFileAtomic(abs, data); err != nil {
		s.getLogger().Printf("subtitle write-back: write %s: %v", relOut, redactPathError(err))
		s.addErrors(1)
		s.markActiveErrored(manifestErrSubtitleWriteFailed, manifestErrSubtitleWriteFailed+": subtitle write-back failed for "+relOut)
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		s.getLogger().Printf("subtitle write-back: stat written %s: %v", relOut, err)
		return
	}
	artifact := store.EmittedArtifact{
		RelPath: relOut, DocID: plan.doc.DocID, OutputRoot: s.emitOutputRoot(), Format: format, Lang: lang,
		SizeBytes: info.Size(), MTimeUnix: info.ModTime().Unix(), ContentSHA256: sha,
		EmittedUnix: time.Now().Unix(),
	}
	if err := as.UpsertEmittedArtifact(ctx, artifact); err != nil {
		// The file is on disk but unrecorded: the next scan would read it as an
		// authored sidecar. Remove it so the run stays consistent and retries.
		_ = os.Remove(abs)
		s.getLogger().Printf("subtitle write-back: record %s: %v (file removed; will retry next run)", relOut, err)
		s.addErrors(1)
		s.markActiveErrored(manifestErrSubtitleWriteFailed, manifestErrSubtitleWriteFailed+": could not record subtitle artifact for "+relOut)
		return
	}
	s.rememberOwned(artifact)
	s.activeOutcome.addOutput(emitOutputLabel(format, lang))
}

// persistAdoption records, for this document, an artifact discovery adopted by
// its provenance marker (DocID 0), so later scans find a row instead of reading
// the file again. The in-memory index is updated to match.
func (s *Service) persistAdoption(ctx context.Context, as emittedArtifactStore, plan emitPlan, relOut, format, lang string) {
	rec, ok := s.ownedForDoc(ctx, plan.doc.DocID, relOut)
	if !ok || rec.DocID != 0 {
		return
	}
	artifact := store.EmittedArtifact{
		RelPath: relOut, DocID: plan.doc.DocID, OutputRoot: s.emitOutputRoot(), Format: format, Lang: lang,
		SizeBytes: rec.SizeBytes, MTimeUnix: rec.MTimeUnix, ContentSHA256: rec.ContentSHA256,
		EmittedUnix: time.Now().Unix(),
	}
	if err := as.UpsertEmittedArtifact(ctx, artifact); err != nil {
		s.getLogger().Printf("subtitle write-back: record adopted %s: %v", relOut, err)
		return
	}
	s.rememberOwned(artifact)
}

// artifactWriteDue is the overwrite-policy decision for one artifact (SPEC
// §8.6.14): it reports whether writeArtifact should put the rendered bytes
// (hash sha) at abs. It never approves writing over a file this pipeline did not
// write, and it keeps the ownership rows consistent with the disk on the way.
func (s *Service) artifactWriteDue(ctx context.Context, plan emitPlan, relOut, abs, sha string) bool {
	rec, owned := s.ownedForDoc(ctx, plan.doc.DocID, relOut)
	_, statErr := os.Stat(abs)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		// Neither present nor absent: a permission or I/O problem. Nothing can be
		// confirmed about the target, so nothing is written over it.
		s.getLogger().Printf("subtitle write-back: cannot stat %s: %v; skipping", relOut, redactPathError(statErr))
		return false
	}
	if owned && !exists {
		// The file we wrote is gone. Its row would otherwise outlive it and, if an
		// authored file later landed on the same name with the same size and
		// mtime, wrongly exclude that file from discovery. Drop the row now; the
		// write records the recreated file afresh.
		s.forgetOwned(ctx, s.emitOutputRoot(), relOut)
		return true
	}
	switch {
	case owned && s.cfg.MediaSubtitlesEmitPolicy != "refresh":
		return false // present and ours; if_missing leaves it
	case owned && rec.ContentSHA256 == sha:
		return false // refresh: nothing changed
	case owned:
		// refresh wants to rewrite; only if the bytes on disk are still the bytes
		// we wrote. An edit that kept size and mtime is authored now: never
		// overwrite it (ownedBytesIntact drops the record).
		return s.ownedBytesIntact(ctx, abs, relOut, rec)
	case exists:
		// A file at the target name that did not bind as a sidecar (excluded by
		// path_excludes, or a shape §8.6.4 refuses). Not ours: never overwrite.
		s.getLogger().Printf("subtitle write-back: %s exists and was not written by dir2mcp; leaving it untouched", relOut)
		return false
	}
	return true
}

// ownedBytesIntact verifies, immediately before a rewrite, that an owned file
// still holds the bytes recorded for it (SPEC §8.6.14). Discovery's stat test
// cannot see an edit that preserved size and mtime; this read can, and it runs
// only here — once per rewrite, never per scan. On a mismatch the file becomes
// authored (record dropped) and false is returned. A read failure is treated the
// same way: when the bytes cannot be confirmed as ours, they are not rewritten.
func (s *Service) ownedBytesIntact(ctx context.Context, abs, relOut string, rec ownedArtifact) bool {
	data, err := os.ReadFile(abs)
	if err != nil {
		s.getLogger().Printf("subtitle write-back: cannot verify %s before rewrite: %v; leaving it untouched", relOut, redactPathError(err))
		return false
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) == rec.ContentSHA256 {
		return true
	}
	s.getLogger().Printf("subtitle write-back: %s was edited since it was written (same size and mtime); it is an authored sidecar from now on and is not rewritten", relOut)
	s.forgetOwned(ctx, s.emitOutputRoot(), relOut)
	return false
}

// emitOutputLabel is the manifest `outputs` tag for an artifact: `<format>:<lang>`,
// or the bare format for a TTML or an untagged transcript.
func emitOutputLabel(format, lang string) string {
	if lang == "" {
		return format
	}
	return format + ":" + lang
}

// writeSubtitleFileAtomic writes data to path via a sibling temporary file and
// rename, so a reader never sees a partial file and a crash mid-write cannot
// truncate an existing one. The directory is created if needed. The file lands
// at emitFileMode (before umask): subtitles are for players, not secrets.
func writeSubtitleFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".dir2mcp-subtitle-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, emitFileMode); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// redactPathError keeps an OS error's kind without echoing the absolute
// filesystem path it carries (the log already names the corpus-relative path).
func redactPathError(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return fmt.Sprintf("%s: %v", pe.Op, pe.Err)
	}
	return err.Error()
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

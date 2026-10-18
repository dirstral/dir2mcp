package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/subexport"
)

// Sampled media identity (SPEC §7.6, `ingest.media_identity: sampled`).
//
// With the default ("full") identity every audio/video document is read whole
// on its first scan, only to hash it, even when its transcript then comes from a
// subtitle sidecar and the bytes are never used. On a video archive that is the
// dominant cost of the first pass: tens of terabytes over NFS to confirm files
// that are not even transcribed. A sampled identity hashes the file's size and
// three fixed slices (head, middle, tail) instead, and the bytes are read in full
// only when a representation actually needs them (STT, direct media embedding).
//
// What it gives up: an in-place edit that rewrites only bytes outside the three
// slices and keeps the size identical is not seen as a change. Transcoded media
// is not edited that way; a corpus where it is keeps the default "full".
//
// A file no larger than the three slices together is read whole and hashed
// exactly as under "full", so small media keeps its historical identity.
//
// SPEC §7.6 (spec 0.81.0) fixes the digest bytes and gives three test vectors;
// tests/ingest pins them (TestSampledIdentity_SpecTestVectors).

// mediaIdentitySampleBytes is the size of each of the three slices.
const mediaIdentitySampleBytes = 4 << 20

// mediaIdentityDomain separates a sampled identity from any full-content hash:
// the two can never collide on the same value.
const mediaIdentityDomain = "dir2mcp-media-identity-v1"

// sampledMediaIdentityActive reports whether the configured media identity is
// "sampled" and the document type is one it applies to (audio/video).
func (s *Service) sampledMediaIdentityActive(docType string) bool {
	return strings.EqualFold(strings.TrimSpace(s.cfg.IngestMediaIdentity), "sampled") && isSidecarMediaType(docType)
}

// mediaIdentityRead is the outcome of reading a media file for its identity.
type mediaIdentityRead struct {
	// identity is the sampled hash; empty when the file was small enough to be
	// read whole (content is then set and hashed as under "full").
	identity string
	// content is the whole file when it was read whole, else nil.
	content []byte
	// head is the first slice (or the whole small file): the secret-scan sample.
	head []byte
	// overCap reports that the file is larger than ingest.max_file_mb.
	overCap bool
}

// readMediaIdentity reads the three slices of a media file (or the whole file
// when it is small) through the corpus filesystem. It applies the same size cap
// as a full read, so a file over ingest.max_file_mb is refused identically.
func (s *Service) readMediaIdentity(ctx context.Context, relPath string) (mediaIdentityRead, error) {
	rc, err := s.corpusFS().Open(ctx, relPath)
	if err != nil {
		if errors.Is(err, corpusfs.ErrObjectTooLarge) {
			return mediaIdentityRead{overCap: true}, nil
		}
		return mediaIdentityRead{}, err
	}
	defer func() { _ = rc.Close() }()

	size, err := rc.Seek(0, io.SeekEnd)
	if err != nil {
		return mediaIdentityRead{}, fmt.Errorf("measure: %w", err)
	}
	if size > s.sourceReadCapBytes() {
		return mediaIdentityRead{overCap: true}, nil
	}
	if size <= 3*mediaIdentitySampleBytes {
		if _, err := rc.Seek(0, io.SeekStart); err != nil {
			return mediaIdentityRead{}, err
		}
		all, err := io.ReadAll(io.LimitReader(rc, size+1))
		if err != nil {
			return mediaIdentityRead{}, err
		}
		return mediaIdentityRead{content: all, head: all}, nil
	}

	h := sha256.New()
	h.Write([]byte(mediaIdentityDomain + "\x00" + strconv.FormatInt(size, 10) + "\x00"))
	var head []byte
	for i, off := range []int64{0, size/2 - mediaIdentitySampleBytes/2, size - mediaIdentitySampleBytes} {
		slice, err := readSlice(rc, off, mediaIdentitySampleBytes)
		if err != nil {
			return mediaIdentityRead{}, fmt.Errorf("read slice at %d: %w", off, err)
		}
		if i == 0 {
			head = slice
		}
		h.Write(slice)
	}
	return mediaIdentityRead{identity: hex.EncodeToString(h.Sum(nil)), head: head}, nil
}

func readSlice(r io.ReadSeeker, off, n int64) ([]byte, error) {
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// sampledContentHash folds a sidecar fingerprint into a sampled identity the
// same way mediaContentHash folds it into full content, so a sidecar change still
// re-processes the media (§7.6).
func sampledContentHash(identity, sidecarFingerprint string) string {
	if sidecarFingerprint == "" {
		return identity
	}
	return computeContentHash([]byte(identity + "\x00" + sidecarFingerprint))
}

// buildSampledMediaDocument is buildDocumentWithContent for an audio/video file
// under the sampled identity. It returns nil content for a large file: the bytes
// are loaded later by ensureMediaContent, only if a representation needs them.
func (s *Service) buildSampledMediaDocument(ctx context.Context, f DiscoveredFile, doc model.Document, secretPatterns []*regexp.Regexp) (model.Document, []byte, error) {
	read, err := s.readMediaIdentity(ctx, f.RelPath)
	if err != nil {
		return doc, nil, fmt.Errorf("read %s: %w", f.RelPath, err)
	}
	if read.overCap {
		return s.sizeCapSkippedDocument(doc, f), nil, nil
	}
	sidecarFP := s.sidecarFingerprint(ctx, f.RelPath, doc.DocType)
	doc.SidecarFingerprint = sidecarFP
	if read.content != nil {
		doc.ContentHash = mediaContentHash(read.content, sidecarFP)
	} else {
		doc.ContentHash = sampledContentHash(read.identity, sidecarFP)
	}
	// The secret scan of a media file only ever looks at a sample of its bytes
	// (secretScanBytes → contentSample), which the head slice covers.
	if hasSecretMatch(secretScanBytes(doc.DocType, read.head), secretPatterns) {
		doc.Status = "secret_excluded"
		doc.SkipReason = model.SkipReasonSecretExcluded
	}
	return doc, read.content, nil
}

// ensureMediaContent returns the document's whole content, reading it now when
// the sampled identity left it unread. It is called only where bytes are needed.
func (s *Service) ensureMediaContent(ctx context.Context, doc model.Document, content []byte) ([]byte, error) {
	if content != nil {
		return content, nil
	}
	full, overCap, err := s.readSourceBytes(ctx, doc.RelPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", doc.RelPath, err)
	}
	if overCap {
		return nil, fmt.Errorf("read %s: %w", doc.RelPath, ErrFileTooLarge)
	}
	return full, nil
}

// derivationNeedsContent reports whether the two-phase derivation pass must read
// the media's bytes to translate it (§8.6.11). Translation applies only to a
// model-derived transcript (§8.6.2), and its result is cached; so the read is
// unnecessary when the document's transcripts are all sidecar-sourced (nothing
// to translate), or when every configured target language already has a
// translation by the active translator and the derivation identity (§8.6.7) is
// current. Any doubt (a multi-track selection, a store without the read surface,
// a store error) answers true and takes the full path, so the output set never
// changes.
func (s *Service) derivationNeedsContent(ctx context.Context, doc model.Document) bool {
	sel, err := config.ParseSTTTracks(s.cfg.MediaSTTTracks)
	if err != nil || sel.Mode != config.STTTracksFirst {
		return true
	}
	ts, ok := s.store.(subexport.Store)
	if !ok {
		return true
	}
	reps, err := ts.TranscriptRepresentations(ctx, doc.RelPath)
	if err != nil {
		return true
	}
	// have holds only the target languages of translations made by the active
	// translator. An authored sidecar in a target language does not count:
	// single-pass translates a model-derived transcript into every target, so
	// the derivation pass must too. A translation by another provider or model
	// does not count either: its cache key differs, so it is re-derived.
	have := make(map[string]struct{}, len(reps))
	modelSource := false
	for _, rep := range reps {
		if subexport.RepIsTranslation(rep.MetaJSON) {
			if s.translationByActiveTranslator(rep.MetaJSON) {
				have[strings.ToLower(subexport.RepLanguage(rep.MetaJSON))] = struct{}{}
			}
			continue
		}
		if !subexport.RepIsSidecar(rep.MetaJSON) {
			modelSource = true
		}
	}
	if !modelSource {
		return false
	}
	if s.derivationIdentityStale(ctx, doc.RelPath) {
		return true
	}
	for _, target := range s.translateTargetLangs {
		if _, ok := have[strings.ToLower(target)]; !ok {
			return true
		}
	}
	return false
}

// translationByActiveTranslator reports whether a translated transcript's
// meta_json records the active translate provider and model (§8.6.7). An
// unparseable meta, or a different provider or model, answers false, so the
// derivation pass reads the bytes and re-derives the translation.
func (s *Service) translationByActiveTranslator(metaJSON string) bool {
	var meta transcriptMeta
	if err := json.Unmarshal([]byte(strings.TrimSpace(metaJSON)), &meta); err != nil {
		return false
	}
	return strings.TrimSpace(meta.TranslateProvider) == strings.TrimSpace(s.translateProvider) &&
		strings.TrimSpace(meta.TranslateModel) == strings.TrimSpace(s.translateModel)
}

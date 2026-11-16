package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// SMIL caption patch (SPEC §8.6.14, `media.subtitles.emit.smil`).
//
// After write-back has put the subtitles beside a video, the video's existing
// SMIL (`<stem>.smil`, written by whatever produced the renditions) gets caption
// entries pointing at them, so a streaming server shows the captions. The rules
// are the ones the retired livevtt archive_transcriber followed:
//
//   - a SMIL is never created; a missing or malformed one is reported
//     (SMIL_INVALID) and left alone;
//   - only the caption entries this pipeline manages for the video are replaced:
//     `<stem>.<lang>.vtt`, `<stem>.ttml` and `<stem>_<N>p.ttml`; any other
//     <textstream> is kept;
//   - entries reference only caption files that exist; when none exists (yet,
//     or because a write failed), managed entries whose file is present are
//     kept and only those pointing at a missing file are removed;
//   - before a change the SMIL is copied to `<smil>.bak.<YYYYmmdd_HHMMSS>`, then
//     replaced atomically with its file mode kept.
//
// Unlike livevtt, the edit splices only the caption lines (every other byte is
// kept) and a SMIL whose caption entries are already right is not touched.

// Non-fatal per-document manifest codes (§14.4).
const (
	manifestErrSMILInvalid     = "SMIL_INVALID"
	manifestErrSMILPatchFailed = "SMIL_PATCH_FAILED"
)

// smilBackupTimeLayout is the timestamp of a SMIL backup (`<smil>.bak.<ts>`),
// the same shape livevtt used, so existing backup exclusions keep matching.
const smilBackupTimeLayout = "20060102_150405"

// smilTarget is the media's SMIL as read once per document.
type smilTarget struct {
	relPath string
	abs     string
	data    []byte
	mode    fs.FileMode
	// err is why the SMIL cannot be used (missing, unreadable, malformed).
	err error
	// topStem is the top rendition's stem (e.g. "abc_1080p"), when valid.
	topStem string
}

func (s *Service) smilPatchMode() string {
	return strings.ToLower(strings.TrimSpace(s.cfg.MediaSubtitlesEmitSMIL))
}

// planSMIL reads the SMIL when the patch or the top-rendition TTML name needs it
// and sets plan.ttmlStem.
func (s *Service) planSMIL(plan *emitPlan) {
	plan.ttmlStem = plan.stem
	wantName := strings.EqualFold(s.cfg.MediaSubtitlesEmitTTMLName, "top_rendition")
	mode := s.smilPatchMode()
	if !wantName && (mode == "" || mode == "off") {
		return
	}
	plan.smil = s.readSMIL(plan)
	if wantName {
		if plan.smil.topStem != "" {
			plan.ttmlStem = plan.smil.topStem
		} else {
			// No usable SMIL: name after the media itself (its own rendition).
			base := path.Base(plan.doc.RelPath)
			plan.ttmlStem = strings.TrimSuffix(base, path.Ext(base))
		}
	}
}

func (s *Service) readSMIL(plan *emitPlan) smilTarget {
	rel := emitRelPath(plan.doc, plan.stem, "", ".smil")
	t := smilTarget{relPath: rel, abs: filepath.Join(s.cfg.RootDir, filepath.FromSlash(rel))}
	info, err := os.Stat(t.abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			t.err = errors.New("smil_missing")
		} else {
			t.err = fmt.Errorf("smil_unreadable: %s", redactPathError(err))
		}
		return t
	}
	t.mode = info.Mode().Perm()
	if t.data, err = os.ReadFile(t.abs); err != nil {
		t.err = fmt.Errorf("smil_unreadable: %s", redactPathError(err))
		return t
	}
	sinfo, err := subtitle.InspectSMIL(t.data)
	if err != nil {
		t.err = err
		return t
	}
	t.topStem = subtitle.SMILSrcStem(sinfo.TopVideoSrc)
	return t
}

// patchSMIL is the §8.6.14 caption patch for one document. Every outcome is
// per-document and non-fatal.
func (s *Service) patchSMIL(plan emitPlan) {
	mode := s.smilPatchMode()
	if mode == "" || mode == "off" {
		return
	}
	if plan.smil.err != nil {
		s.getLogger().Printf("subtitle write-back: SMIL %s not patched: %v", plan.smil.relPath, plan.smil.err)
		s.markActiveErrored(manifestErrSMILInvalid, manifestErrSMILInvalid+": "+plan.smil.relPath+": "+plan.smil.err.Error())
		return
	}
	want := s.smilWantedTextStreams(plan, mode)
	managed := managedCaptionMatcher(plan)
	// An entry that already references a caption file dir2mcp did not write (for
	// example livevtt's TTML) is that file's author's: it is kept exactly as it
	// is, never rewritten or removed (SPEC §8.6.14). Recomputing its languages
	// from dir2mcp's transcripts narrowed thousands of livevtt entries whose own
	// VTTs were empty, dropping caption tracks that had text.
	want, kept := s.keepForeignEntries(plan, want, managed)
	if len(kept) > 0 {
		isManaged := managed
		managed = func(src string) bool {
			return isManaged(src) && !kept[smilEntryName(src)]
		}
	}
	if len(want) == 0 {
		// Nothing to link: never drop a working entry, only dangling ones.
		dir := filepath.Dir(plan.smil.abs)
		isManaged := managed
		managed = func(src string) bool {
			return isManaged(src) && !smilSiblingExists(dir, subtitle.NormalizeSMILSrc(src))
		}
	}
	out, changed, err := subtitle.PatchSMILTextStreams(plan.smil.data, managed, want)
	if err != nil {
		s.markActiveErrored(manifestErrSMILInvalid, manifestErrSMILInvalid+": "+plan.smil.relPath+": "+err.Error())
		return
	}
	if !changed {
		return
	}
	if err := replaceSMIL(plan.smil, out, s.smilBeforeCommit); err != nil {
		if errors.Is(err, errSMILChanged) {
			// Another tool rewrote the SMIL after it was read. Leave its version
			// alone; the next scan patches the new content.
			s.getLogger().Printf("subtitle write-back: SMIL %s changed during write-back; not patched this scan", plan.smil.relPath)
			return
		}
		s.getLogger().Printf("subtitle write-back: patch SMIL %s: %v", plan.smil.relPath, redactPathError(err))
		s.addErrors(1)
		s.markActiveErrored(manifestErrSMILPatchFailed, manifestErrSMILPatchFailed+": "+plan.smil.relPath)
		return
	}
	// The patch invalidates the SMIL's cached change signal (§8.6.14, §7.8): a
	// same-length patch on a coarse-mtime filesystem can keep its (size,
	// mtime). Nothing needs to be cleared here, because the (size, mtime) skip
	// (mediaStatUnchanged) covers audio and video only: the next scan reads the
	// SMIL and confirms it by content_hash. If that skip ever widens to text,
	// the SMIL's stored signal must be cleared here.
	// TestSMILPatch_SameSizeSameMtimePatchIsIndexedAgain pins this.
	s.activeOutcome.addOutput("smil")
}

// smilWantedTextStreams lists the caption entries the SMIL should carry: one for
// the TTML (mode ttml) or one per VTT (mode vtt), for files that exist.
func (s *Service) smilWantedTextStreams(plan emitPlan, mode string) []subtitle.TextStream {
	params := s.smilParams()
	dir := filepath.Dir(plan.smil.abs)
	exists := func(name string) bool { return smilSiblingExists(dir, name) }
	var want []subtitle.TextStream
	switch mode {
	case "ttml":
		name := plan.ttmlStem + ".ttml"
		if !exists(name) {
			return nil
		}
		want = append(want, subtitle.TextStream{Src: name, Lang: s.smilTTMLLanguages(plan, filepath.Join(dir, name)), Params: params})
	case "vtt":
		for _, lang := range plan.langs {
			name := plan.stem + "." + lang + ".vtt"
			if lang == "" || !exists(name) {
				continue
			}
			want = append(want, subtitle.TextStream{Src: name, Lang: s.smilLanguageCode(lang), Params: params})
		}
	}
	return want
}

func (s *Service) smilParams() []subtitle.SMILParam {
	var out []subtitle.SMILParam
	for _, p := range s.cfg.MediaSubtitlesEmitSMILParams {
		name, value, _ := strings.Cut(p, "=")
		out = append(out, subtitle.SMILParam{Name: strings.TrimSpace(name), Value: strings.TrimSpace(value)})
	}
	return out
}

// smilLanguageCode renders a language tag for system-language: unchanged under
// "bcp47", or the ISO 639-2 three-letter code ("ru" → "rus") under "iso639_2".
func (s *Service) smilLanguageCode(tag string) string {
	return subtitle.LanguageCode(strings.TrimSpace(tag), s.cfg.MediaSubtitlesEmitSMILLanguageCodes)
}

// smilSiblingExists reports whether name, relative to the SMIL's folder, is an
// existing regular file.
func smilSiblingExists(dir, name string) bool {
	info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
	return err == nil && info.Mode().IsRegular()
}

// managedCaptionMatcher reports whether a SMIL <textstream> src is a caption
// entry this pipeline manages for the document: `<stem>.<lang>.vtt`,
// `<stem>.ttml`, `<stem>_<N>p.ttml`, or the TTML name in use (SPEC §8.6.14).
// The match is exact: another separator (`<stem>-720p.ttml`) or another case
// names a file this pipeline did not write, so that entry is foreign and kept.
// Only a bare file name beside the SMIL qualifies: "other/abc.ttml" is foreign.
func managedCaptionMatcher(plan emitPlan) func(string) bool {
	stem := regexp.QuoteMeta(plan.stem)
	re := regexp.MustCompile(`^(?:` + stem + `\.[^./\\]+\.vtt|` + stem + `(?:_\d+p)?\.ttml|` +
		regexp.QuoteMeta(plan.ttmlStem) + `\.ttml)$`)
	return func(src string) bool {
		return re.MatchString(strings.TrimPrefix(path.Clean(subtitle.NormalizeSMILSrc(src)), "./"))
	}
}

// errSMILChanged reports that the SMIL on disk no longer holds the bytes the
// patch was computed from.
var errSMILChanged = errors.New("smil changed since it was read")

// replaceSMIL backs the SMIL up (livevtt's `<smil>.bak.<ts>` shape) and replaces
// it atomically with data, keeping its file mode. Every change gets its own
// backup; one that cannot be made aborts the change.
//
// The order keeps the check close to the rename. The new bytes go to a temp
// file beside the SMIL first, and get the SMIL's mode. Then the SMIL is read
// once more: when another tool rewrote it after planSMIL read it, the patch
// would discard that rewrite and the backup would hold the older bytes, so
// replaceSMIL returns errSMILChanged and writes nothing. Only after that check
// does it write the backup and rename the temp file over the SMIL. On any
// error or mismatch the temp file is removed.
//
// A race remains: a write by another tool between the final check and the
// rename is lost (the backup holds the bytes that were read, not that write).
// Only a lock that the other writer also honours can close that window;
// replaceSMIL takes none.
//
// beforeCommit, when not nil, runs just before the final check (a test seam).
func replaceSMIL(t smilTarget, data []byte, beforeCommit func(abs string)) error {
	tmp, err := os.CreateTemp(filepath.Dir(t.abs), ".dir2mcp-smil-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, t.mode); err != nil {
		return err
	}
	if beforeCommit != nil {
		beforeCommit(t.abs)
	}
	current, err := os.ReadFile(t.abs)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, t.data) {
		return errSMILChanged
	}
	if err := backupSMIL(t); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if err := os.Rename(tmpName, t.abs); err != nil {
		return err
	}
	committed = true
	return nil
}

// backupSMIL writes the SMIL's current bytes to a new `<smil>.bak.<ts>` file,
// adding a `.<n>` suffix when that name is taken (two changes in one second),
// so an existing backup is never reused or overwritten.
func backupSMIL(t smilTarget) error {
	base := t.abs + ".bak." + time.Now().Format(smilBackupTimeLayout)
	for n := 0; n < 100; n++ {
		name := base
		if n > 0 {
			name = fmt.Sprintf("%s.%d", base, n)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, t.mode)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := f.Write(t.data); err != nil {
			_ = f.Close()
			_ = os.Remove(name)
			return err
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(name)
			return err
		}
		return nil
	}
	return errors.New("no free backup name")
}

// smilEntryName is a <textstream> src as a bare name beside the SMIL.
func smilEntryName(src string) string {
	return strings.TrimPrefix(path.Clean(subtitle.NormalizeSMILSrc(src)), "./")
}

// keepForeignEntries finds every caption entry the SMIL references whose file
// exists and was not written by dir2mcp, and returns those names: the existing
// entries stay as their author left them, also one under a name the patch would not write
// (e.g. abc.ttml when ttml_name selects abc_1080p.ttml). A wanted entry for
// such a name is dropped. A foreign file the SMIL does not reference yet keeps
// its wanted entry, so it is linked once. An entry whose file is missing is no
// one's captions: the patch still replaces or removes it.
func (s *Service) keepForeignEntries(plan emitPlan, want []subtitle.TextStream, managed func(string) bool) ([]subtitle.TextStream, map[string]bool) {
	srcs, err := subtitle.SMILTextStreamSources(plan.smil.data)
	if err != nil {
		return want, nil
	}
	smilDir := path.Dir(plan.smil.relPath)
	fileDir := filepath.Dir(plan.smil.abs)
	var kept map[string]bool
	for _, src := range srcs {
		name := smilEntryName(src)
		if !managed(src) || kept[name] || !smilSiblingExists(fileDir, subtitle.NormalizeSMILSrc(src)) ||
			s.smilEntryOwned(plan, path.Join(smilDir, name)) {
			continue
		}
		if kept == nil {
			kept = map[string]bool{}
		}
		kept[name] = true
	}
	if len(kept) == 0 {
		return want, nil
	}
	out := want[:0:0]
	for _, ts := range want {
		if !kept[smilEntryName(ts.Src)] {
			out = append(out, ts)
		}
	}
	return out, kept
}

// smilEntryOwned reports whether write-back wrote the caption file relPath for
// this document (an ownership record, §8.6.14).
func (s *Service) smilEntryOwned(plan emitPlan, relPath string) bool {
	_, owned := s.ownedForDoc(context.Background(), plan.doc.DocID, relPath)
	return owned
}

// smilTTMLLanguages is the system-language of a TTML entry: the languages of
// the file's own caption text (the xml:lang in effect for it, inherited from
// the document root when the blocks carry none), in the SMIL's code form,
// primary first. A file with no language tags falls back to the document's
// transcript languages.
func (s *Service) smilTTMLLanguages(plan emitPlan, abs string) string {
	var langs []string
	if data, err := os.ReadFile(abs); err == nil {
		for _, l := range subtitle.TTMLTextLanguages(string(data)) {
			langs = append(langs, s.smilLanguageCode(l))
		}
	}
	if len(langs) == 0 {
		for _, l := range []string{plan.primary, plan.secondary} {
			if strings.TrimSpace(l) != "" {
				langs = append(langs, s.smilLanguageCode(l))
			}
		}
	}
	primary := s.smilLanguageCode(plan.primary)
	sort.SliceStable(langs, func(i, j int) bool { return langs[i] == primary && langs[j] != primary })
	return strings.Join(dedupeStrings(langs), ",")
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0:0]
	for _, v := range in {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

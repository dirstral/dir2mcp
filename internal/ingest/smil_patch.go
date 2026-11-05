package ingest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/text/language"

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
//   - entries reference only caption files that exist;
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
	if len(want) == 0 {
		return // no caption file to reference yet
	}
	out, changed, err := subtitle.PatchSMILTextStreams(plan.smil.data, managedCaptionMatcher(plan), want)
	if err != nil {
		s.markActiveErrored(manifestErrSMILInvalid, manifestErrSMILInvalid+": "+plan.smil.relPath+": "+err.Error())
		return
	}
	if !changed {
		return
	}
	if err := replaceSMIL(plan.smil, out); err != nil {
		s.getLogger().Printf("subtitle write-back: patch SMIL %s: %v", plan.smil.relPath, redactPathError(err))
		s.addErrors(1)
		s.markActiveErrored(manifestErrSMILPatchFailed, manifestErrSMILPatchFailed+": "+plan.smil.relPath)
		return
	}
	s.activeOutcome.addOutput("smil")
}

// smilWantedTextStreams lists the caption entries the SMIL should carry: one for
// the TTML (mode ttml) or one per VTT (mode vtt), for files that exist.
func (s *Service) smilWantedTextStreams(plan emitPlan, mode string) []subtitle.TextStream {
	params := s.smilParams()
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(filepath.Dir(plan.smil.abs), name))
		return err == nil
	}
	var want []subtitle.TextStream
	switch mode {
	case "ttml":
		name := plan.ttmlStem + ".ttml"
		if !exists(name) {
			return nil
		}
		langs := []string{}
		for _, l := range []string{plan.primary, plan.secondary} {
			if strings.TrimSpace(l) != "" {
				langs = append(langs, s.smilLanguageCode(l))
			}
		}
		want = append(want, subtitle.TextStream{Src: name, Lang: strings.Join(langs, ","), Params: params})
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
	tag = strings.TrimSpace(tag)
	if !strings.EqualFold(s.cfg.MediaSubtitlesEmitSMILLanguageCodes, "iso639_2") {
		return tag
	}
	base, err := language.ParseBase(strings.SplitN(tag, "-", 2)[0])
	if err != nil {
		return tag
	}
	return base.ISO3()
}

// managedCaptionMatcher reports whether a SMIL <textstream> src is a caption
// entry this pipeline manages for the document: its VTTs, its group-named TTML,
// a rendition-named TTML of the same group, or the TTML name in use.
func managedCaptionMatcher(plan emitPlan) func(string) bool {
	stem := regexp.QuoteMeta(plan.stem)
	re := regexp.MustCompile(`(?i)^(?:` + stem + `\.[^./\\]+\.vtt|` + stem + `(?:[_.-]\d{3,4}p)?\.ttml|` +
		regexp.QuoteMeta(plan.ttmlStem) + `\.ttml)$`)
	return func(src string) bool {
		return re.MatchString(path.Base(subtitle.NormalizeSMILSrc(src)))
	}
}

// replaceSMIL backs the SMIL up (livevtt's `<smil>.bak.<ts>` shape) and replaces
// it atomically with data, keeping its file mode. A backup that cannot be made
// aborts the change.
func replaceSMIL(t smilTarget, data []byte) error {
	backup := t.abs + ".bak." + time.Now().Format(smilBackupTimeLayout)
	if _, err := os.Stat(backup); errors.Is(err, fs.ErrNotExist) {
		if err := os.WriteFile(backup, t.data, t.mode); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(t.abs), ".dir2mcp-smil-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, t.mode); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, t.abs); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

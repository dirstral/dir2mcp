package subtitle

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
)

// SMIL caption patch (SPEC §8.6.14, `media.subtitles.emit.smil`).
//
// A multi-bitrate SMIL beside archive media is owned by whatever produced the
// media (a transcoder): it lists every rendition so a streaming server can switch
// quality. Captions are attached to it by adding <textstream> children to its
// <switch>. This file updates an EXISTING SMIL that way and nothing more:
//
//   - it never creates a SMIL and never touches <video> or any other element;
//   - it replaces only the caption entries it manages (the caller says which
//     sources those are) and keeps every other <textstream>;
//   - it splices bytes: everything outside the replaced <textstream> elements is
//     left byte-for-byte as it was (no re-serialization of the document);
//   - it reports no change when the managed entries already match, so a correct
//     SMIL is never rewritten.

// TextStream is one caption entry to put in a SMIL <switch>.
type TextStream struct {
	// Src is the caption file name, relative to the SMIL (no "mp4:" prefix).
	Src string
	// Lang is the system-language attribute value (e.g. "rus,eng"); empty omits it.
	Lang string
	// Params are <param name=… value=… valuetype="data"/> children, in order.
	Params []SMILParam
}

// SMILParam is one <param> child of a <textstream>.
type SMILParam struct {
	Name, Value string
}

// Reasons a SMIL is not patchable. They are stable strings a caller can report.
var (
	ErrSMILUnparseable = errors.New("smil_unparseable")
	ErrSMILNoSwitch    = errors.New("smil_no_switch")
	ErrSMILNoVideo     = errors.New("smil_no_video_nodes")
)

// SMILInfo is what InspectSMIL learns about a SMIL.
type SMILInfo struct {
	// TopVideoSrc is the src (without an "mp4:" prefix) of the <video> with the
	// greatest height attribute, or the first <video> when none carries a height.
	TopVideoSrc string
}

// smilSwitch is the parse of a SMIL's body/switch.
type smilSwitch struct {
	// start/end are the byte offsets of the <switch> start tag and of its end tag.
	start, end int64
	videos     []smilVideo
	streams    []smilStream
	// childIndent is the indentation of the switch's first child line, reused for
	// inserted entries so they line up with the existing ones.
	childIndent string
}

type smilVideo struct {
	src    string
	height int
}

type smilStream struct {
	// start/end bound the whole <textstream>…</textstream> (or self-closing) element.
	start, end int64
	ts         TextStream
}

// InspectSMIL validates that doc is a SMIL whose body has a <switch> holding at
// least one <video>, and reports its top rendition.
func InspectSMIL(doc []byte) (SMILInfo, error) {
	sw, err := parseSMILSwitch(doc)
	if err != nil {
		return SMILInfo{}, err
	}
	top := sw.videos[0]
	for _, v := range sw.videos[1:] {
		if v.height > top.height {
			top = v
		}
	}
	return SMILInfo{TopVideoSrc: top.src}, nil
}

// PatchSMILTextStreams makes the SMIL's managed caption entries equal to want.
// managed reports whether a <textstream> src (without "mp4:") is one this caller
// manages; those are removed and want is inserted before </switch>. Entries the
// caller does not manage are kept. When the managed entries already equal want
// (as a set), it returns the input unchanged and changed=false.
func PatchSMILTextStreams(doc []byte, managed func(src string) bool, want []TextStream) (out []byte, changed bool, err error) {
	sw, err := parseSMILSwitch(doc)
	if err != nil {
		return doc, false, err
	}
	var current []TextStream
	var drop []smilStream
	for _, st := range sw.streams {
		if managed(st.ts.Src) {
			current = append(current, st.ts)
			drop = append(drop, st)
		}
	}
	if sameTextStreams(current, want) {
		return doc, false, nil
	}

	var b bytes.Buffer
	pos := int64(0)
	for _, st := range drop {
		// Remove the element together with the indentation before it and the line
		// break after it, so no blank line is left behind.
		cut := lineStart(doc, st.start, pos)
		b.Write(doc[pos:cut])
		pos = skipLineEnd(doc, st.end)
	}
	// Insert before the line holding </switch>, indented like the other children.
	insertAt := lineStart(doc, sw.end, pos)
	b.Write(doc[pos:insertAt])
	if insertAt == sw.end && insertAt > 0 && doc[insertAt-1] != '\n' {
		b.WriteByte('\n')
	}
	for _, ts := range want {
		writeTextStream(&b, ts, sw.childIndent)
	}
	b.Write(doc[insertAt:])
	return b.Bytes(), true, nil
}

// lineStart returns the offset where the indentation before off begins, but
// only when that indentation runs back to a line break (or to floor); otherwise
// it returns off, so text on the same line is never consumed.
func lineStart(doc []byte, off, floor int64) int64 {
	i := off
	for i > floor && (doc[i-1] == ' ' || doc[i-1] == '\t') {
		i--
	}
	if i == floor || doc[i-1] == '\n' {
		return i
	}
	return off
}

// skipLineEnd returns the offset just past a line break that follows off
// (allowing trailing spaces), or off when other text follows on the line.
func skipLineEnd(doc []byte, off int64) int64 {
	i := off
	for i < int64(len(doc)) && (doc[i] == ' ' || doc[i] == '\t' || doc[i] == '\r') {
		i++
	}
	if i < int64(len(doc)) && doc[i] == '\n' {
		return i + 1
	}
	return off
}

func writeTextStream(b *bytes.Buffer, ts TextStream, indent string) {
	b.WriteString(indent)
	b.WriteString(`<textstream src="`)
	b.WriteString(escapeAttr(ts.Src))
	b.WriteString(`"`)
	if ts.Lang != "" {
		b.WriteString(` system-language="`)
		b.WriteString(escapeAttr(ts.Lang))
		b.WriteString(`"`)
	}
	if len(ts.Params) == 0 {
		b.WriteString(" />\n")
		return
	}
	b.WriteString(">\n")
	for _, p := range ts.Params {
		b.WriteString(indent)
		b.WriteString(`  <param name="`)
		b.WriteString(escapeAttr(p.Name))
		b.WriteString(`" value="`)
		b.WriteString(escapeAttr(p.Value))
		b.WriteString("\" valuetype=\"data\" />\n")
	}
	b.WriteString(indent)
	b.WriteString("</textstream>\n")
}

// sameTextStreams compares two caption entry lists as sets (order-insensitive).
func sameTextStreams(a, b []TextStream) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(ts TextStream) string {
		ps := make([]string, 0, len(ts.Params))
		for _, p := range ts.Params {
			ps = append(ps, p.Name+"="+p.Value)
		}
		sort.Strings(ps)
		return ts.Src + "\x00" + ts.Lang + "\x00" + strings.Join(ps, "\x00")
	}
	ka := make([]string, 0, len(a))
	kb := make([]string, 0, len(b))
	for i := range a {
		ka = append(ka, key(a[i]))
		kb = append(kb, key(b[i]))
	}
	sort.Strings(ka)
	sort.Strings(kb)
	for i := range ka {
		if ka[i] != kb[i] {
			return false
		}
	}
	return true
}

// NormalizeSMILSrc strips surrounding space and a leading "mp4:" (any case),
// the form a streaming server's SMIL uses for media references.
func NormalizeSMILSrc(src string) string {
	src = strings.TrimSpace(src)
	if len(src) >= 4 && strings.EqualFold(src[:4], "mp4:") {
		src = src[4:]
	}
	return src
}

// SMILSrcStem returns a src's file name without directory or extension.
func SMILSrcStem(src string) string {
	base := path.Base(NormalizeSMILSrc(src))
	return strings.TrimSuffix(base, path.Ext(base))
}

// parseSMILSwitch walks the SMIL once and records the body/switch it patches.
func parseSMILSwitch(doc []byte) (smilSwitch, error) {
	dec := xml.NewDecoder(bytes.NewReader(doc))
	dec.Strict = true
	p := &smilParser{doc: doc, depthSw: -1}
	for {
		off := dec.InputOffset()
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return smilSwitch{}, fmt.Errorf("%w: %v", ErrSMILUnparseable, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			p.onStart(t, off)
		case xml.EndElement:
			p.onEnd(t, off, dec.InputOffset())
		}
	}
	return p.finish()
}

// smilParser is the state of one parseSMILSwitch walk. depthSw is -1 before the
// body/switch is seen, its stack depth while inside it, and -2 once it closed.
type smilParser struct {
	doc     []byte
	sw      smilSwitch
	stack   []string
	cur     *smilStream
	depthSw int
}

func (p *smilParser) onStart(t xml.StartElement, off int64) {
	p.stack = append(p.stack, t.Name.Local)
	depth := len(p.stack)
	switch {
	case p.depthSw == -1 && pathIs(p.stack, "smil", "body", "switch"):
		p.sw.start, p.depthSw = off, depth
	case p.depthSw > 0 && depth == p.depthSw+1:
		if p.sw.childIndent == "" {
			p.sw.childIndent = indentBefore(p.doc, off)
		}
		recordSwitchChild(&p.sw, t, off, &p.cur)
	case p.cur != nil && p.depthSw > 0 && depth == p.depthSw+2 && t.Name.Local == "param":
		p.cur.ts.Params = append(p.cur.ts.Params, SMILParam{Name: attr(t, "name"), Value: attr(t, "value")})
	}
}

func (p *smilParser) onEnd(t xml.EndElement, off, after int64) {
	depth := len(p.stack)
	if p.cur != nil && p.depthSw > 0 && depth == p.depthSw+1 && t.Name.Local == "textstream" {
		p.cur.end = after
		p.sw.streams = append(p.sw.streams, *p.cur)
		p.cur = nil
	}
	if p.depthSw > 0 && depth == p.depthSw {
		p.sw.end = off
		p.depthSw = -2 // done with the first body/switch
	}
	p.stack = p.stack[:depth-1]
}

func (p *smilParser) finish() (smilSwitch, error) {
	if p.depthSw != -2 {
		return smilSwitch{}, ErrSMILNoSwitch
	}
	if len(p.sw.videos) == 0 {
		return smilSwitch{}, ErrSMILNoVideo
	}
	if p.sw.childIndent == "" {
		p.sw.childIndent = indentBefore(p.doc, p.sw.end) + "  "
	}
	return p.sw, nil
}

func recordSwitchChild(sw *smilSwitch, t xml.StartElement, off int64, cur **smilStream) {
	switch t.Name.Local {
	case "video":
		h, _ := strconv.Atoi(strings.TrimSpace(attr(t, "height")))
		sw.videos = append(sw.videos, smilVideo{src: NormalizeSMILSrc(attr(t, "src")), height: h})
	case "textstream":
		lang := attr(t, "system-language")
		if lang == "" {
			lang = attr(t, "systemLanguage")
		}
		*cur = &smilStream{start: off, ts: TextStream{Src: NormalizeSMILSrc(attr(t, "src")), Lang: strings.TrimSpace(lang)}}
	}
}

func pathIs(stack []string, want ...string) bool {
	if len(stack) != len(want) {
		return false
	}
	for i := range want {
		if stack[i] != want[i] {
			return false
		}
	}
	return true
}

func attr(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// indentBefore returns the run of spaces/tabs immediately before off when that
// run starts a line, else "".
func indentBefore(doc []byte, off int64) string {
	i := off
	for i > 0 && (doc[i-1] == ' ' || doc[i-1] == '\t') {
		i--
	}
	if i > 0 && doc[i-1] != '\n' {
		return ""
	}
	return string(doc[i:off])
}

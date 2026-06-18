package subtitle

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/dirstral/dir2mcp/internal/avutil"
)

// SMILSubtitleRef names a companion subtitle document referenced by a SMIL
// packaging file: the relative Src (e.g. "clip.ttml") and its Lang (BCP-47),
// used for the <textstream> systemLanguage attribute.
type SMILSubtitleRef struct {
	Src  string
	Lang string
}

// SMILInput is the data a SMIL packaging document is rendered from (SPEC
// §8.6.10): the media reference plus probed track metadata and the companion
// subtitle document(s). MediaSrc is the relative media reference; Info carries
// the ffprobe-derived metadata (any field may be zero — see fail-open below).
type SMILInput struct {
	MediaSrc  string
	Info      avutil.MediaInfo
	Subtitles []SMILSubtitleRef
}

// RenderSMIL serializes a SMIL packaging document describing the media
// presentation: the media reference plus probed container/codec/bitrate and
// (for video) width/height, and references to the companion subtitle
// document(s) (SPEC §8.6.10). Unknown metadata fields are simply omitted from
// the output (a zero width/height omits the region size, an empty codec omits
// the param) — this is the fail-open contract: a partial probe still yields a
// valid SMIL, and a caller that could not probe at all should omit SMIL
// entirely rather than call this with an empty MediaSrc.
//
// Rendering is deterministic for a given input.
func RenderSMIL(in SMILInput) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<smil xmlns="http://www.w3.org/ns/SMIL" version="3.0">` + "\n")

	b.WriteString("  <head>\n")
	writeMeta(&b, "container", in.Info.Container)
	writeMeta(&b, "videoCodec", in.Info.VideoCodec)
	writeMeta(&b, "audioCodec", in.Info.AudioCodec)
	if in.Info.BitRateBPS > 0 {
		writeMeta(&b, "bitrate", strconv.Itoa(in.Info.BitRateBPS))
	}
	if in.Info.HasVideo() {
		fmt.Fprintf(&b, "    <layout>\n      <root-layout width=%q height=%q/>\n    </layout>\n",
			strconv.Itoa(in.Info.Width), strconv.Itoa(in.Info.Height))
	}
	b.WriteString("  </head>\n")

	b.WriteString("  <body>\n")
	b.WriteString("    <par>\n")
	if in.Info.HasVideo() {
		fmt.Fprintf(&b, "      <video src=%q width=%q height=%q/>\n",
			escapeAttr(in.MediaSrc), strconv.Itoa(in.Info.Width), strconv.Itoa(in.Info.Height))
	} else {
		fmt.Fprintf(&b, "      <audio src=%q/>\n", escapeAttr(in.MediaSrc))
	}
	for _, sub := range in.Subtitles {
		src := strings.TrimSpace(sub.Src)
		if src == "" {
			continue
		}
		if lang := strings.TrimSpace(sub.Lang); lang != "" {
			fmt.Fprintf(&b, "      <textstream src=%q systemLanguage=%q/>\n",
				escapeAttr(src), escapeAttr(lang))
		} else {
			fmt.Fprintf(&b, "      <textstream src=%q/>\n", escapeAttr(src))
		}
	}
	b.WriteString("    </par>\n")
	b.WriteString("  </body>\n")
	b.WriteString("</smil>\n")
	return b.String()
}

// writeMeta emits a SMIL <meta name=.. content=../> line, skipping empty values
// so an unreported metadata field is simply absent (fail open).
func writeMeta(b *strings.Builder, name, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	fmt.Fprintf(b, "    <meta name=%q content=%q/>\n", escapeAttr(name), escapeAttr(value))
}

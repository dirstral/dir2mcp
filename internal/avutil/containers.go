package avutil

import "strings"

// Container extension tables. They are the single source for every place that
// decides "is this media" by file extension: ingest classification, media
// rendition grouping, open_file's media gate, the clip and embedding MIME
// types, and the ffmpeg segment muxer. ffmpeg probes an input by its content,
// so each container here decodes for duration, audio extraction and time-span
// clips; the extension only names the output muxer and the MIME type.

// audioContainerExts lists the extensions that always hold audio.
var audioContainerExts = map[string]bool{
	".mp3": true, ".wav": true, ".m4a": true, ".flac": true,
	".aac": true, ".ogg": true, ".opus": true,
}

// videoContainerMIME maps each video container extension to the MIME type of
// that container. A stream-copied clip keeps the source container, so it keeps
// this type too.
var videoContainerMIME = map[string]string{
	".mp4":  "video/mp4",
	".mov":  "video/quicktime",
	".m4v":  "video/mp4",
	".mkv":  "video/x-matroska",
	".webm": "video/webm",
	".avi":  "video/x-msvideo",
	".flv":  "video/x-flv",
	".wmv":  "video/x-ms-wmv",
	".mpg":  "video/mpeg",
	".mpeg": "video/mpeg",
	".vob":  "video/mpeg",
	".mxf":  "application/mxf",
	".3gp":  "video/3gpp",
	".m2ts": "video/mp2t",
	// The extensions below are shared with other formats. They are video only
	// when the content says so (see IsContentSniffedVideoExt).
	".ts":  "video/mp2t",
	".mts": "video/mp2t",
	".mod": "video/mpeg",
}

// contentSniffedVideoExts lists the extensions that a video container shares
// with another format: .ts and .mts with TypeScript, .mod with tracker music
// and other module files. Only the content can tell them apart.
var contentSniffedVideoExts = map[string]bool{
	".ts": true, ".mts": true, ".mod": true,
}

// segmentMuxerExt maps a source extension that ffmpeg cannot use to pick an
// output muxer to one that names the same container. A camcorder .mod file is
// an MPEG program stream, and ffmpeg has no muxer registered for ".mod". Some
// ffmpeg builds map ".m4v" to the raw MPEG-4 video muxer, which drops the
// audio track; the MP4 muxer writes the same container with audio.
var segmentMuxerExt = map[string]string{
	".mod": ".mpg",
	".m4v": ".mp4",
}

func normExt(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return ext
}

// IsAudioContainerExt reports whether ext (with or without the leading dot,
// any case) is an audio container.
func IsAudioContainerExt(ext string) bool {
	return audioContainerExts[normExt(ext)]
}

// IsVideoContainerExt reports whether ext (with or without the leading dot,
// any case) always holds video. It is false for the extensions that
// IsContentSniffedVideoExt reports, because those need the content to decide.
func IsVideoContainerExt(ext string) bool {
	e := normExt(ext)
	_, ok := videoContainerMIME[e]
	return ok && !contentSniffedVideoExts[e]
}

// IsContentSniffedVideoExt reports whether ext (with or without the leading
// dot, any case) is a video container extension that another format also uses
// (.ts, .mts, .mod), so only the file content can decide the type.
func IsContentSniffedVideoExt(ext string) bool {
	return contentSniffedVideoExts[normExt(ext)]
}

// VideoContainerMIME returns the MIME type of the video container that ext
// names, and false for an extension that is not a video container. The caller
// must already know the file is video when ext is content-sniffed.
func VideoContainerMIME(ext string) (string, bool) {
	mime, ok := videoContainerMIME[normExt(ext)]
	return mime, ok
}

// SegmentOutputExt returns the extension extractSegment gives its output file
// so that ffmpeg picks the muxer of the source container. It is srcExt itself
// for every container ffmpeg knows by extension.
func SegmentOutputExt(srcExt string) string {
	if mapped, ok := segmentMuxerExt[normExt(srcExt)]; ok {
		return mapped
	}
	return srcExt
}

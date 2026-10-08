package subtitle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Provenance markers (SPEC §8.6.14).
//
// A subtitle file dir2mcp writes beside media is its own output, not a human's.
// The state database records that, but a database that is reset, lost with a
// disk, or wiped for a fresh index forgets it, and every written file would then
// be read back as an authored sidecar: no re-transcription for those videos,
// ever, and nothing left to tell the files apart from legacy ones. So the file
// certifies itself. VTT and TTML output carries a marker line that names the
// tool and the SHA-256 of everything after the marker. A reader that finds a
// marker whose hash matches the body knows dir2mcp wrote the file and nobody
// changed it; a marker whose hash does not match is an edited file, which is
// authored. SRT has no comment syntax and carries no marker.
//
// Both parsers already ignore the marker: a WebVTT NOTE block is skipped by
// ParseVTT, and an XML comment is skipped by the TTML decoder, so a marked file
// round-trips to the same cues as an unmarked one.

// provenanceTag is the marker's fixed prefix. The version lets the hash rule
// change without misreading older markers.
const provenanceTag = "dir2mcp-emitted v1 sha256="

// vttHeaderLine is the WebVTT signature every RenderVTT output starts with.
const vttHeaderLine = "WEBVTT"

// StampVTT returns doc with a provenance NOTE block inserted after the WEBVTT
// header. doc must start with the WEBVTT header line RenderVTT writes; anything
// else is returned unchanged. The hash covers every byte after the marker's
// terminating blank line, which is exactly RenderVTT's cue body.
func StampVTT(doc string) string {
	header, body, ok := strings.Cut(doc, "\n")
	if !ok || !strings.HasPrefix(header, vttHeaderLine) {
		return doc
	}
	// RenderVTT writes "WEBVTT\n\n<cues>"; keep the header's own blank line so
	// the body stays byte-identical to the unmarked render minus its header.
	body = strings.TrimPrefix(body, "\n")
	return header + "\n\nNOTE " + provenanceTag + bodyHash([]byte(body)) + "\n\n" + body
}

// StampTTML returns doc with a provenance XML comment inserted after the XML
// declaration RenderTTML writes. A document without that declaration on its
// first line is returned unchanged. The hash covers every byte after the
// comment line.
func StampTTML(doc string) string {
	decl, body, ok := strings.Cut(doc, "\n")
	if !ok || !strings.HasPrefix(decl, "<?xml") {
		return doc
	}
	return decl + "\n<!-- " + provenanceTag + bodyHash([]byte(body)) + " -->\n" + body
}

// ProvenanceHeadBytes is how much of a file's start MayCarryProvenance needs:
// the marker sits right after the header line, well inside this window.
const ProvenanceHeadBytes = 512

// MayCarryProvenance reports whether a file's first bytes contain the marker
// tag. It lets a caller rule out an unmarked file (every legacy or authored
// subtitle) with one small read before reading the whole file.
func MayCarryProvenance(head []byte) bool {
	return bytes.Contains(head, []byte(provenanceTag))
}

// Provenance is the result of reading a subtitle file for a marker.
type Provenance struct {
	// Marked reports whether a dir2mcp provenance marker was found at all.
	Marked bool
	// Intact reports whether the marker's hash matches the body: the file is
	// dir2mcp's output and has not been changed since. False with Marked true
	// means an edited file.
	Intact bool
	// SHA256 is the hex hash the marker carries (empty when not marked).
	SHA256 string
}

// ReadProvenance inspects a subtitle document's bytes for a marker. It accepts
// the VTT shape ("WEBVTT...\n\nNOTE <tag>\n\n<body>") and the TTML shape
// ("<?xml...?>\n<!-- <tag> -->\n<body>"). Any other content is unmarked.
func ReadProvenance(data []byte) Provenance {
	if hash, body, ok := splitVTTMarker(data); ok {
		return Provenance{Marked: true, Intact: bodyHash(body) == hash, SHA256: hash}
	}
	if hash, body, ok := splitTTMLMarker(data); ok {
		return Provenance{Marked: true, Intact: bodyHash(body) == hash, SHA256: hash}
	}
	return Provenance{}
}

func splitVTTMarker(data []byte) (hash string, body []byte, ok bool) {
	if !bytes.HasPrefix(data, []byte(vttHeaderLine)) {
		return "", nil, false
	}
	nl := bytes.IndexByte(data, '\n')
	if nl < 0 {
		return "", nil, false
	}
	rest := data[nl+1:]
	rest = bytes.TrimPrefix(rest, []byte("\n"))
	marker := []byte("NOTE " + provenanceTag)
	if !bytes.HasPrefix(rest, marker) {
		return "", nil, false
	}
	end := bytes.Index(rest, []byte("\n\n"))
	if end < 0 {
		return "", nil, false
	}
	hash = strings.TrimSpace(string(rest[len(marker):end]))
	return hash, rest[end+2:], validHex(hash)
}

func splitTTMLMarker(data []byte) (hash string, body []byte, ok bool) {
	if !bytes.HasPrefix(data, []byte("<?xml")) {
		return "", nil, false
	}
	nl := bytes.IndexByte(data, '\n')
	if nl < 0 {
		return "", nil, false
	}
	rest := data[nl+1:]
	open := []byte("<!-- " + provenanceTag)
	if !bytes.HasPrefix(rest, open) {
		return "", nil, false
	}
	closeAt := bytes.Index(rest, []byte(" -->\n"))
	if closeAt < 0 {
		return "", nil, false
	}
	hash = strings.TrimSpace(string(rest[len(open):closeAt]))
	return hash, rest[closeAt+len(" -->\n"):], validHex(hash)
}

func bodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func validHex(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

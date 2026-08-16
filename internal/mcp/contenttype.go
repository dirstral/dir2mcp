package mcp

import "mime"

// jsonMediaType is the only request media type this server serves on the MCP
// path. It is the bare media type, with no parameters: the spelling the MCP Go
// SDK compares against.
const jsonMediaType = "application/json"

// hasJSONContentType reports whether a Content-Type header names the JSON media
// type.
//
// It decides on the MEDIA TYPE alone. RFC 9110 §8.3 lets a sender append
// parameters, and RFC 8259 §11 registers application/json with none ("No
// 'charset' parameter is defined for this registration. Adding one really has
// no effect on compliant recipients."), so `application/json; charset=utf-8`
// carries exactly the same payload as `application/json` and must be served
// (issue #841). Many HTTP clients and proxies add that parameter by themselves.
//
// mime.ParseMediaType handles the casing, the surrounding whitespace and the
// quoting, so this stays a single comparison against one canonical token. The
// media type itself is still matched whole: `application/jsonx` is a different
// media type and is refused.
//
// A header mime.ParseMediaType cannot parse is refused. A malformed header
// states nothing the server can act on, and an empty header states nothing at
// all.
func hasJSONContentType(header string) bool {
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return false
	}
	return mediaType == jsonMediaType
}

package mcp

import (
	"log"
	"net/http"
	"net/url"
	"strings"
)

// sdkOriginGuard keeps the MCP SDK's cross-origin protection and this server's
// `allowed_origins` policy in agreement (issue #652).
//
// Two guards run on an MCP-path POST or DELETE, in this order:
//
//  1. Server.allowOrigin, which enforces the validated `allowed_origins`
//     allowlist and answers 403 FORBIDDEN_ORIGIN when the origin is not named
//     (SPEC §10.5, §17).
//  2. the SDK's http.CrossOriginProtection, inside StreamableHTTPHandler.
//
// Guard 2 knew nothing about the allowlist, so it refused every cross-origin
// browser request that guard 1 had just accepted. The operator configured an
// origin and the browser client still failed.
//
// The fix keeps both guards and gives them one policy:
//
//   - newSDKOriginGuard builds the SDK guard from the same allowlist, so every
//     allowlist entry that is a complete origin becomes a trusted origin.
//   - adjudicate covers the two allowlist forms the SDK's exact-string
//     trusted-origin API cannot express.
//
// Disabling the SDK guard is not an option: it also refuses a cross-site request
// that carries no Origin header at all, a case the allowlist cannot judge.
type sdkOriginGuard struct {
	crossOrigin *http.CrossOriginProtection
}

// secFetchSiteAdjudicated is the Sec-Fetch-Site value that makes the SDK guard
// accept a request whose origin this server already accepted. The SDK reads this
// header first, so it is the one field that carries the decision.
const secFetchSiteAdjudicated = "same-origin"

// newSDKOriginGuard builds the guard from the validated allowlist.
//
// An allowlist entry becomes a trusted origin when it is a complete origin
// (scheme://host[:port]). An entry in any other form stays out of the trusted
// set, and adjudicate handles it per request.
func newSDKOriginGuard(allowedOrigins []string) *sdkOriginGuard {
	guard := &sdkOriginGuard{crossOrigin: http.NewCrossOriginProtection()}
	for _, allowed := range allowedOrigins {
		trusted := trustedOriginForm(allowed)
		if trusted == "" {
			continue
		}
		if err := guard.crossOrigin.AddTrustedOrigin(trusted); err != nil {
			// trustedOriginForm only returns a complete origin, so this branch is
			// defensive. The allowlist check stays the authority, so a rejected
			// form is reported and is not fatal.
			log.Printf("warning: the MCP SDK cross-origin guard rejected allowed origin %q; the allowed_origins check stays the authority for it: %v", trusted, err)
		}
	}
	return guard
}

// trustedOriginForm returns the exact Origin string to trust for an allowlist
// entry, or "" when the entry is not a complete origin.
//
// The SDK guard compares the Origin header by exact string, so the entry is
// normalized the same way isOriginAllowed normalizes an origin: a lowercase
// scheme and a lowercase host, with the port kept as written. The result is
// therefore always an origin the allowlist accepts as well, so the SDK's trusted
// set can never be wider than the configured policy.
func trustedOriginForm(allowed string) string {
	allowed = strings.TrimSpace(allowed)
	if allowed == "" || !strings.Contains(allowed, "://") {
		// A bare host entry matches any scheme and any port. It is not a
		// complete origin, so adjudicate handles it per request.
		return ""
	}
	parsed, err := url.Parse(allowed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	if parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
}

// protection returns the guard to hand to the SDK handler.
func (g *sdkOriginGuard) protection() *http.CrossOriginProtection {
	if g == nil {
		return nil
	}
	return g.crossOrigin
}

// check runs the SDK's own cross-origin check and returns its error.
//
// The transport calls this after adjudicate, so it can answer a refusal with this
// server's canonical contract. Left to the SDK, the same refusal arrives as a
// bare text/plain body from inside the streamable handler, with no JSON-RPC
// envelope and no canonical code, and the SDK ignores a deny handler. An opaque
// 403 that names no code is what made issue #652 hard to diagnose.
//
// This is the SDK's own function on the SDK's own guard, so the set of refused
// requests does not change. Only the response the client reads changes.
func (g *sdkOriginGuard) check(req *http.Request) error {
	if g == nil || g.crossOrigin == nil || req == nil {
		return nil
	}
	return g.crossOrigin.Check(req)
}

// adjudicate records, on the request, that the `allowed_origins` allowlist has
// already accepted this origin.
//
// Call it only after Server.allowOrigin returned true, and only on the path that
// forwards the request to the SDK handler. A refused origin never reaches this
// function: allowOrigin writes 403 and stops.
//
// Why it is needed. The SDK guard matches trusted origins by exact string, so it
// cannot express the two looser allowlist forms this server supports:
//
//   - a scheme+host entry matches any port ("http://localhost" allows
//     "http://localhost:5173"), which is the shipped default;
//   - a bare host entry matches any scheme and any port.
//
// Learning each concrete origin into the SDK's trusted set is not safe: that set
// only grows and has no removal, so an allowed host with a free port or scheme
// would let a caller grow it without bound. Instead, the decision the allowlist
// already made is written into the field the SDK guard reads, and only when the
// two guards actually disagree.
//
// A request with no Origin header is left untouched. The allowlist has no origin
// to judge there, so the SDK guard keeps full authority and still refuses a
// request that declares itself cross-site without naming an origin.
//
// Consequence to keep in mind. This makes `allowed_origins` effective, so the
// policy in the configuration is now the policy on the wire, for better and for
// worse. The shipped default names "http://localhost" and "http://127.0.0.1", and
// a scheme+host entry matches any port, so by default any page served from the
// local machine is an allowed origin. Two things bound that:
//
//   - authorize runs BEFORE the origin decision on this path, and auth is on by
//     default, so a page that holds no bearer token gets 401 and reaches no tool;
//   - an operator who wants a narrower policy narrows `allowed_origins`.
//
// An operator who runs with `--auth none` therefore serves every local origin,
// which is what that combination of settings asks for. The SDK's default guard
// used to hide this by refusing the configured policy outright.
func (g *sdkOriginGuard) adjudicate(req *http.Request) {
	if g == nil || g.crossOrigin == nil || req == nil {
		return
	}
	if strings.TrimSpace(req.Header.Get("Origin")) == "" {
		return
	}
	if g.crossOrigin.Check(req) == nil {
		// The SDK guard already agrees. Leave the request exactly as it came.
		return
	}
	req.Header.Set("Sec-Fetch-Site", secFetchSiteAdjudicated)
}

package web

// Content Security Policy
//
// One policy string, defined here and applied by the server's secureHeaders middleware to every
// web response. It lives in this package rather than next to the middleware because the OAuth
// consent screen has to widen one directive of it at render time, and a policy defined in two
// places is a policy that drifts.
//
// @joestump-agent 09/04/2026 - Extracted from internal/server.secureHeaders and gave it the
// form-action seam, fixing the OAuth consent screen (see CSPAllowingFormActionTo).

import (
	"net/url"
	"strings"
)

// formActionSelf is the directive CSPAllowingFormActionTo widens. Kept as its own constant so the
// rewrite below cannot silently no-op if the policy is ever reordered or reworded.
const formActionSelf = "form-action 'self'"

// ContentSecurityPolicy is the policy every web route carries. It is tuned to the actual web UI:
// an external stylesheet under /static plus an inline <style> block and inline style="" attributes
// (hence style-src 'unsafe-inline'); the only scripts are the vendored htmx assets embedded and
// served from /static (ADR-0001: no CDN), so script-src stays locked to 'self'. connect-src 'self'
// is explicit — it permits exactly the same-origin SSE stream (/events) and HTMX fetches, so
// injected markup cannot exfiltrate to another origin even under default-src drift. base-uri 'none'
// forbids <base> entirely (no page needs one, and an injected <base> would rebase every relative
// form action and asset URL). frame-ancestors 'none' backs up X-Frame-Options: DENY.
//
// Governing: SPEC-0012 REQ "Security Headers" (base-uri 'none', explicit connect-src), SPEC-0013
// "Security Headers" (same-origin connect-src for SSE).
const ContentSecurityPolicy = "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
	"script-src 'self'; connect-src 'self'; base-uri 'none'; " + formActionSelf + "; frame-ancestors 'none'"

// CSPAllowingFormActionTo returns ContentSecurityPolicy with the OAuth client's callback origin
// added to form-action, and is used by exactly one page: the consent screen.
//
// Why it has to exist. form-action governs where a form may submit AND every redirect that
// submission follows. The consent form posts same-origin to /oauth/authorize, which is why
// 'self' looks sufficient — but approving mints a code and 302s to the client's registered
// callback, which is by definition another origin (for a CLI, an RFC 8252 loopback
// http://127.0.0.1:<port>). Safari and Firefox enforce form-action across that redirect and
// refuse the navigation outright; Chrome does not follow redirects through form-action at all, so
// it never noticed. The failure mode is silent and expensive to read: the code is minted
// server-side, the browser drops the callback on the floor, the page appears to reload unchanged,
// and the only trace is a console line and an oauth_codes row whose used_at stays NULL forever.
//
// Why it is safe. redirectURI reaches this function only after parseAuthorizeRequest has
// exact-matched it against the client's registered allowlist, and it is the very URI this page is
// about to redirect to. Permitting the navigation grants nothing the redirect itself does not.
// Only the origin is added — never the path or query — and a URI that will not parse, or carries
// no scheme/host, widens nothing.
//
// Governing: SPEC-0016 REQ "Authorization Code Flow With Consent"; RFC 8252 section 7.3 (loopback
// redirect URIs); ADR-0019.
func CSPAllowingFormActionTo(redirectURI string) string {
	u, err := url.Parse(redirectURI)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ContentSecurityPolicy
	}
	origin := u.Scheme + "://" + u.Host
	return strings.Replace(ContentSecurityPolicy, formActionSelf, formActionSelf+" "+origin, 1)
}

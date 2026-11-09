package provider

// Connection Addresses And The Operator's Origin Allowlist
//
// A reconcile pass reads a provider back on the strength of values stored on a webhook row: an
// installation address, a repository, and a token that authenticates the read (#614). Those three
// arrive from a database that is, by ADR-0002, inside the trust boundary but written by software that
// is not — so the address is checked where it is written, not where it is dialed. A stored value that
// has already been refused never reaches a client, which is the whole reason these helpers are
// exported rather than folded into newHTTPBase: the write path needs the same rules the read clients
// enforce, and one more on top (an address must survive being stored, so it may not carry anything a
// future reader would have to interpret, such as credentials or a query).
//
// The doctrine matches the read clients: this file builds no request, dials nothing, and reads no
// credential. It turns a string a human or an agent wrote into either a canonical value or an error.
//
// An https origin is required, with loopback exempted: a read token rides in an Authorization header,
// so an http address sends it in the clear to anyone on the path. Loopback is exempt because local
// development and the httptest servers these clients are tested against are exactly that — there is
// no path for the token to travel, and refusing it would push tests toward disabling the check
// wholesale, which is worse than the exemption.
//
// The allowlist is an operator bound, the same shape SWITCHBOARD_NOTIFY_HOOK_ALLOW_CIDRS takes for
// outbound notify hooks: off (empty) means any https origin is acceptable, and once set it applies to
// every tenant's webhook alike. It is compared on origin, never on prefix: "https://gitea.example"
// must not match "https://gitea.example.evil.test", which a string prefix would.
//
// Governing: stump.wtf/switchboard#614 (per-webhook connection config), ADR-0002 (the trust boundary),
// ADR-0052 (proposed: what a provider read may be used for).

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// maxBaseURLLen bounds a stored address well past any real API root while keeping a pathological
// value out of every log line and error message that quotes it.
const maxBaseURLLen = 2048

// ErrInvalidBaseURL is a provider address that cannot be stored or dialed: no host, a scheme other
// than http/https, an http origin that is not loopback, or userinfo, a query or a fragment in the
// value. errors.Is-matching so a write path can report "invalid_argument" rather than a sentence.
var ErrInvalidBaseURL = errors.New("provider: invalid provider base url")

// ErrOriginNotAllowed is a valid address whose origin the operator's allowlist does not include.
var ErrOriginNotAllowed = errors.New("provider: provider base url origin is not allowed by this instance")

// ErrInvalidRepo is a "owner/name" spec that is not exactly two bare path segments.
var ErrInvalidRepo = errors.New("provider: invalid repo")

// NormalizeBaseURL canonicalizes a provider API root for storage: lowercase scheme and host, no
// trailing slash on the path, and nothing that could hide a credential or make a later reader guess.
// The value it returns is what belongs in endpoint_webhooks.provider_base_url, and it is accepted
// unchanged by NewGitHub and NewGitea (a Gitea install's API root keeps its /api/v1 path).
func NormalizeBaseURL(raw string) (string, error) {
	canon, _, err := splitBaseURL(raw)
	return canon, err
}

// OriginOf canonicalizes a provider address the way NormalizeBaseURL does and returns only its origin
// (scheme://host[:port]), which is what an allowlist is written in.
func OriginOf(raw string) (string, error) {
	_, origin, err := splitBaseURL(raw)
	return origin, err
}

func splitBaseURL(raw string) (full, origin string, err error) {
	v := strings.TrimSpace(raw)
	switch {
	case v == "":
		return "", "", fmt.Errorf("%w: it is empty", ErrInvalidBaseURL)
	case len(v) > maxBaseURLLen:
		return "", "", fmt.Errorf("%w: %d bytes, the cap is %d", ErrInvalidBaseURL, len(v), maxBaseURLLen)
	}
	u, perr := url.Parse(v)
	if perr != nil {
		return "", "", fmt.Errorf("%w: %v", ErrInvalidBaseURL, perr)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", "", fmt.Errorf("%w: scheme %q: want https (or http on loopback)", ErrInvalidBaseURL, u.Scheme)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("%w: %q carries no host", ErrInvalidBaseURL, v)
	}
	if u.User != nil {
		return "", "", fmt.Errorf("%w: credentials in the url are never allowed", ErrInvalidBaseURL)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("%w: query and fragment are never allowed", ErrInvalidBaseURL)
	}
	host := strings.ToLower(u.Host)
	if scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return "", "", fmt.Errorf("%w: %s://%s is not https; a read token would travel in the clear", ErrInvalidBaseURL, scheme, host)
	}
	origin = scheme + "://" + host
	path := strings.TrimRight(u.EscapedPath(), "/")
	return origin + path, origin, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ParseAllowedOrigins reads SWITCHBOARD_PROVIDER_READ_ALLOWED_ORIGINS: a comma-separated list of
// origins (https://host or https://host:port; no path, credentials or query). Surrounding and repeated
// commas are ignored, an empty list is "no restriction", and anything else fails — a typo in a
// security-relevant allowlist must stop startup rather than silently admit the origin it failed to
// describe.
func ParseAllowedOrigins(list string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(list, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		full, origin, err := splitBaseURL(part)
		if err != nil {
			return nil, fmt.Errorf("%w: %q", ErrInvalidBaseURL, part)
		}
		if full != origin {
			return nil, fmt.Errorf("%w: %q: an allowlist entry is an origin, with no path", ErrInvalidBaseURL, part)
		}
		out = append(out, origin)
	}
	return out, nil
}

// CheckAllowedBaseURL reports whether an address may be stored given the operator's allowed origins.
// An empty allowlist admits every valid address; a set one is matched on origin equality.
func CheckAllowedBaseURL(raw string, allowed []string) error {
	if len(allowed) == 0 {
		return nil
	}
	_, origin, err := splitBaseURL(raw)
	if err != nil {
		return err
	}
	for _, a := range allowed {
		if strings.EqualFold(a, origin) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrOriginNotAllowed, origin)
}

// ParseRepoSpec turns the stored "owner/name" into the Repo a read client takes. The stored form is
// one column because the halves are never written apart; the split happens here so the rules stay in
// one place (Repo.validate owns what a segment may contain).
func ParseRepoSpec(spec string) (Repo, error) {
	v := strings.TrimSpace(spec)
	owner, name, found := strings.Cut(v, "/")
	if !found || strings.Contains(name, "/") {
		return Repo{}, fmt.Errorf("%w: %q: want owner/name", ErrInvalidRepo, spec)
	}
	r := Repo{Owner: owner, Name: name}
	if verr := r.validate(); verr != nil {
		return Repo{}, fmt.Errorf("%w: %v", ErrInvalidRepo, verr)
	}
	return r, nil
}

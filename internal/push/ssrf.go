// Package push holds the A2A PushNotificationConfig machinery: the shared SSRF-guard URL validator
// (this file) and — in follow-up stores — the CRUD storage and authenticated, retried delivery path.
//
// The validator is deliberately a standalone, dependency-light helper so the SAME code runs at two
// distinct moments: once at CreateTaskPushNotificationConfig time (fail fast with a clear error before
// a bad URL is ever persisted) and again immediately before every delivery attempt (re-resolve the
// host to defend against DNS-rebinding — a URL that resolved to a public address at registration can
// be repointed at an internal address before delivery fires). Both call sites share one Validator so
// the allow/deny rules can never silently diverge between "what we accepted" and "what we'll connect
// to". The delivery path calls Validate right before dialing and treats any error as fail-closed: it
// MUST NOT connect on a validation error.
//
// Governing: ADR-0021 (A2A task-delegation transport),
// SPEC-0019 REQ "Webhook Target Validation (SSRF Guard)".
package push

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrValidation is the sentinel wrapping every SSRF-guard rejection. Callers use errors.Is(err,
// ErrValidation) to map a rejection to the A2A webhook-validation error shape at creation time and to
// the fail-closed path at delivery time. Every returned error wraps it with contextual detail (the
// scheme, host, or resolved address that failed) per SPEC-0019 REQ "Error Handling Standards".
var ErrValidation = errors.New("push: webhook target validation failed")

// Resolver resolves a host to its IP addresses. It is injected so tests can drive the DNS-rebinding
// scenario deterministically (register-time resolution vs. delivery-time resolution returning a
// different, disallowed address) without real DNS. *net.Resolver satisfies it; DefaultResolver wraps
// the process resolver. The context bounds the lookup so a slow or hostile resolver cannot hang the
// delivery path.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// DefaultResolver is the process DNS resolver, used when a Validator is built without an explicit one.
var DefaultResolver Resolver = net.DefaultResolver

// Validator enforces the SSRF guard on a caller-supplied webhook URL. It is safe for concurrent use:
// all fields are read-only after construction. Construct it once (at wiring time) and reuse it at both
// creation and delivery time.
type Validator struct {
	// allowHTTP, when true, permits http:// targets — an explicit operator opt-in for a documented
	// non-production scope. Off by default: https is required. Governing: SPEC-0019 REQ "Webhook
	// Target Validation (SSRF Guard)".
	allowHTTP bool
	// resolver resolves the target host at both creation and delivery time. Never nil after New.
	resolver Resolver
	// ownIPs are switchboard's own listening address(es); a target that resolves to one of these is
	// rejected so the guard cannot be turned back on switchboard itself. Loopback/private ranges are
	// already denied wholesale, so this matters mainly for a non-loopback bind (e.g. 0.0.0.0 expanded
	// to a routable interface address, or an explicit public bind).
	ownIPs []net.IP
}

// Option configures a Validator at construction.
type Option func(*Validator)

// WithAllowHTTP permits http:// targets when allow is true — the explicit operator opt-in for a
// documented non-production scope. Default (option omitted) requires https.
func WithAllowHTTP(allow bool) Option {
	return func(v *Validator) { v.allowHTTP = allow }
}

// WithResolver injects the DNS resolver (tests use this to simulate DNS rebinding). Omit it to use
// DefaultResolver.
func WithResolver(r Resolver) Option {
	return func(v *Validator) {
		if r != nil {
			v.resolver = r
		}
	}
}

// WithOwnListenAddrs registers switchboard's own listening address(es) so a target resolving to one
// of them is rejected. Each addr is a listen address (host, host:port, or a bare IP); the host
// portion is parsed as an IP and non-IP hosts (e.g. a "0.0.0.0" wildcard is an IP and is kept; a
// "localhost" bind is already covered by the loopback rule) are skipped. A ":8080"-style
// port-only bind contributes no specific IP (it binds all interfaces, already covered by the
// range rules), so it is skipped without error.
func WithOwnListenAddrs(addrs ...string) Option {
	return func(v *Validator) {
		for _, a := range addrs {
			v.ownIPs = append(v.ownIPs, parseListenIPs(a)...)
		}
	}
}

// New builds a Validator. With no options it requires https, denies loopback/link-local/private/
// unspecified/multicast/carrier-grade-NAT ranges, and uses the process DNS resolver.
func New(opts ...Option) *Validator {
	v := &Validator{resolver: DefaultResolver}
	for _, opt := range opts {
		opt(v)
	}
	if v.resolver == nil {
		v.resolver = DefaultResolver
	}
	return v
}

// Validate checks a caller-supplied webhook URL against the SSRF guard. It returns nil only when the
// URL parses, its scheme is permitted, and EVERY address the host currently resolves to is a
// permitted (public) address that is not one of switchboard's own listening addresses. It resolves
// the host on each call, so calling it immediately before a delivery attempt re-checks the current
// DNS answer and catches a rebind to a disallowed range. Any non-nil error wraps ErrValidation and
// carries context for the server log; the delivery path MUST treat a non-nil error as fail-closed (do
// not connect). The context can name a resolved address, so a caller-facing message uses
// PublicReason(err), never err.Error().
//
// It rejects if ANY resolved address is disallowed (not just the first): a host that resolves to both
// a public and a private address must not be reachable, since Go's dialer may pick any of them.
func (v *Validator) Validate(ctx context.Context, raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return &refusal{RefusedMalformed, fmt.Sprintf("unparseable url: %v", err)}
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "https":
		// always allowed
	case "http":
		if !v.allowHTTP {
			return &refusal{RefusedScheme, fmt.Sprintf("scheme %q requires https (http allowed only under the operator opt-in for non-prod)", u.Scheme)}
		}
	default:
		return &refusal{RefusedScheme, fmt.Sprintf("scheme %q not allowed (must be https)", u.Scheme)}
	}

	host := u.Hostname()
	if host == "" {
		return &refusal{RefusedMalformed, "url has no host"}
	}

	// If the host is a literal IP, check it directly — no DNS to resolve, and no rebinding possible.
	if literal := net.ParseIP(host); literal != nil {
		if err := v.checkIP(literal); err != nil {
			return err
		}
		return nil
	}

	addrs, err := v.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return &refusal{RefusedUnresolvable, fmt.Sprintf("resolve %q: %v", host, err)}
	}
	if len(addrs) == 0 {
		return &refusal{RefusedUnresolvable, fmt.Sprintf("host %q resolved to no addresses", host)}
	}
	// Fail closed if ANY resolved address is disallowed: the dialer may connect to any of them, so a
	// single private answer among public ones is enough to reach an internal service.
	for _, a := range addrs {
		if err := v.checkIP(a.IP); err != nil {
			return err
		}
	}
	return nil
}

// checkIP rejects an address that is not a permitted (public, routable, non-switchboard) target.
func (v *Validator) checkIP(ip net.IP) error {
	if ip == nil {
		return &refusal{RefusedAddress, "nil resolved address"}
	}
	if reason := disallowedReason(ip); reason != "" {
		return &refusal{RefusedAddress, fmt.Sprintf("address %s is %s", ip, reason)}
	}
	for _, own := range v.ownIPs {
		if own.Equal(ip) {
			return &refusal{RefusedAddress, fmt.Sprintf("address %s is switchboard's own listening address", ip)}
		}
	}
	return nil
}

// disallowedReason returns a non-empty human reason if ip falls in a range a webhook target must never
// resolve to: loopback, link-local (unicast or multicast), private (RFC 1918 / RFC 4193 ULA),
// unspecified, multicast, interface-local, or the IPv4 shared/CGNAT range (RFC 6598, 100.64.0.0/10).
// Empty string means the address is a permitted public target. IPv4-mapped IPv6 addresses are
// unmapped first so an ::ffff:127.0.0.1 cannot smuggle a loopback past the IPv4 checks.
func disallowedReason(ip net.IP) string {
	// Normalize an IPv4-mapped IPv6 address to its 4-byte form so the IPv4 range checks below apply.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	switch {
	case ip.IsUnspecified():
		return "the unspecified address"
	case ip.IsLoopback():
		return "a loopback address"
	case ip.IsLinkLocalUnicast():
		return "a link-local address"
	case ip.IsLinkLocalMulticast():
		return "a link-local multicast address"
	case ip.IsInterfaceLocalMulticast():
		return "an interface-local multicast address"
	case ip.IsMulticast():
		return "a multicast address"
	case ip.IsPrivate():
		return "a private address"
	case isSharedIPv4(ip):
		return "a shared/CGNAT address"
	default:
		return ""
	}
}

// isSharedIPv4 reports whether ip is in the IPv4 shared address space 100.64.0.0/10 (RFC 6598,
// carrier-grade NAT). net.IP.IsPrivate does not cover this range, but it is not a routable public
// destination and can front internal infrastructure, so the SSRF guard denies it too.
func isSharedIPv4(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	return v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
}

// parseListenIPs extracts the IP(s) a listen address binds. It accepts "host:port", a bare host, or a
// bare IP. A port-only bind (":8080") or a non-IP host binds no specific address the guard can pin, so
// it returns nil (the range rules already cover the wildcard case).
func parseListenIPs(addr string) []net.IP {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if host == "" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}
	}
	return nil
}

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
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// ErrValidation is the sentinel wrapping every SSRF-guard rejection. Callers use errors.Is(err,
// ErrValidation) to map a rejection to the A2A webhook-validation error shape at creation time and to
// the fail-closed path at delivery time. Every returned error wraps it with contextual detail (the
// scheme, host, or resolved address that failed) per SPEC-0019 REQ "Error Handling Standards".
var ErrValidation = errors.New("push: webhook target validation failed")

// ErrResolve additionally marks a rejection caused by the host not resolving at all (a lookup
// error or timeout, or an empty answer), as opposed to an address it resolved to being disallowed.
// It always travels with ErrValidation, so a caller that checks only ErrValidation still fails
// closed. A delivery-time caller uses it to tell a transient resolver outage (retryable) from an
// address-policy rejection (never retried). The lookup error is in the chain too, so
// errors.Is(err, context.DeadlineExceeded) works.
//
// Governing: SPEC-0024 REQ-7 (a network error or a timeout MUST be retried), REQ-8 (a DNS outage
// must not be recorded as an SSRF rejection).
var ErrResolve = errors.New("push: webhook target host did not resolve")

// resolveError keeps Resolve's established message ("<ErrValidation>: resolve ...") while
// unwrapping to ErrValidation, ErrResolve and the lookup error.
type resolveError struct {
	msg   string
	cause error
}

func (e *resolveError) Error() string { return e.msg }

func (e *resolveError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrValidation, ErrResolve}
	}
	return []error{ErrValidation, ErrResolve, e.cause}
}

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
	// ownAddrPorts are the listen addresses that named a port (e.g. 127.0.0.1:8080). An address the
	// CIDR allowlist exempts is compared against these as address PLUS port, so a same-host receiver
	// on another port is reachable while switchboard's own port stays refused (SPEC-0024 REQ-3).
	ownAddrPorts []netip.AddrPort
	// ownWildcardPorts are ports switchboard binds on every interface (":8080", "0.0.0.0:8080"). An
	// allowlist-exempted address on one of these ports may be switchboard itself, so it is refused.
	ownWildcardPorts []uint16
	// allow is the operator CIDR allowlist (WithAllowCIDRs). Empty by default: nothing is exempted.
	allow []netip.Prefix
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
			ap, wildcardPort := parseListenAddrPort(a)
			if ap.IsValid() {
				v.ownAddrPorts = append(v.ownAddrPorts, ap)
			}
			if wildcardPort != 0 {
				v.ownWildcardPorts = append(v.ownWildcardPorts, wildcardPort)
			}
		}
	}
}

// WithAllowCIDRs exempts the listed ranges from the private-address rejection: the operator bound
// for a single-tenant or homelab install whose receiver lives on the LAN (SPEC-0024 REQ-3,
// SWITCHBOARD_NOTIFY_HOOK_ALLOW_CIDRS). The exemption is deliberately narrow:
//
//   - private (RFC 1918), unique-local (RFC 4193) and shared/CGNAT (RFC 6598) addresses are exempted
//     by any listed range that contains them;
//   - loopback and link-local addresses are exempted only by a range lying WHOLLY inside loopback
//     (127.0.0.0/8, ::1/128) or link-local (169.254.0.0/16, fe80::/10), such as 127.0.0.1/32, so a
//     broad entry like 0.0.0.0/0 never opens loopback or a cloud metadata service;
//   - unspecified and multicast addresses are never exempted;
//   - an exempted address is still refused when it is switchboard's own listen address and port.
//
// Omit it (the default) and nothing is exempted.
func WithAllowCIDRs(prefixes ...netip.Prefix) Option {
	return func(v *Validator) {
		for _, p := range prefixes {
			if p.IsValid() {
				v.allow = append(v.allow, unmapPrefix(p))
			}
		}
	}
}

// ParseCIDRList parses a comma-separated CIDR list, the SWITCHBOARD_NOTIFY_HOOK_ALLOW_CIDRS format.
// A bare IP is a single-address range. Empty entries are skipped; any malformed entry is an error
// (wrapping ErrValidation) naming it, so a typo fails startup instead of silently allowing nothing.
func ParseCIDRList(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			out = append(out, p.Masked())
			continue
		}
		if a, err := netip.ParseAddr(part); err == nil {
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		return nil, fmt.Errorf("%w: %q is not a CIDR range or IP address", ErrValidation, part)
	}
	return out, nil
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
// carries context; the delivery path MUST treat a non-nil error as fail-closed (do not connect).
//
// It rejects if ANY resolved address is disallowed (not just the first): a host that resolves to both
// a public and a private address must not be reachable, since Go's dialer may pick any of them.
func (v *Validator) Validate(ctx context.Context, raw string) error {
	_, err := v.Resolve(ctx, raw)
	return err
}

// Target is a URL that passed the guard, with the one resolution that passed it. A caller that
// dials must connect to one of IPs and nothing else: resolving the host again between validation
// and dial reopens the DNS-rebinding window (SPEC-0024 REQ-3, design.md "the dial is pinned").
type Target struct {
	URL  *url.URL
	Host string   // the URL's hostname: TLS ServerName and the Host header
	Port string   // the explicit port, or the scheme default ("443" / "80")
	IPs  []net.IP // every address the host resolved to; each passed the guard
}

// Resolve is Validate returning what it validated: the parsed URL and the addresses it resolved,
// every one of which passed. It performs exactly one lookup (none for a literal IP). Errors are
// Validate's.
func (v *Validator) Resolve(ctx context.Context, raw string) (Target, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return Target{}, fmt.Errorf("%w: unparseable url: %v", ErrValidation, err)
	}
	scheme := strings.ToLower(u.Scheme)
	defaultPort := "443"
	switch scheme {
	case "https":
		// always allowed
	case "http":
		if !v.allowHTTP {
			return Target{}, fmt.Errorf("%w: scheme %q requires https (http allowed only under the operator opt-in for non-prod)", ErrValidation, u.Scheme)
		}
		defaultPort = "80"
	default:
		return Target{}, fmt.Errorf("%w: scheme %q not allowed (must be https)", ErrValidation, u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return Target{}, fmt.Errorf("%w: url has no host", ErrValidation)
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	portN, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portN == 0 {
		return Target{}, fmt.Errorf("%w: invalid port %q", ErrValidation, port)
	}
	t := Target{URL: u, Host: host, Port: port}

	// If the host is a literal IP, check it directly — no DNS to resolve, and no rebinding possible.
	if literal := net.ParseIP(host); literal != nil {
		if err := v.checkIP(literal, uint16(portN)); err != nil {
			return Target{}, err
		}
		t.IPs = []net.IP{literal}
		return t, nil
	}

	addrs, err := v.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return Target{}, &resolveError{msg: fmt.Sprintf("%s: resolve %q: %v", ErrValidation, host, err), cause: err}
	}
	if len(addrs) == 0 {
		return Target{}, &resolveError{msg: fmt.Sprintf("%s: host %q resolved to no addresses", ErrValidation, host)}
	}
	// Fail closed if ANY resolved address is disallowed: the dialer may connect to any of them, so a
	// single private answer among public ones is enough to reach an internal service.
	for _, a := range addrs {
		if err := v.checkIP(a.IP, uint16(portN)); err != nil {
			return Target{}, err
		}
		t.IPs = append(t.IPs, a.IP)
	}
	return t, nil
}

// checkIP rejects an address that is not a permitted (public, routable, non-switchboard) target, or
// an operator-allowlisted one that is switchboard's own listen address and port.
func (v *Validator) checkIP(ip net.IP, port uint16) error {
	if ip == nil {
		return fmt.Errorf("%w: nil resolved address", ErrValidation)
	}
	if reason := disallowedReason(ip); reason != "" {
		if !v.allowlisted(ip) {
			return fmt.Errorf("%w: address %s is %s", ErrValidation, ip, reason)
		}
		// Exempted by the operator allowlist: the range rule no longer protects switchboard itself,
		// so compare against its own listen address and port.
		if v.isOwnAddrPort(ip, port) {
			return fmt.Errorf("%w: address %s port %d is switchboard's own listening address", ErrValidation, ip, port)
		}
		return nil
	}
	for _, own := range v.ownIPs {
		if own.Equal(ip) {
			return fmt.Errorf("%w: address %s is switchboard's own listening address", ErrValidation, ip)
		}
	}
	return nil
}

// Ranges an allowlist entry must lie wholly inside to exempt a loopback or link-local address.
var (
	loopbackRanges  = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	linkLocalRanges = []netip.Prefix{netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("fe80::/10")}
)

// allowlisted reports whether the operator allowlist exempts ip from the range rules (the rules are
// on WithAllowCIDRs).
func (v *Validator) allowlisted(ip net.IP) bool {
	if len(v.allow) == 0 {
		return false
	}
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	var within []netip.Prefix // non-nil: the entry must lie wholly inside one of these
	switch {
	case a.IsUnspecified(), a.IsMulticast():
		return false
	case a.IsLoopback():
		within = loopbackRanges
	case a.IsLinkLocalUnicast():
		within = linkLocalRanges
	}
	for _, p := range v.allow {
		if !p.Contains(a) {
			continue
		}
		if within == nil || prefixInside(p, within) {
			return true
		}
	}
	return false
}

// isOwnAddrPort reports whether ip:port is one of switchboard's own listen addresses: an exact
// address-and-port match, a port bound on every interface, or a listen address given without a port
// (which covers every port of that address).
func (v *Validator) isOwnAddrPort(ip net.IP, port uint16) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	for _, ap := range v.ownAddrPorts {
		if ap.Addr().Unmap() == a && ap.Port() == port {
			return true
		}
	}
	for _, p := range v.ownWildcardPorts {
		if p == port {
			return true
		}
	}
	for _, own := range v.ownIPs {
		if own.Equal(ip) && !v.ownIPHasPort(own) {
			return true
		}
	}
	return false
}

// ownIPHasPort reports whether an own IP came from a listen address that named a port, in which
// case the address-and-port comparison above is the precise one.
func (v *Validator) ownIPHasPort(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	for _, ap := range v.ownAddrPorts {
		if ap.Addr().Unmap() == a {
			return true
		}
	}
	return false
}

// prefixInside reports whether p lies wholly inside one of the ranges.
func prefixInside(p netip.Prefix, ranges []netip.Prefix) bool {
	for _, r := range ranges {
		if r.Contains(p.Addr()) && p.Bits() >= r.Bits() {
			return true
		}
	}
	return false
}

// unmapPrefix normalizes an IPv4-mapped IPv6 prefix (::ffff:10.0.0.0/104) to its IPv4 form, so it
// matches the unmapped addresses allowlisted compares.
func unmapPrefix(p netip.Prefix) netip.Prefix {
	p = p.Masked()
	if p.Addr().Is4In6() {
		bits := p.Bits() - 96
		if bits < 0 {
			bits = 0
		}
		return netip.PrefixFrom(p.Addr().Unmap(), bits).Masked()
	}
	return p
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

// parseListenAddrPort extracts a listen address's specific address and port, or — for a bind on
// every interface (":8080", "0.0.0.0:8080", "[::]:8080") — the port alone. A listen address with no
// port yields neither.
func parseListenAddrPort(addr string) (netip.AddrPort, uint16) {
	host, portS, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return netip.AddrPort{}, 0
	}
	port, err := strconv.ParseUint(portS, 10, 16)
	if err != nil || port == 0 {
		return netip.AddrPort{}, 0
	}
	if host == "" {
		return netip.AddrPort{}, uint16(port)
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.AddrPort{}, 0
	}
	if a.IsUnspecified() {
		return netip.AddrPort{}, uint16(port)
	}
	return netip.AddrPortFrom(a.Unmap(), uint16(port)), 0
}

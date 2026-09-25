package push

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
)

// Tests for the additive SPEC-0024 REQ-3 options: the operator CIDR allowlist, the port-aware
// own-address check for allowlisted targets, and Resolve returning the validated addresses for a
// pinned dial. Governing: SPEC-0024 REQ-3 "Target Validation (SSRF Guard)", design.md "Private
// ranges are an operator bound, off by default".

func mustCIDRs(t *testing.T, s string) []netip.Prefix {
	t.Helper()
	p, err := ParseCIDRList(s)
	if err != nil {
		t.Fatalf("ParseCIDRList(%q): %v", s, err)
	}
	return p
}

// Scenario "Same-host receiver when the operator opts in".
func TestAllowlistSameHostReceiver(t *testing.T) {
	ctx := context.Background()
	v := New(
		WithAllowCIDRs(mustCIDRs(t, "127.0.0.1/32")...),
		WithOwnListenAddrs("127.0.0.1:8080"),
		WithResolver(&fakeResolver{byHost: map[string][]net.IPAddr{"harness.local": ipAddrs("127.0.0.1")}}),
	)
	if err := v.Validate(ctx, "https://harness.local:9443/hook"); err != nil {
		t.Fatalf("allowlisted same-host receiver on another port must pass: %v", err)
	}
	if err := v.Validate(ctx, "https://127.0.0.1:9443/hook"); err != nil {
		t.Fatalf("allowlisted literal on another port must pass: %v", err)
	}
	for _, u := range []string{"https://harness.local:8080/hook", "https://127.0.0.1:8080/hook", "http://127.0.0.1:8080/"} {
		err := New(
			WithAllowHTTP(true),
			WithAllowCIDRs(mustCIDRs(t, "127.0.0.1/32")...),
			WithOwnListenAddrs("127.0.0.1:8080"),
			WithResolver(&fakeResolver{byHost: map[string][]net.IPAddr{"harness.local": ipAddrs("127.0.0.1")}}),
		).Validate(ctx, u)
		if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "own listening address") {
			t.Fatalf("%s: switchboard's own address and port must stay refused, got %v", u, err)
		}
	}
	// Without the allowlist, loopback is refused on every port, as before.
	if err := New().Validate(ctx, "https://127.0.0.1:9443/"); !errors.Is(err, ErrValidation) {
		t.Fatalf("loopback without an allowlist must be refused, got %v", err)
	}
}

// Scenario "A broad entry does not open loopback or metadata".
func TestAllowlistBroadEntryKeepsLoopbackAndLinkLocalClosed(t *testing.T) {
	ctx := context.Background()
	v := New(WithAllowCIDRs(mustCIDRs(t, "0.0.0.0/0, ::/0")...))
	for _, u := range []string{
		"https://169.254.169.254/latest/meta-data",
		"https://127.0.0.1/",
		"https://[::1]/",
		"https://[fe80::1]/",
		"https://0.0.0.0/",
		"https://224.0.0.1/",
	} {
		if err := v.Validate(ctx, u); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: a broad allowlist entry must not open it, got %v", u, err)
		}
	}
	// The broad entry does exempt private, ULA and CGNAT addresses: that is what it is for.
	for _, u := range []string{"https://10.0.0.5/", "https://192.168.1.20/", "https://[fd00::5]/", "https://100.64.1.1/"} {
		if err := v.Validate(ctx, u); err != nil {
			t.Errorf("%s: a covering allowlist entry must exempt it: %v", u, err)
		}
	}
}

// Cloud metadata services that sit in CGNAT or ULA space are not opened by a range that merely
// covers them (0.0.0.0/0, 100.64.0.0/10, ::/0, fc00::/7); only an entry for exactly that address
// exempts one. Scenario "A broad entry does not open loopback or metadata".
func TestAllowlistBroadEntryKeepsMetadataClosed(t *testing.T) {
	ctx := context.Background()
	for _, list := range []string{"0.0.0.0/0, ::/0", "100.64.0.0/10, fc00::/7", "100.100.100.0/24, fd00:ec2::/64"} {
		v := New(WithAllowHTTP(true), WithAllowCIDRs(mustCIDRs(t, list)...))
		for _, u := range []string{"http://100.100.100.200/latest/meta-data/", "http://[fd00:ec2::254]/latest/meta-data/"} {
			if err := v.Validate(ctx, u); !errors.Is(err, ErrValidation) {
				t.Errorf("allow %q, %s: a covering entry must not open a metadata service, got %v", list, u, err)
			}
		}
		// The rest of the covered range is still exempted.
		for _, u := range []string{"http://100.100.100.201/", "http://[fd00:ec2::253]/"} {
			if err := v.Validate(ctx, u); err != nil {
				t.Errorf("allow %q, %s: a covering entry must exempt it: %v", list, u, err)
			}
		}
	}
	// An entry for exactly the address is the operator's explicit, narrow opt-in.
	v := New(WithAllowHTTP(true), WithAllowCIDRs(mustCIDRs(t, "100.100.100.200/32, fd00:ec2::254")...))
	for _, u := range []string{"http://100.100.100.200/", "http://[fd00:ec2::254]/", "http://[::ffff:100.100.100.200]/"} {
		if err := v.Validate(ctx, u); err != nil {
			t.Errorf("%s: an exact entry must exempt it: %v", u, err)
		}
	}
}

func TestAllowlistNarrowEntries(t *testing.T) {
	ctx := context.Background()
	v := New(WithAllowCIDRs(mustCIDRs(t, "192.168.1.0/24, 169.254.10.0/24")...))
	cases := map[string]bool{
		"https://192.168.1.7/":          true,  // inside the private entry
		"https://192.168.2.7/":          false, // private, not listed
		"https://10.0.0.5/":             false, // private, not listed (scenario "Private target refused at create")
		"https://169.254.10.9/":         true,  // link-local, entry wholly inside link-local
		"https://169.254.169.254":       false, // link-local, not covered
		"https://[::ffff:192.168.1.7]/": true,  // IPv4-mapped form of an allowed address
	}
	for u, ok := range cases {
		err := v.Validate(ctx, u)
		if ok && err != nil {
			t.Errorf("%s: want allowed, got %v", u, err)
		}
		if !ok && !errors.Is(err, ErrValidation) {
			t.Errorf("%s: want refused, got %v", u, err)
		}
	}
	// A refusal names the address class.
	if err := v.Validate(ctx, "https://10.0.0.5/"); err == nil || !strings.Contains(err.Error(), "private address") {
		t.Fatalf("refusal must name the address class, got %v", err)
	}
}

// A wildcard bind puts switchboard on every interface, so an allowlisted address on its port is
// refused, while other ports pass.
func TestAllowlistWildcardBindPort(t *testing.T) {
	ctx := context.Background()
	v := New(WithAllowCIDRs(mustCIDRs(t, "192.168.1.0/24")...), WithOwnListenAddrs("0.0.0.0:8080"))
	if err := v.Validate(ctx, "https://192.168.1.5:8080/"); !errors.Is(err, ErrValidation) {
		t.Fatalf("allowlisted address on the wildcard-bound port must be refused, got %v", err)
	}
	if err := v.Validate(ctx, "https://192.168.1.5:9443/"); err != nil {
		t.Fatalf("allowlisted address on another port must pass: %v", err)
	}
}

// A listen address whose host is a name, not an IP literal, names no address to compare, so its port
// is refused on every allowlisted address rather than on none (fail closed, SPEC-0024 REQ-3).
func TestAllowlistHostnameListenAddrFailsClosed(t *testing.T) {
	ctx := context.Background()
	for _, listen := range []string{"localhost:8080", "switchboard.internal:8080"} {
		v := New(
			WithAllowHTTP(true),
			WithAllowCIDRs(mustCIDRs(t, "127.0.0.1/32, 192.168.1.0/24")...),
			WithOwnListenAddrs(listen),
		)
		for _, u := range []string{"http://127.0.0.1:8080/", "https://192.168.1.5:8080/"} {
			err := v.Validate(ctx, u)
			if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "own listening address") {
				t.Errorf("listen %s, %s: switchboard's own port must stay refused, got %v", listen, u, err)
			}
		}
		if err := v.Validate(ctx, "http://127.0.0.1:9443/"); err != nil {
			t.Errorf("listen %s: an allowlisted address on another port must pass: %v", listen, err)
		}
	}
}

func TestParseCIDRList(t *testing.T) {
	got := mustCIDRs(t, " 10.0.0.0/8 ,,127.0.0.1, fd00::/8 ")
	want := []string{"10.0.0.0/8", "127.0.0.1/32", "fd00::/8"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("entry %d = %s, want %s", i, got[i], want[i])
		}
	}
	if p := mustCIDRs(t, ""); len(p) != 0 {
		t.Fatalf("empty list = %v", p)
	}
	if _, err := ParseCIDRList("10.0.0.0/8, nonsense"); !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "nonsense") {
		t.Fatalf("malformed entry must fail naming it, got %v", err)
	}
}

// Resolve returns the parsed URL, the SNI host, the effective port and exactly the addresses it
// validated, from one lookup: the dialer pins to these.
func TestResolveReturnsValidatedAddresses(t *testing.T) {
	ctx := context.Background()
	res := &countingResolver{answer: ipAddrs("93.184.216.34", "2606:2800:220:1::1")}
	v := New(WithResolver(res))
	tg, err := v.Resolve(ctx, "https://dispatch.example.com/sb?x=1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.calls != 1 {
		t.Fatalf("lookups = %d, want exactly 1", res.calls)
	}
	if tg.Host != "dispatch.example.com" || tg.Port != "443" || tg.URL.Path != "/sb" || len(tg.IPs) != 2 {
		t.Fatalf("target = %+v", tg)
	}
	if tg, err := New(WithAllowHTTP(true), WithResolver(res)).Resolve(ctx, "http://dispatch.example.com:8081/"); err != nil || tg.Port != "8081" {
		t.Fatalf("explicit port target = %+v, %v", tg, err)
	}
	if tg, err := New(WithAllowHTTP(true)).Resolve(ctx, "http://93.184.216.34/"); err != nil || tg.Port != "80" || len(tg.IPs) != 1 {
		t.Fatalf("literal http target = %+v, %v", tg, err)
	}
	if _, err := v.Resolve(ctx, "https://dispatch.example.com:0/"); !errors.Is(err, ErrValidation) {
		t.Fatalf("port 0 must be refused, got %v", err)
	}
	// A mixed answer still fails closed, and returns no addresses to dial.
	res.answer = ipAddrs("93.184.216.34", "10.0.0.1")
	if tg, err := v.Resolve(ctx, "https://dispatch.example.com/"); !errors.Is(err, ErrValidation) || len(tg.IPs) != 0 {
		t.Fatalf("mixed answer = %+v, %v; want refused with no addresses", tg, err)
	}
}

type countingResolver struct {
	answer []net.IPAddr
	calls  int
}

func (c *countingResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	c.calls++
	return c.answer, nil
}

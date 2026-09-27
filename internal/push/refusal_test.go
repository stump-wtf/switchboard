package push

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"testing"
)

// ipLiteral matches anything that looks like an IPv4 or IPv6 address, so a leaked address fails the
// assertion whatever its form.
var ipLiteral = regexp.MustCompile(`\d+\.\d+\.\d+\.\d+|[0-9a-fA-F]*:[0-9a-fA-F]*:[0-9a-fA-F:]*`)

// Every refusal maps to a fixed class phrase that names no resolved address and no resolver error,
// while the error itself keeps the detail for the server log. Governing: SPEC-0033 REQ "Owned Replay
// Targets" (audit F9).
func TestPublicReasonIsClassOnly(t *testing.T) {
	ctx := context.Background()
	res := &fakeResolver{byHost: map[string][]net.IPAddr{
		"db.corp.internal": ipAddrs("10.20.30.40"),
		"v6.corp.internal": ipAddrs("fd12:3456::7"),
		"mixed.example":    ipAddrs("203.0.113.10", "192.168.7.7"),
		"empty.example":    {},
		"own.example":      ipAddrs("198.51.100.9"),
	}}
	v := New(WithResolver(res), WithOwnListenAddrs("198.51.100.9:8080"))
	cases := []struct{ raw, class, detail string }{
		{"https://db.corp.internal/", RefusedAddress, "10.20.30.40"},
		{"https://v6.corp.internal/", RefusedAddress, "fd12:3456::7"},
		{"https://mixed.example/", RefusedAddress, "192.168.7.7"},
		{"https://own.example/", RefusedAddress, "198.51.100.9"},
		{"https://10.0.0.5/", RefusedAddress, "10.0.0.5"},
		{"https://empty.example/", RefusedUnresolvable, "empty.example"},
		{"http://203.0.113.10/", RefusedScheme, "http"},
		{"file:///etc/passwd", RefusedScheme, "file"},
		{"https:///pathonly", RefusedMalformed, "no host"},
	}
	for _, tc := range cases {
		err := v.Validate(ctx, tc.raw)
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("%s: got %v, want a refusal", tc.raw, err)
		}
		if got := PublicReason(err); got != tc.class {
			t.Errorf("%s: PublicReason = %q, want %q", tc.raw, got, tc.class)
		}
		if m := ipLiteral.FindString(PublicReason(err)); m != "" {
			t.Errorf("%s: PublicReason leaks %q", tc.raw, m)
		}
		if !strings.Contains(err.Error(), tc.detail) {
			t.Errorf("%s: log detail %q lost %q", tc.raw, err.Error(), tc.detail)
		}
	}
}

// A resolver failure (whose text can name the resolver's own address) reads as unresolvable, and the
// resolver's text never reaches the class.
func TestPublicReasonHidesResolverError(t *testing.T) {
	v := New(WithResolver(&fakeResolver{err: errors.New("lookup db.corp.internal on 10.0.0.2:53: no such host")}))
	err := v.Validate(context.Background(), "https://db.corp.internal/")
	if got := PublicReason(err); got != RefusedUnresolvable {
		t.Fatalf("PublicReason = %q, want %q", got, RefusedUnresolvable)
	}
	if !strings.Contains(err.Error(), "10.0.0.2:53") {
		t.Fatalf("the log detail %q lost the resolver error", err.Error())
	}
}

// An error the guard did not classify reads as the most conservative class, not its own text.
func TestPublicReasonUnclassified(t *testing.T) {
	for _, err := range []error{
		errors.New("address 10.1.1.1 is private"),
		fmt.Errorf("%w: dial address %q", ErrValidation, "10.1.1.1:443"),
	} {
		if got := PublicReason(err); got != RefusedAddress {
			t.Fatalf("PublicReason(%v) = %q, want %q", err, got, RefusedAddress)
		}
	}
}

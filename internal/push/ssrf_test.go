package push

import (
	"context"
	"errors"
	"net"
	"testing"
)

// fakeResolver drives the DNS-rebinding scenario deterministically: it maps a host to a fixed answer
// (and can be swapped between calls to simulate a record changing between registration and delivery).
type fakeResolver struct {
	byHost map[string][]net.IPAddr
	err    error
}

func (f *fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byHost[host], nil
}

func ipAddrs(ips ...string) []net.IPAddr {
	out := make([]net.IPAddr, 0, len(ips))
	for _, s := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out
}

// TestValidateScheme covers the https-only rule and the explicit http opt-in.
func TestValidateScheme(t *testing.T) {
	ctx := context.Background()
	// A routable public literal IP so scheme is the only variable under test.
	pub := "93.184.216.34"

	t.Run("https allowed", func(t *testing.T) {
		v := New(WithResolver(&fakeResolver{}))
		if err := v.Validate(ctx, "https://"+pub+"/hook"); err != nil {
			t.Fatalf("https public target should pass: %v", err)
		}
	})

	t.Run("http rejected by default", func(t *testing.T) {
		v := New(WithResolver(&fakeResolver{}))
		err := v.Validate(ctx, "http://"+pub+"/hook")
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("http must be rejected without the opt-in, got %v", err)
		}
	})

	t.Run("http allowed under opt-in", func(t *testing.T) {
		v := New(WithAllowHTTP(true), WithResolver(&fakeResolver{}))
		if err := v.Validate(ctx, "http://"+pub+"/hook"); err != nil {
			t.Fatalf("http should pass under the operator opt-in: %v", err)
		}
	})

	t.Run("non-http(s) scheme rejected", func(t *testing.T) {
		v := New(WithAllowHTTP(true), WithResolver(&fakeResolver{}))
		for _, raw := range []string{"ftp://" + pub, "file:///etc/passwd", "gopher://" + pub, "//" + pub} {
			if err := v.Validate(ctx, raw); !errors.Is(err, ErrValidation) {
				t.Fatalf("scheme in %q must be rejected, got %v", raw, err)
			}
		}
	})
}

// TestValidateRejectsDisallowedRanges asserts every private/loopback/link-local/etc. range is denied,
// whether supplied as a literal IP or resolved from a hostname.
func TestValidateRejectsDisallowedRanges(t *testing.T) {
	ctx := context.Background()
	disallowed := []string{
		"127.0.0.1",        // loopback v4
		"::1",              // loopback v6
		"10.1.2.3",         // RFC1918
		"172.16.5.6",       // RFC1918
		"192.168.1.1",      // RFC1918
		"169.254.10.20",    // link-local v4
		"fe80::1",          // link-local v6
		"0.0.0.0",          // unspecified v4
		"::",               // unspecified v6
		"fc00::1",          // ULA (private v6)
		"100.64.0.1",       // CGNAT / shared (RFC 6598)
		"224.0.0.1",        // multicast v4
		"ff02::1",          // link-local multicast v6
		"::ffff:127.0.0.1", // IPv4-mapped loopback — must not smuggle past the v4 checks
	}
	for _, ip := range disallowed {
		t.Run("literal/"+ip, func(t *testing.T) {
			v := New(WithResolver(&fakeResolver{}))
			// IPv6 literals must be bracketed in a URL host; IPv4 must not be.
			host := ip
			if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() == nil {
				host = "[" + ip + "]"
			}
			if err := v.Validate(ctx, "https://"+host+"/hook"); !errors.Is(err, ErrValidation) {
				t.Fatalf("literal %s must be rejected, got %v", ip, err)
			}
		})
		t.Run("resolved/"+ip, func(t *testing.T) {
			v := New(WithResolver(&fakeResolver{byHost: map[string][]net.IPAddr{"evil.example": ipAddrs(ip)}}))
			if err := v.Validate(ctx, "https://evil.example/hook"); !errors.Is(err, ErrValidation) {
				t.Fatalf("host resolving to %s must be rejected, got %v", ip, err)
			}
		})
	}
}

// TestValidateRejectsMixedAnswer asserts a host that resolves to BOTH a public and a private address
// is rejected — the dialer could pick the private one.
func TestValidateRejectsMixedAnswer(t *testing.T) {
	ctx := context.Background()
	v := New(WithResolver(&fakeResolver{byHost: map[string][]net.IPAddr{
		"mixed.example": ipAddrs("93.184.216.34", "10.0.0.5"),
	}}))
	if err := v.Validate(ctx, "https://mixed.example/hook"); !errors.Is(err, ErrValidation) {
		t.Fatalf("a host resolving to any private address must be rejected, got %v", err)
	}
}

// TestValidateOwnListenAddr asserts a target resolving to switchboard's own listening address is
// rejected even when that address is public/routable (loopback is already denied wholesale).
func TestValidateOwnListenAddr(t *testing.T) {
	ctx := context.Background()
	own := "203.0.113.9" // TEST-NET-3 public-shaped address standing in for a routable bind
	v := New(
		WithOwnListenAddrs(own+":8080", ":9090", "localhost:1234"),
		WithResolver(&fakeResolver{byHost: map[string][]net.IPAddr{
			"self.example":  ipAddrs(own),
			"other.example": ipAddrs("198.51.100.7"),
		}}),
	)
	if err := v.Validate(ctx, "https://self.example/hook"); !errors.Is(err, ErrValidation) {
		t.Fatalf("target resolving to switchboard's own address must be rejected, got %v", err)
	}
	// A different public address still passes — the own-address rule is specific, not a blanket public deny.
	if err := v.Validate(ctx, "https://other.example/hook"); err != nil {
		t.Fatalf("a different public target should pass: %v", err)
	}
	// The own listen address supplied as a literal is also rejected.
	if err := v.Validate(ctx, "https://"+own+"/hook"); !errors.Is(err, ErrValidation) {
		t.Fatalf("literal own address must be rejected, got %v", err)
	}
}

// TestValidateDNSRebinding is the delivery-time re-resolution scenario: a URL that resolved to a
// public address at registration re-resolves to a private one before delivery, and the re-check must
// fail closed. The same Validator instance is called twice against a resolver whose answer changes.
func TestValidateDNSRebinding(t *testing.T) {
	ctx := context.Background()
	res := &fakeResolver{byHost: map[string][]net.IPAddr{
		"rebind.example": ipAddrs("93.184.216.34"), // registration-time: public
	}}
	v := New(WithResolver(res))

	// Creation-time validation passes against the public answer.
	if err := v.Validate(ctx, "https://rebind.example/hook"); err != nil {
		t.Fatalf("registration-time validation should pass: %v", err)
	}

	// The record is repointed at an internal address before delivery fires.
	res.byHost["rebind.example"] = ipAddrs("10.0.0.99")

	// Delivery-time re-validation must now fail closed — the caller must NOT connect.
	if err := v.Validate(ctx, "https://rebind.example/hook"); !errors.Is(err, ErrValidation) {
		t.Fatalf("delivery-time re-resolution to a private address must fail closed, got %v", err)
	}
}

// TestValidateResolverError asserts a resolver failure fails closed (never connect on an unknown
// answer) rather than being treated as "no disallowed address found".
func TestValidateResolverError(t *testing.T) {
	ctx := context.Background()
	v := New(WithResolver(&fakeResolver{err: errors.New("dns down")}))
	if err := v.Validate(ctx, "https://whatever.example/hook"); !errors.Is(err, ErrValidation) {
		t.Fatalf("a resolver error must fail closed, got %v", err)
	}
}

// TestValidateMalformed covers empty/host-less/garbage URLs.
func TestValidateMalformed(t *testing.T) {
	ctx := context.Background()
	v := New(WithResolver(&fakeResolver{}))
	for _, raw := range []string{"", "   ", "https://", "not a url", "https:///pathonly"} {
		if err := v.Validate(ctx, raw); !errors.Is(err, ErrValidation) {
			t.Fatalf("malformed url %q must be rejected, got %v", raw, err)
		}
	}
}

// TestValidateNoAddresses asserts a host that resolves to an empty answer fails closed.
func TestValidateNoAddresses(t *testing.T) {
	ctx := context.Background()
	v := New(WithResolver(&fakeResolver{byHost: map[string][]net.IPAddr{"empty.example": {}}}))
	if err := v.Validate(ctx, "https://empty.example/hook"); !errors.Is(err, ErrValidation) {
		t.Fatalf("a host resolving to no addresses must fail closed, got %v", err)
	}
}

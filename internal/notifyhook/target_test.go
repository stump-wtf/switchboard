package notifyhook

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/push"
)

type fixedResolver map[string][]net.IPAddr

func (f fixedResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	return f[host], nil
}

func addrs(ips ...string) []net.IPAddr {
	var out []net.IPAddr
	for _, s := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out
}

func TestValidateURL(t *testing.T) {
	ctx := context.Background()
	v := push.New(push.WithResolver(fixedResolver{
		"dispatch.example.com": addrs("93.184.216.34"),
		"internal.example.com": addrs("10.0.0.5"),
	}))
	tg, err := ValidateURL(ctx, v, " https://dispatch.example.com/sb?token=abc ")
	if err != nil || tg.Host != "dispatch.example.com" || len(tg.IPs) != 1 {
		t.Fatalf("valid url = %+v, %v", tg, err)
	}

	for name, tc := range map[string]struct {
		url      string
		sentinel error
	}{
		"empty":           {"", ErrInvalidURL},
		"too long":        {"https://dispatch.example.com/" + strings.Repeat("a", MaxURLLen), ErrInvalidURL},
		"userinfo":        {"https://user:pass@dispatch.example.com/sb", ErrInvalidURL},
		"relative":        {"/just/a/path?token=abc", ErrInvalidURL},
		"unparseable":     {"https://dispatch.example.com/%zz?token=abc", ErrInvalidURL},
		"plain http":      {"http://dispatch.example.com/sb", push.ErrValidation},
		"private target":  {"https://internal.example.com/sb", push.ErrValidation},
		"metadata":        {"https://169.254.169.254/latest", push.ErrValidation},
		"not http at all": {"ftp://dispatch.example.com/", push.ErrValidation},
	} {
		_, err := ValidateURL(ctx, v, tc.url)
		if !errors.Is(err, tc.sentinel) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.sentinel)
		}
		if err != nil && strings.Contains(err.Error(), "token=abc") {
			t.Errorf("%s: error echoes the query string: %v", name, err)
		}
	}
	if _, err := ValidateURL(ctx, nil, "https://dispatch.example.com/"); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("nil validator must refuse, got %v", err)
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://dispatch.example.com/sb":                   "https://dispatch.example.com/sb",
		"https://dispatch.example.com:8443/sb?token=s3cr3t": "https://dispatch.example.com:8443/sb?redacted",
		"https://dispatch.example.com/sb?":                  "https://dispatch.example.com/sb?redacted",
		"https://u:p@dispatch.example.com/sb#frag":          "https://dispatch.example.com/sb",
		"not a url": "(unparseable url)",
	} {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

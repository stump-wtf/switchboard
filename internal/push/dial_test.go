package push

import (
	"errors"
	"testing"
)

// CheckDialAddress refuses every range Validate refuses, accepts a public literal, and never
// resolves a name. Governing: SPEC-0033 REQ "Owned Replay Targets".
func TestCheckDialAddress(t *testing.T) {
	v := New(WithResolver(&fakeResolver{err: errors.New("the dial check must not resolve")}),
		WithOwnListenAddrs("198.51.100.9:8080"))
	for _, addr := range []string{
		"127.0.0.1:80", "[::1]:443", "10.0.0.5:443", "192.168.1.1:80", "169.254.169.254:80",
		"[fe80::1]:443", "0.0.0.0:80", "100.64.0.1:443", "[::ffff:127.0.0.1]:80",
		"198.51.100.9:8080", // switchboard's own listen address
		"example.com:443",   // a name, not an address
		"93.184.216.34",     // no port
	} {
		if err := v.CheckDialAddress(addr); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: want refusal, got %v", addr, err)
		}
	}
	for _, addr := range []string{"93.184.216.34:443", "[2606:2800:220:1::1]:80", "93.184.216.34:8081"} {
		if err := v.CheckDialAddress(addr); err != nil {
			t.Errorf("%s: public address refused: %v", addr, err)
		}
	}
}

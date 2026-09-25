package push

// The dial-time half of the SSRF guard. Validate checks a URL and every address its host resolves
// to; a caller that then dials needs the same answer for the one concrete "ip:port" it is about to
// connect to (a net.Dialer Control hook, or a dialer pinned to addresses it resolved itself), so a
// DNS answer that changed after Validate cannot reach a range the guard refuses.
//
// Governing: ADR-0029 (one shared SSRF guard for every outbound call), ADR-0038, SPEC-0033 REQ
// "Owned Replay Targets" (no tenant call may bypass the SSRF validator).

import (
	"context"
	"fmt"
	"net"
)

// CheckDialAddress applies the address rules Validate applies to a resolved address to one concrete
// dial address in "host:port" form, where host MUST be an IP literal: the dial-time check is for
// addresses, never names, so a hostname is refused rather than resolved again. The result wraps
// ErrValidation on refusal, and a dialer MUST NOT connect on a non-nil error.
func (v *Validator) CheckDialAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: dial address %q: %v", ErrValidation, address, err)
	}
	if net.ParseIP(host) == nil {
		return fmt.Errorf("%w: dial address %q is not an IP literal", ErrValidation, address)
	}
	// An IP-literal URL takes Validate's literal branch: no DNS, the same address (and port) rules.
	// The scheme is fixed to https because the scheme rule was settled when the URL was validated.
	return v.Validate(context.Background(), "https://"+net.JoinHostPort(host, port)+"/")
}

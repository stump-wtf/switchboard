package push

// A caller-facing view of an SSRF-guard refusal. Validate's error carries the detail an operator
// needs in a server log: the address a host resolved to, or the resolver's own error text (which on
// Linux names the resolver's address). That detail is the server's view of its own network, not the
// caller's input, so returning it to a tenant would let anyone holding a guarded verb map internal
// DNS one name at a time. PublicReason reduces a refusal to its class, and every tenant-facing
// caller returns that instead of err.Error().
//
// Governing: ADR-0038, SPEC-0033 REQ "Owned Replay Targets" (audit F9); SPEC-0019 REQ "Webhook
// Target Validation (SSRF Guard)".

import (
	"errors"
	"strings"
)

// Refusal classes: fixed, caller-safe phrases. None of them names a resolved address or quotes a
// resolver error.
const (
	// RefusedMalformed is a URL that does not parse or names no host.
	RefusedMalformed = "is not an absolute https URL"
	// RefusedScheme is a scheme the guard does not permit (https, or http under the operator opt-in).
	RefusedScheme = "must use https"
	// RefusedUnresolvable is a host whose lookup failed or returned no addresses.
	RefusedUnresolvable = "host could not be resolved"
	// RefusedAddress is a host (or IP literal) that resolves to an address the guard refuses, where
	// the classifier named no more specific class.
	RefusedAddress = "resolves to a disallowed address"
)

// addressClassPrefix lends these phrases to an address-class refusal. "resolves to a private
// address" names the class and nothing else, which is what a caller-facing surface wants: the class
// is the caller's own input's shape, never the address it resolved to (SPEC-0024 REQ-3, SPEC-0033
// REQ "Owned Replay Targets"). Each phrase is a disallowedReason return, which is fixed prose and
// never carries an address.
const addressClassPrefix = "resolves to "

// refusal is a Validate rejection: class is the caller-safe phrase, detail the log-only text. Its
// Error() text is exactly what the guard returned before classes existed, "<ErrValidation>: <detail>".
type refusal struct {
	class  string
	detail string
}

func (e *refusal) Error() string { return ErrValidation.Error() + ": " + e.detail }
func (e *refusal) Unwrap() error { return ErrValidation }

// PublicReason returns the caller-safe class of an SSRF-guard refusal: one of the Refused* phrases,
// never the resolved address or the resolver's error. An error the guard did not classify (including
// one that is not a refusal at all) reads as RefusedAddress, the most conservative phrase.
func PublicReason(err error) string {
	var r *refusal
	if !errors.As(err, &r) {
		return RefusedAddress
	}
	// An address refusal carries the classifier's own fixed phrase as its detail ("a private
	// address"), which is a class and not an address. Lending it through names the class precisely
	// rather than collapsing every one of them to the generic phrase above.
	if r.class == RefusedAddress {
		if class, ok := strings.CutPrefix(r.detail, "address "); ok {
			if _, reason, found := strings.Cut(class, " is "); found && reason != "" {
				return addressClassPrefix + reason
			}
		}
	}
	return r.class
}

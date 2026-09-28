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

import "errors"

// Refusal classes: fixed, caller-safe phrases. None of them names a resolved address or quotes a
// resolver error.
const (
	// RefusedMalformed is a URL that does not parse or names no host.
	RefusedMalformed = "is not an absolute https URL"
	// RefusedScheme is a scheme the guard does not permit (https, or http under the operator opt-in).
	RefusedScheme = "must use https"
	// RefusedUnresolvable is a host whose lookup failed or returned no addresses.
	RefusedUnresolvable = "host could not be resolved"
	// RefusedAddress is a host (or IP literal) that resolves to an address the guard refuses.
	RefusedAddress = "resolves to a disallowed address"
)

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
	if errors.As(err, &r) {
		return r.class
	}
	return RefusedAddress
}

// Package sourcekind is SPEC-0032 REQ-1's per-source descriptor table: for every signed source type
// switchboard knows, one row states the trust mode it verifies under, how its signing secret
// originates, the replay window its signed timestamp implies, and the format check a caller-supplied
// secret must pass.
//
// It lives in its own package because the three surfaces that need the answer are not in a
// dependency chain: the MCP verb layer (create_webhook decides whether a supplied secret is legal),
// the store (which column a new row is written with), and the operator-facing lists. Reading the same
// map everywhere is what keeps "who mints this secret" from drifting between them: adding a provider
// is one row here plus a verifier.
//
// Governing: SPEC-0032 REQ-1 (secret origin per source type, fixed in code), REQ-3 (the format check),
// design "A secret-origin table beside the trust-mode table"; ADR-0037.
package sourcekind

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// Trust modes (SPEC-0003, ADR-0003): signed webhooks are HMAC-verified against a stored secret;
// token webhooks are authenticated by their unguessable ingest URL and hold no secret at all.
const (
	TrustSigned = "signed"
	TrustToken  = "token"
)

// Secret origins (SPEC-0032 REQ-1). minted: switchboard generates the secret and reveals it once, so
// the producer pastes a value switchboard chose. provider: the producer signs with a secret IT chose
// (Stripe's whsec_…), so switchboard must be told it — until it is, the webhook awaits its secret.
// none: the source needs no secret.
const (
	OriginMinted   = "minted"
	OriginProvider = "provider"
	OriginNone     = "none"
)

// Secret length bounds every supplied secret must satisfy, provider-issued or not (SPEC-0032 REQ-3).
// The floor is what makes a secret brute-forceable-but-not-trivial; the ceiling keeps a hostile or
// mistaken caller from storing a megabyte in the credential column.
const (
	MinSecretBytes = 16
	MaxSecretBytes = 512
)

// ErrSecretMalformed is the sentinel behind a supplied secret that fails the length, printability or
// per-kind format rule. Callers wrap it with the field name so the client-safe message names
// signing_secret without ever quoting the value. Governing: SPEC-0032 REQ-3 scenario "Malformed
// Stripe secret".
var ErrSecretMalformed = errors.New("sourcekind: signing_secret is malformed")

// Kind is one row of the table.
type Kind struct {
	// TrustMode is how switchboard verifies a delivery from this source.
	TrustMode string
	// Origin is how the webhook's signing secret comes to exist.
	Origin string
	// ReplayWindow is how long a signed timestamp stays valid; 0 means the scheme carries no signed
	// timestamp, so there is nothing to replay-bound. SPEC-0032 REQ-6 fills the per-scheme values.
	ReplayWindow time.Duration
	// FormatCheck reports whether a caller-supplied secret has the shape this provider issues. Nil
	// means the length and printability rules alone apply.
	FormatCheck func(string) error
	// Selectable marks a type create_webhook accepts today, because switchboard can verify its
	// deliveries end to end. A declared-but-unselectable row has a fixed origin and a missing
	// verifier: SPEC-0032 REQ-10 (the Linear and Plain kinds) flips it when their verifiers land.
	Selectable bool
}

// kinds is SPEC-0032 REQ-1's table. The row order is the spec's; the comments record why a row says
// what it does.
var kinds = map[string]Kind{
	"github":  {TrustMode: TrustSigned, Origin: OriginMinted, Selectable: true},
	"gitea":   {TrustMode: TrustSigned, Origin: OriginMinted, Selectable: true},
	"cairn":   {TrustMode: TrustSigned, Origin: OriginMinted, Selectable: true},
	"stripe":  {TrustMode: TrustSigned, Origin: OriginProvider, FormatCheck: checkStripe, Selectable: true},
	"slack":   {TrustMode: TrustSigned, Origin: OriginProvider, Selectable: true},
	"linear":  {TrustMode: TrustSigned, Origin: OriginProvider},
	"plain":   {TrustMode: TrustSigned, Origin: OriginProvider},
	"generic": {TrustMode: TrustToken, Origin: OriginNone, Selectable: true},
	// telemetry is token like generic (origin none); it predates the spec's table, which names only
	// generic, and keeps its current behavior.
	"telemetry": {TrustMode: TrustToken, Origin: OriginNone, Selectable: true},
}

// checkStripe enforces the provider's own prefix: Stripe signing secrets are `whsec_` plus base64. A
// value without it is a paste error, and catching it here beats a webhook that fails every delivery
// with 401. Governing: SPEC-0032 REQ-3 (per-kind format check).
func checkStripe(secret string) error {
	if !strings.HasPrefix(secret, "whsec_") {
		return fmt.Errorf("%w: a stripe signing_secret starts with whsec_", ErrSecretMalformed)
	}
	return nil
}

// Lookup returns the descriptor for a source type, and whether the type is known at all. An unknown
// type has no row: callers refuse it rather than guess a trust mode for it.
func Lookup(sourceType string) (Kind, bool) {
	k, ok := kinds[sourceType]
	return k, ok
}

// Selectable returns the source types create_webhook accepts, sorted, as a fresh slice the caller may
// keep. The vend wizard offers exactly these as its source-type chips, so a type made selectable
// here is pickable in the ceiling step without a second, drift-prone list.
func Selectable() []string {
	out := make([]string, 0, len(kinds))
	for name, k := range kinds {
		if k.Selectable {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Names returns every declared source type, selectable or not, sorted. Governing: SPEC-0032 REQ-1.
func Names() []string { return slices.Sorted(maps.Keys(kinds)) }

// OriginOf reports how a webhook's signing secret originates. A source type with no row (a ceiling
// that names something switchboard never learned) falls back to the trust mode's rule: a signed
// webhook holds a switchboard-minted secret, anything else holds none. That keeps an unmapped type
// behaving exactly as it did before this table existed.
func OriginOf(sourceType, trustMode string) string {
	if k, ok := kinds[sourceType]; ok {
		return k.Origin
	}
	if trustMode == TrustSigned {
		return OriginMinted
	}
	return OriginNone
}

// CheckSecret validates a caller-supplied signing secret: within the length bounds, printable ASCII,
// and past the source's own format check. The message names the field, never the value — an error
// that quotes a secret has leaked it through the response, the log and the trace at once.
//
// Governing: SPEC-0032 REQ-3 (at least 16 and at most 512 bytes of printable ASCII, per-kind format),
// REQ-3 ("MUST NOT appear in any response … or error message").
func CheckSecret(sourceType, secret string) error {
	if n := len(secret); n < MinSecretBytes || n > MaxSecretBytes {
		return fmt.Errorf("%w: signing_secret must be between %d and %d bytes (got %d)",
			ErrSecretMalformed, MinSecretBytes, MaxSecretBytes, n)
	}
	for i := 0; i < len(secret); i++ {
		if secret[i] < 0x20 || secret[i] > 0x7E {
			return fmt.Errorf("%w: signing_secret must be printable ASCII", ErrSecretMalformed)
		}
	}
	if k, ok := kinds[sourceType]; ok && k.FormatCheck != nil {
		if err := k.FormatCheck(secret); err != nil {
			return err
		}
	}
	return nil
}

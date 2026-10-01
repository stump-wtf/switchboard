// Package lease holds the claim-lease bounds every agent-facing claim path shares, and the one rule
// that picks a claim's lease: an explicit per-call lease_ttl_seconds, else the endpoint's own
// default lease, else the server default.
//
// The MCP verbs (claim, claim_next, heartbeat), the store, the human API, the web UI and the CLI
// all read the bounds from here, so the 60-second floor, the 300-second server default and the
// 86400-second cap are typed once. The endpoints.default_lease_ttl_seconds CHECK constraint
// (migration 0029) restates the bounds in SQL; a store test pins it to these constants.
//
// Governing: ADR-0043 (per-endpoint default claim lease), SPEC-0006 REQ "Lease Lifecycle and Crash
// Safety", SPEC-0007 REQ "Endpoint Default Lease".
//
// @joestump-agent 10/01/2026 - Added. The bounds lived as unexported constants in internal/mcp; a
// per-endpoint default needs them in the store, the API, the web UI and the CLI as well.
package lease

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultTTL is the server default: the lease a claim gets when neither the call nor its
	// endpoint names one.
	DefaultTTL = 5 * time.Minute
	// MaxTTL caps every lease, per-call or endpoint default, so a typo cannot park a todo for a
	// year.
	MaxTTL = 24 * time.Hour
	// MinEndpointDefault is the shortest default lease an endpoint may be given. A per-call
	// lease_ttl_seconds may still be shorter: the floor guards the setting that applies to every
	// claim an endpoint makes, not one worker's deliberate choice.
	MinEndpointDefault = time.Minute
)

// The bounds in the whole seconds every surface speaks (lease_ttl_seconds,
// default_lease_ttl_seconds).
const (
	DefaultSeconds    = int(DefaultTTL / time.Second)
	MaxSeconds        = int(MaxTTL / time.Second)
	MinDefaultSeconds = int(MinEndpointDefault / time.Second)
)

// ErrOutOfRange is returned for an endpoint default outside [MinDefaultSeconds, MaxSeconds]. It is
// refused, never clamped: an operator who typed 90000 should hear about it, not get 86400.
var ErrOutOfRange = errors.New("lease: default lease out of range")

// ValidateDefault checks an endpoint default lease in seconds against the bounds.
func ValidateDefault(seconds int) error {
	if seconds < MinDefaultSeconds || seconds > MaxSeconds {
		return fmt.Errorf("%w: default_lease_ttl_seconds must be between %d and %d seconds, got %d",
			ErrOutOfRange, MinDefaultSeconds, MaxSeconds, seconds)
	}
	return nil
}

// EffectiveSeconds is the lease, in seconds, a claim or heartbeat that names no lease_ttl_seconds
// gets: the endpoint's default when it has one, else the server default.
func EffectiveSeconds(endpointDefault *int) int {
	return int(TTL(0, endpointDefault) / time.Second)
}

// TTL resolves one claim's or heartbeat's lease. An explicit per-call value wins and is clamped to
// MaxTTL as it always has been; without one the endpoint's default applies; without that, the
// server default. A stored default outside the bounds cannot exist (the column's CHECK refuses it),
// but it is clamped the same way rather than trusted.
func TTL(perCallSeconds int, endpointDefault *int) time.Duration {
	seconds := perCallSeconds
	if seconds <= 0 && endpointDefault != nil {
		seconds = *endpointDefault
	}
	if seconds <= 0 {
		return DefaultTTL
	}
	d := time.Duration(seconds) * time.Second
	if d > MaxTTL {
		return MaxTTL
	}
	return d
}

// ParseSeconds reads an operator-typed lease into whole seconds: bare seconds ("3600") or a Go
// duration ("45m", "1h", "90s"). It does not range-check; the server owns the bounds, so the CLI
// and the web UI report the same refusal. A fractional second, an empty string and anything
// unparseable are errors.
func ParseSeconds(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("lease: empty duration")
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("lease: %q is neither whole seconds nor a duration like 45m or 1h", s)
	}
	if d%time.Second != 0 {
		return 0, fmt.Errorf("lease: %q is not a whole number of seconds", s)
	}
	return int(d / time.Second), nil
}

// Label renders a lease for people: "1h (3600s)", "45m (2700s)", "90s".
func Label(seconds int) string {
	switch {
	case seconds > 0 && seconds%3600 == 0:
		return fmt.Sprintf("%dh (%ds)", seconds/3600, seconds)
	case seconds > 0 && seconds%60 == 0:
		return fmt.Sprintf("%dm (%ds)", seconds/60, seconds)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}

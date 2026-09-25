package server

// Owned replay targets on the operator API: a vend may name the replay destinations the new
// endpoint owns. They are part of the endpoint's immutable vend-time scope (SPEC-0007: a changed
// scope is a new endpoint), the first is the default when a replay names no target, and each one is
// checked by the shared SSRF guard here — fail fast, before anything is minted — and again by
// replay_webhook_event at call and dial time, because ownership never exempts a target from it.
//
// Governing: ADR-0038, SPEC-0033 REQ "Owned Replay Targets" (audit F9); ADR-0029 (shared guard).

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/stump-wtf/switchboard/internal/push"
)

const (
	// maxReplayTargets bounds how many replay targets one endpoint may own.
	maxReplayTargets = 10
	// maxReplayTargetLen bounds one target URL.
	maxReplayTargetLen = 2048
)

// normalizeReplayTargets trims, drops blanks, de-duplicates (keeping first-seen order, since the
// first is the default) and validates every target with v. The error text describes the caller's
// own input and is safe to return to them.
func normalizeReplayTargets(ctx context.Context, v *push.Validator, in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		t := strings.TrimSpace(raw)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		if len(out) == maxReplayTargets {
			// Checked before the next target is validated, so an oversized list costs at most
			// maxReplayTargets lookups.
			return nil, fmt.Errorf("at most %d replay targets", maxReplayTargets)
		}
		if len(t) > maxReplayTargetLen {
			return nil, fmt.Errorf("replay target is longer than %d characters", maxReplayTargetLen)
		}
		if u, err := url.Parse(t); err != nil || !u.IsAbs() || u.Host == "" {
			return nil, fmt.Errorf("replay target %q must be an absolute https URL", t)
		}
		if err := v.Validate(ctx, t); err != nil {
			return nil, fmt.Errorf("replay target %q refused by the SSRF guard: %s", t,
				strings.TrimPrefix(err.Error(), push.ErrValidation.Error()+": "))
		}
		out = append(out, t)
	}
	return out, nil
}

// nonNilStrings renders an absent list as [] rather than null in API JSON.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

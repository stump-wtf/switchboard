// Package vend holds the rules a vended endpoint's scope must obey, away from both surfaces that
// apply them: the web wizard (internal/web) and the scripted vend (POST /api/v1/endpoints, driven by
// `switchboard endpoint vend`).
//
// Scope is chosen once and never edited afterwards (SPEC-0007), which is what makes these rules
// worth one home rather than two: an endpoint vended by a script that accepted a scope the wizard
// would have refused is a permanent artifact carrying the looser rule, and the only repair is to
// revoke it and vend again. The wizard's validation is the source of truth for a legal scope; this
// package is that validation in the place both callers can reach, so the wizard and the CLI cannot
// drift apart on what a lifetime, a queue name or a source type may be.
//
// What lives here is only what both surfaces decide alike. The wizard's step-by-step form state,
// drafts and reveal stay in internal/web; the API's JSON body shape stays in internal/server.
//
// Governing: SPEC-0007 (vend-time scope is immutable), SPEC-0016 REQ "Credential Lifetime",
// SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling", SPEC-0026 REQ-6 (the reserved
// queue name), SPEC-0035 REQ "Shared Implementation With MCP".
//
// @joestump-agent 10/22/2026 - Added for stump.wtf/switchboard#584, so a scripted vend validates a
// scope exactly as the wizard does instead of trusting whatever a script sent.
package vend

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/stump-wtf/switchboard/internal/sourcekind"
	"github.com/stump-wtf/switchboard/internal/store"
)

// ErrInvalidScope says a scope field is not legal. Callers render it in their own error shape: the
// wizard re-shows its step, the API answers SPEC-0035's JSON error.
var ErrInvalidScope = errors.New("vend: invalid scope")

// ParseLifetime reads a credential lifetime: a Go duration (90m, 24h, 1h30m) or a whole count of a
// coarse unit (7d, 4w), which is how the wizard's lifetime step words its choices. An empty string,
// zero and a negative are all refused: a lifetime that has already passed would mint a credential
// dead on arrival, and "valid until revoked" is expressed by not sending a lifetime at all, never by
// sending an empty one.
//
// Governing: SPEC-0016 REQ "Credential Lifetime".
func ParseLifetime(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errField(ErrInvalidScope, "lifetime must not be empty: omit it to vend a credential that never expires")
	}
	var (
		d   time.Duration
		err error
	)
	switch {
	case strings.HasSuffix(s, "d"), strings.HasSuffix(s, "w"):
		unit := 24 * time.Hour
		count := strings.TrimSuffix(s, "d")
		if strings.HasSuffix(s, "w") {
			unit, count = 7*24*time.Hour, strings.TrimSuffix(s, "w")
		}
		var n int
		if n, err = strconv.Atoi(strings.TrimSpace(count)); err == nil {
			d = time.Duration(n) * unit
		}
	default:
		d, err = time.ParseDuration(s)
	}
	if err != nil {
		return 0, errField(ErrInvalidScope, "lifetime \""+s+"\": use a duration like 90m, 24h, 7d, or 4w")
	}
	if d <= 0 {
		return 0, errField(ErrInvalidScope, "lifetime must be positive")
	}
	return d, nil
}

// CheckQueues validates queue names an endpoint drains: at least one, none reserved, none blank. A
// reserved name is refused by name because the operator needs to know which one to rename.
//
// Governing: SPEC-0026 REQ-6.
func CheckQueues(queues []string) error {
	if len(queues) == 0 {
		return errField(ErrInvalidScope, "at least one queue is required")
	}
	for _, q := range queues {
		if strings.TrimSpace(q) == "" {
			return errField(ErrInvalidScope, "a queue name must not be empty")
		}
		if err := store.CheckQueueNames(strings.TrimSpace(q)); err != nil {
			return errField(ErrInvalidScope, err.Error())
		}
	}
	return nil
}

// CheckCeiling validates the webhook ceiling a vend grants: the source types must be ones
// switchboard can verify today, the webhook queues must be legal names, and the count must not be
// negative. It checks nothing about the verbs, because the verb grant has its own shared resolver
// (mcp.VendVerbGrant) that both surfaces already run.
//
// An empty source-type list is legal and means "no self-managed webhooks of any kind", which is the
// wizard leaving the webhooks step blank. Governing: SPEC-0006 REQ "Webhook Self-Management Within a
// Vended Ceiling", SPEC-0026 REQ-6.
func CheckCeiling(sourceTypes, webhookQueues []string, webhookMax int) error {
	for _, s := range sourceTypes {
		s = strings.TrimSpace(s)
		if s == "" {
			return errField(ErrInvalidScope, "a source type must not be empty")
		}
		kind, ok := sourcekind.Lookup(s)
		if !ok || !kind.Selectable {
			return errField(ErrInvalidScope, "source type "+s+" is not supported: switchboard cannot verify its deliveries")
		}
	}
	for _, q := range webhookQueues {
		if strings.TrimSpace(q) == "" {
			return errField(ErrInvalidScope, "an allowed webhook queue must not be empty")
		}
		if err := store.CheckQueueNames(strings.TrimSpace(q)); err != nil {
			return errField(ErrInvalidScope, err.Error())
		}
	}
	if webhookMax < 0 {
		return errField(ErrInvalidScope, "webhook_max must not be negative")
	}
	return nil
}

// errField keeps one sentence attached to the sentinel so a surface can show it and still match on
// ErrInvalidScope.
func errField(_ error, msg string) error {
	return &fieldError{msg: msg}
}

type fieldError struct{ msg string }

func (e *fieldError) Error() string { return e.msg }
func (e *fieldError) Is(target error) bool {
	return target == ErrInvalidScope
}

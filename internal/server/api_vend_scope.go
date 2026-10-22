package server

// The webhook ceiling a scripted vend grants, resolved in one place so the operator API and the web
// wizard hand out the same scope for the same choices.
//
// A vend that names no ceiling field keeps the basics ceiling the one-step quick vend grants: the
// endpoint gets one generic webhook routed to its own queue when the verb grant carries
// create_webhook, and no webhook scope at all when it does not. Naming any ceiling field switches
// the request to a named ceiling, where the fields left out keep those same basics defaults. That
// way `--source-types github` widens what the agent may create rather than silently dropping the
// webhook it would otherwise have gotten, and the only way to vend an endpoint with no webhook is to
// say so: webhook_max 0.
//
// Every named value is checked by internal/vend, the rules the wizard applies before it mints, so a
// script cannot vend a scope the wizard would refuse. Scope is immutable after vend (SPEC-0007), so
// a looser rule accepted here is a permanent artifact.
//
// Governing: SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling", SPEC-0007 (immutable
// vend-time scope), SPEC-0016 REQ "Credential Lifetime", ADR-0012, ADR-0023.
//
// @joestump-agent 10/22/2026 - Added for stump.wtf/switchboard#584, so a scripted vend can grant a
// router endpoint's scope instead of only the basics scope.

import (
	"slices"
	"strings"

	"github.com/stump-wtf/switchboard/internal/mcp"
	"github.com/stump-wtf/switchboard/internal/vend"
)

// vendCeiling is a resolved webhook ceiling: what the endpoint may create, and whether the vend also
// mints its own ingestion webhook.
type vendCeiling struct {
	Max         int
	SourceTypes []string
	Queues      []string
	// AutoWebhook says the vend mints the endpoint's own ingestion webhook. It is false only when the
	// operator named a ceiling with webhook_max 0, which reads as "MCP only".
	AutoWebhook bool
}

// resolveVendCeiling turns the request's ceiling fields into the ceiling to store, refusing an
// illegal one before anything is minted. verbs is the already-resolved verb grant, queue the queue
// the endpoint drains.
func resolveVendCeiling(in vendEndpointIn, verbs []string, queue string) (vendCeiling, error) {
	if in.SourceTypes == nil && in.AllowedQueues == nil && in.WebhookMax == nil {
		max, sourceTypes, queues := mcp.BasicWebhookCeiling(verbs, []string{queue})
		return vendCeiling{Max: max, SourceTypes: sourceTypes, Queues: queues, AutoWebhook: true}, nil
	}
	c := vendCeiling{
		Max:         mcp.BasicWebhookMax,
		SourceTypes: []string{"generic"},
		Queues:      []string{queue},
		AutoWebhook: true,
	}
	if in.WebhookMax != nil {
		c.Max = *in.WebhookMax
	}
	if in.SourceTypes != nil {
		c.SourceTypes = trimmedUnique(in.SourceTypes)
	}
	if in.AllowedQueues != nil {
		c.Queues = trimmedUnique(in.AllowedQueues)
	}
	if err := vend.CheckCeiling(c.SourceTypes, c.Queues, c.Max); err != nil {
		return vendCeiling{}, err
	}
	c.AutoWebhook = c.Max > 0
	return c, nil
}

// trimmedUnique drops blanks and repeats from a submitted list, keeping order, so a ceiling the
// wizard would have stored is the ceiling a script stores.
func trimmedUnique(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || slices.Contains(out, s) {
			continue
		}
		out = append(out, s)
	}
	return out
}

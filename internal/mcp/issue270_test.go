package mcp

// The issue #270 repro over the real store and a live MCP session: vending an endpoint for a queue
// (the CLI/API vend shape) must let the owner's webhook rules route to that queue. Pre-fix, the
// rule-verb grant was only the webhook-owning endpoint's vend-time webhook_queues ceiling, so the
// save was refused with "queue lane-s is not in the webhook owner's allowed webhook queues" and the
// documented difficulty-lane fan-out was impossible to configure. Post-fix, the store's routing read
// unites the owner's active endpoints' queues into WebhookQueues, and the same save succeeds.
//
// Governing: SPEC-0020 REQ "Rule Validation at Save Time", REQ "Isolation and Tenant Safety"; ADR-0008.
//
// @joestump-agent 09/18/2026 - Regression test for issue #270 (vended endpoints cannot be routed to).

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/cred"
	"github.com/stump-wtf/switchboard/internal/store"
)

// vendCLI mirrors what POST /api/v1/endpoints does for `switchboard endpoint vend NAME --queue Q`:
// a fresh agent, full drain scope, a one-webhook ceiling, one source type, and a one-queue
// webhook-queue grant. It returns the vended endpoint and its bearer token.
func vendCLI(t *testing.T, ctx context.Context, st *store.Store, humanID, name, slug, queue string) (store.Endpoint, string) {
	t.Helper()
	token, hash, prefix, err := cred.Mint()
	if err != nil {
		t.Fatalf("mint credential: %v", err)
	}
	res, err := st.VendAgentEndpoint(ctx, store.VendParams{
		OwnerHumanID: humanID, Name: name,
		CredHash: hash, CredPrefix: prefix, Slug: slug,
		Queues: []string{queue}, Verbs: allRuleVerbs,
		WebhookMax:         5,
		WebhookSourceTypes: []string{"cairn"},
		WebhookQueues:      []string{queue},
	})
	if err != nil {
		t.Fatalf("vend %s: %v", name, err)
	}
	return res.Endpoint, token
}

func TestIssue270VendedQueuesEnterTheRuleVerbGrant(t *testing.T) {
	pool, ctx := routeTestPool(t)
	st := store.New(pool)
	human := mustHuman(t, ctx, st, "sub-270", "Human 270")

	// The router vend, and the lane vends, under ONE human. The lane endpoints have no webhook
	// self-management at all (the wizard's lane-pool shape): pre-fix their queues could never be
	// named by any rule on any webhook, because no webhook's grant contained them.
	router, token := vendCLI(t, ctx, st, human, "router-270", "router-270-00000001", "forge")
	laneS, _ := vendCLI(t, ctx, st, human, "pool-lane-s", "pool-lane-s-00000002", "lane-s")
	laneM, _ := vendCLI(t, ctx, st, human, "pool-lane-m", "pool-lane-m-00000003", "lane-m")

	wh, err := st.CreateWebhook(ctx, router.ID, "cairn", "forge", "signed", "tok-270", "whsec-270", 5)
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	cs := ruleSession(t, ctx, st, router.Slug, token)

	// The issue's exact rule shape, fanned out across two vended queues. Pre-fix this save was
	// refused with forbidden / "queue lane-s is not in the webhook owner's allowed webhook queues".
	rules := []any{
		map[string]any{
			"id": "handoff-lane-s", "name": "handoff pinned to lane:s",
			"expr":   `.source == "cairn" and any((.artifact.tags // [])[]; . == "lane:s")`,
			"action": map[string]any{"queue": "lane-s"},
		},
		map[string]any{
			"id": "handoff-lane-m", "name": "handoff pinned to lane:m",
			"expr":   `.source == "cairn" and any((.artifact.tags // [])[]; . == "lane:m")`,
			"action": map[string]any{"queue": "lane-m"},
		},
	}

	// A queue the owner has NOT vended anywhere must still be refused — the union follows the
	// endpoints the owner has; it is not a free-for-all.
	msg := callErr(t, ctx, cs, "set_webhook_rules", map[string]any{
		"webhook_id": wh.ID,
		"rules": append(slices.Clone[[]any](rules), map[string]any{
			"id": "hold", "name": "hold", "expr": `false`,
			"action": map[string]any{"queue": "hold"},
		}),
	}, codeForbidden)
	if !strings.Contains(msg, "hold") || !strings.Contains(msg, "allowed webhook queues") {
		t.Fatalf("unvended queue error %q does not name the grant boundary", msg)
	}

	var saved webhookRulesOut
	callOK(t, ctx, cs, "set_webhook_rules", map[string]any{
		"webhook_id": wh.ID, "rules": rules, "default_action": map[string]any{"queue": "forge"},
	}, &saved)
	if !slices.Equal(saved.Grant.Queues, []string{"forge", "lane-m", "lane-s"}) {
		t.Fatalf("grant queues = %v, want [forge lane-m lane-s]", saved.Grant.Queues)
	}

	// The dry-run the issue was refused on: a lane:s-tagged cairn payload now routes to lane-s.
	var res testWebhookRulesOut
	callOK(t, ctx, cs, "test_webhook_rules", map[string]any{
		"webhook_id": wh.ID,
		"payload": map[string]any{
			"source": "cairn", "kind": "artifact.created", "event_id": "evt-270",
			"data": map[string]any{"id": "S270", "share_type": "markdown", "tags": []string{"handoff", "lane:s"}},
		},
	}, &res)
	if res.Decision.Drop || res.Decision.Queue != "lane-s" {
		t.Fatalf("dry-run = %+v, want a lane-s decision", res.Decision)
	}

	// Revoking a lane endpoint shrinks the owner grant: a new save naming its queue is refused
	// again, and the already-saved rule falls through at delivery (evaluation re-applies the
	// grant, SPEC-0020 REQ "Evaluation-Time Grant Enforcement").
	if err := st.RevokeEndpoint(ctx, laneS.ID, human); err != nil {
		t.Fatalf("revoke lane-s pool: %v", err)
	}
	msg = callErr(t, ctx, cs, "set_webhook_rules", map[string]any{
		"webhook_id": wh.ID, "rules": rules, "default_action": map[string]any{"queue": "forge"},
	}, codeForbidden)
	if !strings.Contains(msg, "lane-s") {
		t.Fatalf("post-revoke save error %q does not name lane-s", msg)
	}

	// The lane-m half survives revocation of the lane-s pool: a queue the owner still vends
	// remains routable (the fan-out degrades per-lane, not wholesale).
	callOK(t, ctx, cs, "test_webhook_rules", map[string]any{
		"webhook_id": wh.ID,
		"payload": map[string]any{
			"source": "cairn", "kind": "artifact.created", "event_id": "evt-270m",
			"data": map[string]any{"id": "S271", "share_type": "markdown", "tags": []string{"handoff", "lane:m"}},
		},
	}, &res)
	if res.Decision.Drop || res.Decision.Queue != "lane-m" {
		t.Fatalf("lane-m dry-run = %+v, want a lane-m decision", res.Decision)
	}
	_ = laneM
}

package ingest

// Issue #270: vending an endpoint for a queue did not let any webhook rule route to that queue,
// because the webhook-queue grant was only the owning endpoint's vend-time ceiling. The fix unites
// the owner's active endpoints' queues into the routing grant, so a rule can route a delivery to a
// queue another vended endpoint drains. This is the delivery-path half of the regression test; the
// save-path half (rule validation over MCP) lives in internal/mcp/webhook_lanes_db_test.go.
//
// Governing: SPEC-0020 REQ "Rule Validation at Save Time", REQ "Evaluation-Time Grant Enforcement".
//
// @joestump-agent 09/18/2026 - Regression test for the issue #270 grant union.

import (
	"fmt"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

// cairnTagsBody is a cairn artifact.created body whose data carries tags (the lane tags the
// difficulty-lane design routes on).
func cairnTagsBody(eventID string, tags ...string) string {
	quoted := ""
	for i, tag := range tags {
		if i > 0 {
			quoted += ","
		}
		quoted += fmt.Sprintf("%q", tag)
	}
	return fmt.Sprintf(`{"source":"cairn","kind":"artifact.created","event_id":%q,"created_at":%q,`+
		`"data":{"id":"i270","share_type":"markdown","title":"issue 270","url":"https://cairn.example/a/i270","tags":[%s]}}`,
		eventID, time.Now().UTC().Format(time.RFC3339Nano), quoted)
}

func TestIssue270DeliveryRoutesToVendedQueue(t *testing.T) {
	ing, _, pool, ctx, _ := testIngestDeps(t, Config{})
	ing.SetRouter(routing.InProcess{})
	st := store.New(pool)

	// Two vends under one human: the webhook owner drains "forge"; a second endpoint drains
	// "lane-s" — the difficulty-lane shape from issue #270. Pre-fix, the grant was only the
	// owner endpoint's webhook_queues (["forge"]), so a rule naming "lane-s" fell through to
	// the default with cause rule_not_granted. Post-fix the grant includes lane-s.
	h, owner, wh := seedWebhook(t, st, ctx, "cairn", "signed", "forge", "cairn-270", cairnSecret)
	laneS := secondEndpoint(t, ctx, st, h.ID, "lane-s", []string{"lane-s"})
	if err := st.AddWebhookRoute(ctx, wh.ID, laneS.ID, h.ID); err != nil {
		t.Fatalf("route lane-s pool: %v", err)
	}
	setRules(t, ctx, st, wh.ID, h.ID, routing.Config{Rules: []routing.Rule{
		{ID: "handoff-lane-s", Name: "handoff pinned to lane:s",
			Expr:   `.source == "cairn" and any((.artifact.tags // [])[]; . == "lane:s")`,
			Action: routing.Action{Queue: "lane-s", Endpoints: []string{laneS.ID}}},
	}})

	body := cairnTagsBody("evt-270-a", "handoff", "lane:s")
	rec := postSelfManaged(ing, "cairn-270", body, cairnHeaders(body, "evt-270-a"))
	r := routed(t, rec)
	if len(r.Todos) != 1 || r.Todos[0].Queue != "lane-s" || r.Todos[0].EndpointID != laneS.ID {
		t.Fatalf("lane:s delivery = %+v, want one todo on the lane-s pool", r)
	}

	// Revoking the lane-s endpoint shrinks the owner grant back to ["forge"]: the rule still
	// matches, but the queue is no longer granted, so the delivery takes the default (the
	// webhook's target queue on its own targets) instead of anywhere it no longer reaches.
	if err := st.RevokeEndpoint(ctx, laneS.ID, h.ID); err != nil {
		t.Fatalf("revoke lane-s pool: %v", err)
	}
	after := cairnTagsBody("evt-270-b", "handoff", "lane:s")
	rec = postSelfManaged(ing, "cairn-270", after, cairnHeaders(after, "evt-270-b"))
	r = routed(t, rec)
	if len(r.Todos) != 1 || r.Todos[0].Queue != "forge" || r.Todos[0].EndpointID != owner.ID {
		t.Fatalf("post-revoke lane:s delivery = %+v, want the default on the owner", r)
	}
}

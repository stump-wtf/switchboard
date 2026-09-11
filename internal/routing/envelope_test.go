package routing

// Tests for the routing envelope contract — the jq paths rule authors write against — including the
// cairn projection and the agent-handoff example rule set documented in SPEC-0020 and the routing
// guide. If one of these fails, a documented path moved.
//
// Governing: SPEC-0020 REQ "Deterministic Rule Evaluation" (the normalized event), cairn SPEC-0012
// REQ "Event Payload".

import (
	"context"
	"net/http"
	"slices"
	"testing"
)

const cairnBody = `{"source":"cairn","kind":"artifact.created","event_id":"0b6c1e7e-2f8e-4d59-9a57-5f1d1c9a3a10",
"created_at":"2026-09-11T12:00:00Z","data":{"id":"abc123","share_type":"markdown","title":"[handoff:joestump-agent-opus-pool] review the audit",
"url":"https://cairn.stump.wtf/a/abc123","channel":"mcp","model":"claude-opus-5","actor_id":"joestump","metadata":{"handoff_to":"forge"}}}`

func cairnEnvelope(body string) map[string]any {
	return Envelope(EnvelopeInput{
		Source: SourceCairn, Kind: "artifact.created", WebhookID: "wh-1", TrustMode: "signed", Verified: true,
		ContentType: "application/json", Headers: map[string]string{"X-Cairn-Event": "artifact.created", "X-Cairn-Signature": "«redacted»"},
		Body: []byte(body),
	})
}

// matches reports whether expr matches the envelope, via the real evaluator.
func matches(t *testing.T, env map[string]any, expr string) bool {
	t.Helper()
	if _, err := Compile(expr); err != nil {
		t.Fatalf("Compile(%q): %v", expr, err)
	}
	d := Evaluate(context.Background(), Config{Rules: []Rule{rule("probe", expr, Action{Queue: "forge"})}}, grant(), env)
	if len(d.Trace.Faults) > 0 {
		t.Fatalf("%s faulted: %+v", expr, d.Trace.Faults)
	}
	return d.Trace.Stage == StageRule
}

func TestEnvelopeCairnPaths(t *testing.T) {
	env := cairnEnvelope(cairnBody)
	for _, expr := range []string{
		`.source == "cairn"`,
		`.kind == "artifact.created"`,
		`.verified and .trust_mode == "signed"`,
		`.webhook_id == "wh-1"`,
		`.headers["x-cairn-event"] == "artifact.created"`,
		`.headers["x-cairn-signature"] == "«redacted»"`,
		`.artifact.id == "abc123"`,
		`.artifact.url == "https://cairn.stump.wtf/a/abc123"`,
		`.artifact.share_type == "markdown"`,
		`.artifact.channel == "mcp" and .artifact.model == "claude-opus-5" and .artifact.actor_id == "joestump"`,
		`.artifact.event_id == "0b6c1e7e-2f8e-4d59-9a57-5f1d1c9a3a10" and .artifact.created_at == "2026-09-11T12:00:00Z"`,
		`.artifact.metadata.handoff_to == "forge"`,
		`.artifact.tags == null and .artifact.expires_at == null`,
		`.payload.data.title | startswith("[handoff:")`,
		`.size > 0 and .content_type == "application/json"`,
	} {
		if !matches(t, env, expr) {
			t.Fatalf("documented path %s did not match the cairn envelope", expr)
		}
	}
}

func TestEnvelopeNonCairnAndNonJSON(t *testing.T) {
	env := Envelope(EnvelopeInput{Source: "gitea", Kind: "issues", TrustMode: "token", Body: []byte("not json {")})
	if !matches(t, env, `.payload == null and .artifact == null and .kind == "issues" and .verified == false`) {
		t.Fatalf("non-JSON gitea envelope = %v", env)
	}
	trailing := Envelope(EnvelopeInput{Source: "generic", Body: []byte(`{"a":1} {"b":2}`)})
	if trailing["payload"] != nil {
		t.Fatalf("two concatenated documents decoded as %v, want null", trailing["payload"])
	}
	big := Envelope(EnvelopeInput{Source: "generic", Body: []byte(`{"id":9007199254740993,"f":1.5}`)})
	if !matches(t, big, `.payload.id == 9007199254740993 and .payload.f == 1.5`) {
		t.Fatalf("large integer lost precision: %v", big["payload"])
	}
}

func TestEventKind(t *testing.T) {
	h := http.Header{}
	h.Set("X-Cairn-Event", "tampered")
	h.Set("X-Gitea-Event", "issues")
	if got := EventKind(SourceCairn, h.Get, []byte(cairnBody)); got != "artifact.created" {
		t.Fatalf("cairn kind = %q, want the signed body kind over the header", got)
	}
	if got := EventKind("gitea", h.Get, nil); got != "issues" {
		t.Fatalf("gitea kind = %q, want issues", got)
	}
	if got := EventKind("generic", http.Header{}.Get, []byte(`{}`)); got != "" {
		t.Fatalf("generic kind = %q, want empty", got)
	}
}

// The agent-handoff example rule set from SPEC-0020 / docs/guides/07-routing-rules.md, verbatim in
// spirit: a cairn paste addressed to a pool lands only on that pool's endpoint, review requests go
// to forge on every target, and everything else from cairn is dropped.
func TestHandoffExampleRuleSet(t *testing.T) {
	cfg := Config{
		Rules: []Rule{
			{ID: "handoff-opus", Name: "handoff to the opus pool", Expr: `.artifact.metadata.handoff_to == "opus-pool" or (.artifact.title // "" | startswith("[handoff:opus-pool]"))`,
				Action: Action{Queue: "handoff", Endpoints: []string{epC}}},
			{ID: "review", Name: "review requests", Expr: `.artifact.metadata.review_requested == true`, Action: Action{Queue: "forge"}},
		},
		Default: &Action{Drop: true},
	}
	if err := Validate(cfg, grant()); err != nil {
		t.Fatalf("example rule set does not validate: %v", err)
	}
	route := func(body string) Decision {
		return Evaluate(context.Background(), cfg, grant(), cairnEnvelope(body))
	}

	byTitle := route(`{"source":"cairn","kind":"artifact.created","data":{"id":"x","title":"[handoff:opus-pool] audit"}}`)
	byMeta := route(`{"source":"cairn","kind":"artifact.created","data":{"id":"x","metadata":{"handoff_to":"opus-pool"}}}`)
	for _, d := range []Decision{byTitle, byMeta} {
		if d.Queue != "handoff" || !slices.Equal(d.Endpoints, []string{epC}) || d.Trace.RuleID != "handoff-opus" {
			t.Fatalf("handoff = %+v, want queue handoff on only %s", d, epC)
		}
	}
	review := route(`{"source":"cairn","kind":"artifact.created","data":{"id":"x","metadata":{"review_requested":true}}}`)
	if review.Queue != "forge" || len(review.Endpoints) != 3 {
		t.Fatalf("review = %+v, want forge on every target", review)
	}
	noise := route(`{"source":"cairn","kind":"artifact.created","data":{"id":"x","title":"notes"}}`)
	if !noise.Drop || noise.Trace.Cause != CauseNoMatch {
		t.Fatalf("noise = %+v, want dropped by the default", noise)
	}
}

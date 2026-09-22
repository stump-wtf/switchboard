package web

import (
	"slices"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/mcp"
)

// TestVendStepWebhooksRendersAllFields pins two regressions on the webhooks
// step in one assertion set.
//
// First: the step must RENDER at all. The original gate around the pickers —
// `gt $v.WebhookMax 0` — compared the string-typed WebhookMax against an int,
// which html/template rejects at execution time ("incompatible types for
// comparison"), so every visit to the step was a 500 and the wizard was
// impassable at step 4. No test rendered this step, which is how it shipped.
//
// Second: all three fields must be present on the FIRST visit, before any
// state is saved. Gating the source-type and queue pickers on the
// previously-saved max meant a straight-through vend could set max>0 and
// advance past pickers it never saw, minting an endpoint with webhook_max>0
// and NO allowed source types — the exact forbidden_source_type dead end the
// webhook-ceiling feature exists to fix (issue #79).
func TestVendStepWebhooksRendersAllFields(t *testing.T) {
	h := newTestHandler(t)
	for _, max := range []string{"", "5"} {
		v := &vendStepView{
			Step: "webhooks", StepNum: 4, StepTotal: 6, Name: "reviewer-bot",
			BackURL:    "/endpoints/vend/verbs",
			WebhookMax: max,
			WebhookSourceTypes: []vendChipOption{
				{Name: "github"}, {Name: "generic"},
			},
		}
		body := renderVendPage(t, h, v, false)
		for _, want := range []string{
			`name="webhook_max"`,
			`name="webhook_source_types" value="github"`,
			`name="webhook_source_types" value="generic"`,
			`name="webhook_queues_extra"`,
			`href="/endpoints/vend/verbs" data-sb-wiz-back`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("WebhookMax=%q: webhooks step missing %q", max, want)
			}
		}
	}
}

// TestVendStepWebhooksOffersEveryAcceptedSourceType: the source-type chips come from
// mcp.WebhookSourceTypes, the list create_webhook accepts, rather than a copy kept here — a type
// added server-side is offered without a wizard edit. A revisit re-checks the saved choice.
// Governing: SPEC-0015 REQ "Endpoints View And Vend Wizard" (webhooks step), SPEC-0006 REQ
// "Webhook Self-Management Within a Vended Ceiling".
func TestVendStepWebhooksOffersEveryAcceptedSourceType(t *testing.T) {
	accepted := mcp.WebhookSourceTypes()
	opts := vendSourceTypeOptions([]string{"gitea"})
	var names []string
	for _, o := range opts {
		names = append(names, o.Name)
		if o.Checked != (o.Name == "gitea") {
			t.Errorf("chip %q Checked = %v, want only the saved gitea choice checked", o.Name, o.Checked)
		}
	}
	if !slices.Equal(names, accepted) {
		t.Fatalf("source-type chips = %v, want mcp.WebhookSourceTypes() = %v", names, accepted)
	}

	v := testVendStepView("webhooks")
	v.WebhookSourceTypes = opts
	body := renderVendPage(t, newTestHandler(t), v, false)
	for _, s := range accepted {
		want := `name="webhook_source_types" value="` + s + `"`
		if s == "gitea" {
			want += " checked"
		}
		if !strings.Contains(body, want) {
			t.Errorf("webhooks step missing %q", want)
		}
	}
}

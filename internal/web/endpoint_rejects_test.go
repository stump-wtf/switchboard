package web

// Endpoint Card Reject Chip Tests
//
// A delivery refused before intake leaves no row in the database, so the endpoint card is the only
// place an operator can see it (issue #612, ADR-0051). These tests pin the three states the row can
// be in: refusals counted for this webhook, nothing refused, and no honest number to quote (no
// reader wired, or the webhook folded into the metrics cardinality overflow bucket) — where the row
// must say nothing rather than invent a zero.

import (
	"strings"
	"testing"
)

func renderCards(t *testing.T, h *Handler, cards ...endpointCard) string {
	t.Helper()
	return renderPage(t, h, "endpoints", view{Title: "Endpoints", Human: testHuman(), CSRF: "tok",
		Shell: shell{Active: "endpoints", DBConnected: true, Initials: "JS"}, EndpointCards: cards})
}

func TestEndpointCardWebhookRejectChip(t *testing.T) {
	h := newTestHandler(t)
	body := renderCards(t, h, endpointCard{ID: "e1", AgentName: "bot", Initials: "BO", State: "active",
		Webhooks: []webhookSignalView{
			{ID: "wh-leak", ShortID: "wh-leak", Source: "gitea", TargetQueue: "triage",
				Rejected: 7, RejectedRateLimited: 6, RejectedKnown: true},
			{ID: "wh-clean", ShortID: "wh-clean", Source: "github", TargetQueue: "reviews", RejectedKnown: true},
			{ID: "wh-untracked", ShortID: "wh-untracked", Source: "generic", TargetQueue: "misc"},
		}})

	for _, want := range []string{
		`data-sb-webhook-rejected="7" data-sb-webhook-rejected-rate-limited="6"`,
		"⚠ 7 rejected (6 rate-limited)",
		// since this process started is stated on the chip: the ledger is in-memory by design.
		"since this process started",
		`data-sb-webhook-rejected="0">0 rejected`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("card missing %q", want)
		}
	}
	if strings.Contains(body, `data-sb-webhook="wh-untracked" data-sb-webhook-rejected`) {
		t.Error("an untracked webhook was given a number it does not own")
	}
	// The untracked row still renders its other signals: only the reject chip is withheld.
	if !strings.Contains(body, `data-sb-webhook="wh-untracked"`) {
		t.Error("the untracked webhook row disappeared entirely")
	}
}

func TestSetPreIntakeRejectReaderFeedsTheRow(t *testing.T) {
	h := newTestHandler(t)
	h.SetPreIntakeRejectReader(func(webhookID string) (int, int, bool) {
		if webhookID == "wh-1" {
			return 3, 1, true
		}
		return 0, 0, false
	})
	if h.preIntakeRejects == nil {
		t.Fatal("SetPreIntakeRejectReader stored nothing")
	}
	total, rateLimited, tracked := h.preIntakeRejects("wh-1")
	if total != 3 || rateLimited != 1 || !tracked {
		t.Errorf("reader = %d/%d/%v, want 3/1/true", total, rateLimited, tracked)
	}
	h.SetPreIntakeRejectReader(nil)
	if h.preIntakeRejects != nil {
		t.Error("passing nil must clear the reader, not keep a broken one")
	}
}

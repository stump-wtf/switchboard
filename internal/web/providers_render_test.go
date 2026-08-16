package web

// Template render coverage for the SPEC-0017 Providers view that runs in the
// `go test ./...` gate: family grouping, trust chips (scenario "Trust at a glance"), secret
// PRESENCE-only rendering, health stamps, the lifecycle affordances, the two-tier catalog
// (scenario "Catalog honesty" — available cards expose NO connect path), and the one-time rotate
// reveal. DOM assertions target data-sb-* attributes and ids, never classes alone.
//
// Governing: SPEC-0017 REQ "Providers View" / "Provider Catalog" / "Provider Lifecycle";
// ADR-0020, ADR-0018.

import (
	"strings"
	"testing"
	"time"

	"github.com/joestump/switchboard/internal/store"
)

// sampleAdapters covers every trust mode + both families: a signed webhook (configured), a token
// webhook (secret MISSING), an explicit open webhook, a disabled token webhook, and a queue pull
// adapter with a poll error.
func sampleAdapters() []store.Adapter {
	lastPoll := time.Now().Add(-30 * time.Second)
	pollErr := "consume: connection refused"
	return []store.Adapter{
		{Name: "github", Family: "webhook", Kind: "github", TrustMode: "signed", Enabled: true,
			SecretConfigured: true, Config: []byte(`{"queue":"ci"}`)},
		{Name: "homelab", Family: "webhook", Kind: "generic", TrustMode: "token", Enabled: true,
			SecretConfigured: false},
		{Name: "lan", Family: "webhook", Kind: "generic", TrustMode: "open", Enabled: true},
		{Name: "paused", Family: "webhook", Kind: "generic", TrustMode: "token", Enabled: false,
			SecretConfigured: true},
		{Name: "redis-builds", Family: "queue", Kind: "redis", TrustMode: "queue", Enabled: true,
			Config: []byte(`{"stream":"builds"}`), LastPollAt: &lastPoll, LastError: &pollErr},
	}
}

func providersView(h *Handler, adapters []store.Adapter, health map[string]store.ProviderHealth) view {
	panel := providersPanel(adapters, health, "tok")
	return view{
		Title: "Providers", Human: testHuman(), CSRF: "tok",
		Shell:     shell{Active: "providers", DBConnected: true, Initials: "JS"},
		Providers: &panel,
	}
}

// Scenario "Trust at a glance": every connected line shows its ENFORCED trust mode as a chip, and
// no secret material appears anywhere in the page — only presence classification.
func TestProvidersViewTrustChipsAndNoSecrets(t *testing.T) {
	h := newTestHandler(t)
	seen := time.Now().Add(-4 * time.Second)
	health := map[string]store.ProviderHealth{
		"github": {EventsPerMin: 42, LastSeenAt: &seen},
	}
	body := renderPage(t, h, "providers", providersView(h, sampleAdapters(), health))

	for _, want := range []string{
		// Family sections, in canvas order (webhook · push before queue · pull).
		`data-sb-provider-family="webhook"`, `data-sb-provider-family="queue"`,
		// One line per registry row, stable ids for OOB/anchor use.
		`id="sb-pr-github"`, `id="sb-pr-homelab"`, `id="sb-pr-lan"`, `id="sb-pr-paused"`, `id="sb-pr-redis-builds"`,
		// The enforced trust mode as a chip on every line (SPEC-0001 vocabulary).
		`data-sb-trust="signed"`, `data-sb-trust="token"`, `data-sb-trust="open"`, `data-sb-trust="queue"`,
		// Secret PRESENCE classification only.
		`data-sb-secret-status="configured"`, `data-sb-secret-status="missing"`, `data-sb-secret-status="none-by-design"`,
		// Health: per-line in-rate + last-seen from the events table.
		`data-sb-in-rate="42"`, "in 42/min", "seen just now",
		// Queue line health surfaces the credential-free poll error.
		"consume: connection refused",
		// Ingestion paths per trust mode.
		"/webhooks/github", "/webhooks/generic/homelab",
		// Kind → queue routing line.
		"github → ci", "redis → builds",
		// Disabled line is stamped, and its lifecycle offers enable.
		"data-sb-provider-disabled", `hx-post="/providers/paused/enable"`,
		// Configure affordance per line (SPEC-0017 REQ "Providers View").
		"data-sb-configure",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("providers page missing %q", want)
		}
	}

	// The render model cannot carry secret material (store.Adapter has no secret field); assert the
	// page also never renders a secret-looking affordance for the view itself.
	for _, forbid := range []string{"old-secret", "sb-prreveal-secret-label"} {
		if strings.Contains(body, forbid) {
			t.Errorf("providers page must not render %q — secrets never render on the view", forbid)
		}
	}

	// Every provider-line trust chip carries the shared one-line definition as its tooltip (#84).
	for mode, def := range trustDefs {
		if !strings.Contains(body, `title="`+def+`"`) {
			t.Errorf("providers page: trust chip %q missing tooltip %q", mode, def)
		}
	}
}

// SPEC-0017 REQ "Provider Lifecycle": enabled lines offer disable/rotate/remove behind the
// confirmation modal (rotate only where a rotatable secret exists); open and queue lines offer no
// rotate at all.
func TestProvidersViewLifecycleAffordances(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "providers", providersView(h, sampleAdapters(), nil))

	for _, want := range []string{
		`hx-get="/providers/github/confirm/disable"`,
		`hx-get="/providers/github/confirm/rotate"`,
		`hx-get="/providers/github/confirm/remove"`,
		`hx-get="/providers/homelab/confirm/rotate"`, // token kind rotates
	} {
		if !strings.Contains(body, want) {
			t.Errorf("providers page missing lifecycle affordance %q", want)
		}
	}
	for _, forbid := range []string{
		`hx-get="/providers/lan/confirm/rotate"`,          // open holds no secret
		`hx-get="/providers/redis-builds/confirm/rotate"`, // queue DSN lives in env
		`hx-post="/providers/github/enable"`,              // enabled line offers no enable
	} {
		if strings.Contains(body, forbid) {
			t.Errorf("providers page must not offer %q", forbid)
		}
	}
}

// Scenario "Catalog honesty": SQS/NATS/AMQP render as available-only cards — visually and
// structurally distinct, with NO form, link, or button (no wizard entry point that would
// dead-end). Implemented kinds render as connectable; a connected single-instance signed kind
// drops off the catalog.
func TestProvidersCatalogHonesty(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "providers", providersView(h, sampleAdapters(), nil))

	for _, kind := range []string{"sqs", "nats", "amqp"} {
		card := extractCatalogCard(t, body, kind)
		if !strings.Contains(card, `data-sb-catalog-state="available"`) {
			t.Errorf("%s card must be available-only: %q", kind, card)
		}
		for _, forbid := range []string{"<form", "<button", "<a ", "hx-get", "hx-post", "href="} {
			if strings.Contains(card, forbid) {
				t.Errorf("%s available card must expose no connect path, found %q in %q", kind, forbid, card)
			}
		}
		if !strings.Contains(card, "planned") {
			t.Errorf("%s card should read as planned: %q", kind, card)
		}
	}

	// Implemented-but-unconnected kinds read connectable; github is connected so it drops off.
	for _, kind := range []string{"stripe", "slack", "generic", "redis"} {
		card := extractCatalogCard(t, body, kind)
		if !strings.Contains(card, `data-sb-catalog-state="connectable"`) {
			t.Errorf("%s card must be connectable: %q", kind, card)
		}
	}
	if strings.Contains(body, `data-sb-catalog-kind="github"`) {
		t.Error("connected single-instance kind github must drop off the catalog")
	}
}

// extractCatalogCard slices one catalog card's markup out of the rendered page by its
// data-sb-catalog-kind attribute (article-delimited).
func extractCatalogCard(t *testing.T, body, kind string) string {
	t.Helper()
	marker := `data-sb-catalog-kind="` + kind + `"`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("catalog card %s not found", kind)
	}
	start := strings.LastIndex(body[:i], "<article")
	end := strings.Index(body[i:], "</article>")
	if start < 0 || end < 0 {
		t.Fatalf("catalog card %s not article-delimited", kind)
	}
	return body[start : i+end]
}

// The empty registry renders the explanatory empty state — plus the full catalog (static honesty,
// not registry state).
func TestProvidersViewEmptyState(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "providers", providersView(h, nil, nil))
	for _, want := range []string{
		"data-sb-providers-empty",
		`data-sb-catalog-kind="sqs"`,
		`data-sb-catalog-kind="github"`, // nothing connected → github is connectable again
	} {
		if !strings.Contains(body, want) {
			t.Errorf("empty providers page missing %q", want)
		}
	}
}

// The confirmation modal copy states consequences plainly; the remove copy states that history is
// never deleted (SPEC-0017 REQ "Provider Lifecycle" — removal requires confirmation).
func TestProviderConfirmModalCopy(t *testing.T) {
	h := newTestHandler(t)
	title, body, button, ok := providerConfirmCopy("remove", "doomed")
	if !ok {
		t.Fatal("remove must be a known confirm action")
	}
	frag, err := h.renderFragment("provider_confirm_modal", providerConfirmView{
		Action: "remove", Name: "doomed", Title: title, Body: body, Button: button, CSRF: "tok",
	})
	if err != nil {
		t.Fatalf("render confirm modal: %v", err)
	}
	for _, want := range []string{
		`data-sb-provider-confirm="remove"`,
		`hx-post="/providers/doomed/remove"`,
		"never deleted", // the modal states what removal does NOT do
		"data-sb-provider-confirm-submit",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("confirm modal missing %q in %q", want, frag)
		}
	}
	if _, _, _, ok := providerConfirmCopy("explode", "x"); ok {
		t.Error("unknown lifecycle action must not produce a confirmation")
	}
}

// The rotate reveal is the standard one-time pattern: the new secret renders in the reveal
// response (and only there), with the danger callout stating the old secret is dead.
func TestProviderRotateRevealRendersSecretOnce(t *testing.T) {
	h := newTestHandler(t)
	frag, err := h.renderFragment("provider_rotate_reveal", providerRevealView{
		Name: "homelab", TrustMode: "token", Secret: "s3cr3t-once", Path: "/webhooks/generic/homelab", CSRF: "tok",
	})
	if err != nil {
		t.Fatalf("render rotate reveal: %v", err)
	}
	for _, want := range []string{
		"data-sb-provider-reveal", "s3cr3t-once", "shown once", "/webhooks/generic/homelab",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("rotate reveal missing %q", want)
		}
	}
}

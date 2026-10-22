package mcp

// tools/call tests for SPEC-0032 over MCP: a provider-origin webhook is created awaiting its secret, a
// supplied signing secret is accepted only where the source table allows it, the operator opt-in is the
// only way a minted-origin type takes one, and no response ever carries a secret value.
//
// Governing: SPEC-0032 REQ-1, REQ-2, REQ-3, REQ-13.

import (
	"context"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/cred"
	"github.com/stump-wtf/switchboard/internal/store"
)

// secretSession is webhookSession's sibling that hands back the handler, so a test can flip the
// operator opt-in that SWITCHBOARD_WEBHOOK_ALLOW_SUPPLIED_SECRET sets.
func secretSession(t *testing.T, ctx context.Context, f *fakeStore, verbs []string, types []string) (*sdk.ClientSession, *Handler) {
	t.Helper()
	token, hash, _, err := cred.Mint()
	if err != nil {
		t.Fatalf("mint credential: %v", err)
	}
	f.byHash[hash] = store.AuthEndpoint{
		ID: "ep-secrets", AgentID: "ag-1", AgentName: "test-agent", OwnerHumanID: "h-1",
		Slug: "agent-a-11111111", ScopeQueues: []string{"reviews"}, ScopeVerbs: verbs,
		WebhookMax: 5, WebhookSourceTypes: types, WebhookQueues: []string{"reviews"},
	}
	ts, hdl := newTestServerHandler(t, f)
	hdl.SetBaseURL("https://switchboard.example")
	cs, err := connect(t, ctx, ts.URL+"/mcp/agent-a-11111111", token)
	if err != nil {
		t.Fatalf("initialize handshake: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, hdl
}

var secretCeilingTypes = []string{"github", "stripe", "slack", "generic", "linear"}

// TestProviderWebhookCreatedAwaitingSecret is REQ-2's acceptance scenario at the tool surface: creating
// a stripe webhook returns secret_origin provider with secret_set false, and no field in the result
// carries a secret.
func TestProviderWebhookCreatedAwaitingSecret(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newFakeStore()
	cs, _ := secretSession(t, ctx, f, strings.Fields(allWebhookVerbs), secretCeilingTypes)

	var out webhookOut
	callOK(t, ctx, cs, "create_webhook", map[string]any{"source_type": "stripe", "target_queue": "reviews"}, &out)
	if out.SecretOrigin != "provider" {
		t.Errorf("secret_origin = %q, want provider", out.SecretOrigin)
	}
	if out.SecretSet || out.SecretFingerprint != "" || out.SecretSetAt != "" {
		t.Errorf("a webhook with no secret reports one: %+v", out)
	}
	if out.SigningSecret != "" {
		t.Error("create revealed a secret switchboard never minted")
	}
	raw := strings.ToLower(rawJSONOf(t, callResult(t, ctx, cs, "create_webhook",
		map[string]any{"source_type": "slack", "target_queue": "reviews"})))
	if strings.Contains(raw, "signing_secret") || strings.Contains(raw, "whsec_") {
		t.Errorf("create result carries a secret field for a provider source: %s", raw)
	}
}

// TestSuppliedSecretRefusedByDefault is REQ-1's opt-in rule: github's secret is switchboard's to mint,
// so a create_webhook that hands one over is refused with invalid_argument before anything is written,
// and the message names the env var that would allow it.
func TestSuppliedSecretRefusedByDefault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newFakeStore()
	cs, _ := secretSession(t, ctx, f, strings.Fields(allWebhookVerbs), secretCeilingTypes)

	const secret = "whsec_NotAcceptedForAMintedType"
	msg := callErr(t, ctx, cs, "create_webhook", map[string]any{
		"source_type": "github", "target_queue": "reviews", "signing_secret": secret,
	}, "invalid_argument")
	if strings.Contains(msg, secret) {
		t.Errorf("error text echoes the secret: %s", msg)
	}
	if !strings.Contains(msg, "SWITCHBOARD_WEBHOOK_ALLOW_SUPPLIED_SECRET") {
		t.Errorf("refusal does not name the opt-in: %s", msg)
	}
	if n := countFakeWebhooks(t, ctx, cs); n != 0 {
		t.Errorf("refused create left %d webhooks", n)
	}
}

// TestSuppliedSecretNeedsOperatorOptIn: with the opt-in on, the same call succeeds and the webhook says
// so in list_webhooks, which is what tells an operator that rotate will replace that value.
func TestSuppliedSecretNeedsOperatorOptIn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newFakeStore()
	cs, hdl := secretSession(t, ctx, f, strings.Fields(allWebhookVerbs), secretCeilingTypes)
	hdl.SetAllowSuppliedSecret(true)

	callOK(t, ctx, cs, "create_webhook", map[string]any{
		"source_type": "github", "target_queue": "reviews", "signing_secret": "whsec_SuppliedUnderOptIn111",
	}, &webhookOut{})
	raw := rawJSONOf(t, callResult(t, ctx, cs, "list_webhooks", map[string]any{}))
	if !strings.Contains(raw, `"secret_supplied":true`) {
		t.Errorf("list does not mark the supplied secret: %s", raw)
	}
	if strings.Contains(raw, "whsec_SuppliedUnderOptIn111") {
		t.Errorf("list echoed the supplied secret: %s", raw)
	}
}

// TestSuppliedSecretRefusedForTokenSources: generic authenticates by its URL, so a signing_secret there
// is a mistake worth refusing rather than a value to store and ignore.
func TestSuppliedSecretRefusedForTokenSources(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newFakeStore()
	cs, hdl := secretSession(t, ctx, f, strings.Fields(allWebhookVerbs), secretCeilingTypes)
	hdl.SetAllowSuppliedSecret(true) // the opt-in is about minted types, not URL-authenticated ones

	msg := callErr(t, ctx, cs, "create_webhook", map[string]any{
		"source_type": "generic", "target_queue": "reviews", "signing_secret": "whsec_NoSecretForTokenTypes",
	}, "invalid_argument")
	if !strings.Contains(msg, "signing_secret") {
		t.Errorf("refusal should name the field: %s", msg)
	}
	if n := countFakeWebhooks(t, ctx, cs); n != 0 {
		t.Errorf("refused create left %d webhooks", n)
	}
}

// TestStripeSuppliedSecretIsFormatChecked is REQ-3's scenario "Malformed Stripe secret": the field is
// named, the value is not, and nothing is written.
//
// @joestump-agent 10/21/2026 - Reworded the malformed fixture so HEAD stops matching gitleaks'
// stripe-access-token rule (it was never a real credential), and allowlisted the historical value in
// .gitleaks.toml: the CI scan reads branch history, so the commit that introduced the old string
// stays flagged no matter what HEAD carries.
func TestStripeSuppliedSecretIsFormatChecked(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newFakeStore()
	cs, _ := secretSession(t, ctx, f, strings.Fields(allWebhookVerbs), secretCeilingTypes)

	const bad = "not-a-stripe-signing-secret"
	msg := callErr(t, ctx, cs, "create_webhook", map[string]any{
		"source_type": "stripe", "target_queue": "reviews", "signing_secret": bad,
	}, "invalid_argument")
	if strings.Contains(msg, bad) {
		t.Errorf("error text echoes the secret: %s", msg)
	}
	if !strings.Contains(msg, "signing_secret") {
		t.Errorf("error does not name the field: %s", msg)
	}
	if n := countFakeWebhooks(t, ctx, cs); n != 0 {
		t.Errorf("refused create left %d webhooks", n)
	}
}

// TestDeclaredButUnsupportedSourceType: linear has a row in the table (provider origin) but no verifier
// yet, so create_webhook refuses it even when the ceiling names it — REQ-10 flips that when its
// verifier lands.
func TestDeclaredButUnsupportedSourceType(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newFakeStore()
	cs, _ := secretSession(t, ctx, f, strings.Fields(allWebhookVerbs), secretCeilingTypes)

	callErr(t, ctx, cs, "create_webhook", map[string]any{"source_type": "linear", "target_queue": "reviews"},
		"invalid_argument")
	if n := countFakeWebhooks(t, ctx, cs); n != 0 {
		t.Errorf("an unverifiable source was created: %d webhooks", n)
	}
}

// TestListWebhooksReportsSecretState is REQ-2 and REQ-13's read side: every row states the origin,
// whether a secret exists, and for a written one the fingerprint, moment and actor — and no row carries
// the value.
func TestListWebhooksReportsSecretState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newFakeStore()
	cs, _ := secretSession(t, ctx, f, strings.Fields(allWebhookVerbs), secretCeilingTypes)

	const stripeSecret = "whsec_TheProvidersOwnKeyForUs1"
	callOK(t, ctx, cs, "create_webhook", map[string]any{
		"source_type": "stripe", "target_queue": "reviews", "signing_secret": stripeSecret,
	}, &webhookOut{})
	callOK(t, ctx, cs, "create_webhook", map[string]any{"source_type": "stripe", "target_queue": "reviews"}, &webhookOut{})

	res := callResult(t, ctx, cs, "list_webhooks", map[string]any{})
	var out listWebhooksOut
	rawUnmarshal(t, res, &out)
	if len(out.Webhooks) != 2 {
		t.Fatalf("webhooks = %d, want 2", len(out.Webhooks))
	}
	var set, awaiting int
	for _, w := range out.Webhooks {
		if w.SecretOrigin != "provider" {
			t.Errorf("secret_origin = %q, want provider", w.SecretOrigin)
		}
		if w.SecretSet {
			set++
			if w.SecretFingerprint == "" || w.SecretSetAt == "" {
				t.Errorf("a set secret reports no fingerprint/set_at: %+v", w)
			}
			if w.SecretSetBy == nil || w.SecretSetBy.Kind != "endpoint" || w.SecretSetBy.ID != "ep-secrets" {
				t.Errorf("secret_set_by = %+v, want the endpoint that wrote it", w.SecretSetBy)
			}
		} else {
			awaiting++
			if w.SecretFingerprint != "" || w.SecretSetAt != "" || w.SecretSetBy != nil {
				t.Errorf("a webhook awaiting its secret reports one: %+v", w)
			}
		}
	}
	if set != 1 || awaiting != 1 {
		t.Errorf("set/awaiting = %d/%d, want 1/1", set, awaiting)
	}
	if raw := rawJSONOf(t, res); strings.Contains(raw, stripeSecret) || strings.Contains(raw, `"signing_secret"`) {
		t.Errorf("list carried a secret value: %s", raw)
	}
}

// TestRotateLeavesProviderSecretAlone: rotate's job is the URL; minting a fresh secret over the
// provider's value would silently break every delivery, so the result says the secret did not change
// and reveals nothing.
func TestRotateLeavesProviderSecretAlone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newFakeStore()
	cs, _ := secretSession(t, ctx, f, strings.Fields(allWebhookVerbs), secretCeilingTypes)

	var created webhookOut
	callOK(t, ctx, cs, "create_webhook", map[string]any{
		"source_type": "stripe", "target_queue": "reviews", "signing_secret": "whsec_RotateMustNotTouchThis1",
	}, &created)
	var rotated webhookOut
	callOK(t, ctx, cs, "rotate_webhook", map[string]any{"webhook_id": created.WebhookID}, &rotated)
	if rotated.SigningSecret != "" {
		t.Error("rotate revealed a secret for a provider webhook")
	}
	if !strings.Contains(rotated.Warning, "provider-issued signing secret unchanged") {
		t.Errorf("rotate warning = %q, want it to say the secret was left alone", rotated.Warning)
	}
	if rotated.IngestURL == created.IngestURL {
		t.Error("rotate did not change the ingest URL")
	}
}

func callResult(t *testing.T, ctx context.Context, cs *sdk.ClientSession, tool string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("%s failed: %v %s", tool, err, contentText(res))
	}
	return res
}

func countFakeWebhooks(t *testing.T, ctx context.Context, cs *sdk.ClientSession) int {
	t.Helper()
	var out listWebhooksOut
	rawUnmarshal(t, callResult(t, ctx, cs, "list_webhooks", map[string]any{}), &out)
	return len(out.Webhooks)
}

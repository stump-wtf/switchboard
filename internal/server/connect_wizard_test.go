// End-to-end connect-provider-wizard tests through the real router + PostgreSQL, driven exactly
// the way a browser WITH JAVASCRIPT DISABLED drives it: plain GETs, plain form POSTs, redirects,
// cookies. They bind the SPEC-0017 "Connect Provider Wizard" contract — the token default for
// generic senders with the one-time reveal (scenario "Homelab sender lands on token"), the
// explicit open acknowledgement (scenario "Open requires intent"), the queue path's Redis
// connection settings, and the ends-registered-enabled-visible guarantee, including live dispatch
// with no restart (REQ "Runtime Provider Registry"). Skipped without
// SWITCHBOARD_TEST_DATABASE_URL (both CI hosts provide a Postgres service; local runs need
// `make ci`).
// Governing: SPEC-0017 REQ "Connect Provider Wizard"; SPEC-0015 REQ "Wizard Interaction Pattern"
// (scenario "JavaScript disabled"); ADR-0020, ADR-0003.
package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/store"
)

var connectTokenRe = regexp.MustCompile(`data-sb-connect-token>([0-9a-f]{64})<`)

// TestConnectWizardHomelabTokenDefaults walks the whole webhook path as a no-JS browser accepting
// every default: source (generic, named) → trust (token, the pre-checked default) → secret
// (generate, the default) → confirm → completion with the copyable URL and the 256-bit token shown
// exactly once — then proves the line is live on the very next request, visible on the view, and
// that the plaintext token is unrecoverable afterwards.
func TestConnectWizardHomelabTokenDefaults(t *testing.T) {
	r, st, ctx, _ := newReceiverDBRouter(t)
	_, session := mintSession(t, st, ctx, "test|connie", "Connie Ops", "connie@example.com")
	c := newWizClient(t, r, session)

	// Start: mints server-side state (cookie) and lands on the source step.
	followTo(t, c.get("/providers/connect"), "/providers/connect/source")
	if _, ok := c.cookies["sb_wiz_connect"]; !ok {
		t.Fatal("wizard start did not set the server-side state token cookie")
	}
	source := c.get("/providers/connect/source")
	if source.Code != http.StatusOK {
		t.Fatalf("source step: got %d", source.Code)
	}
	for _, want := range []string{`data-sb-wizard="connect"`, `method="post"`, `action="/providers/connect/source"`, `name="kind" value="generic" checked`} {
		if !strings.Contains(source.Body.String(), want) {
			t.Errorf("source step: missing %q", want)
		}
	}
	csrf := scrapeCSRF(t, source.Body.String())

	// Step 1 — source: a generic homelab sender.
	followTo(t, c.do(http.MethodPost, "/providers/connect/source",
		url.Values{"csrf_token": {csrf}, "kind": {"generic"}, "name": {"homelab"}}), "/providers/connect/trust")

	// Step 2 — trust: token is the pre-checked default (SPEC-0017); accept it.
	trust := c.get("/providers/connect/trust").Body.String()
	if !strings.Contains(trust, `name="trust" value="token" checked`) {
		t.Error("trust step must pre-check token for a generic sender")
	}
	followTo(t, c.do(http.MethodPost, "/providers/connect/trust",
		url.Values{"csrf_token": {csrf}, "trust": {"token"}}), "/providers/connect/secret")

	// Back navigation preserves entered values (SPEC-0015).
	if !strings.Contains(c.get("/providers/connect/source").Body.String(), `value="homelab"`) {
		t.Error("back nav: source step lost the entered provider name")
	}

	// Step 3 — secret: generate is the pre-checked default; accept it.
	secret := c.get("/providers/connect/secret").Body.String()
	if !strings.Contains(secret, `name="secret_mode" value="generate" checked`) {
		t.Error("secret step must pre-check generate")
	}
	followTo(t, c.do(http.MethodPost, "/providers/connect/secret",
		url.Values{"csrf_token": {csrf}}), "/providers/connect/confirm")

	// Step 4 — confirm: the server-side draft summarized, secret honestly "generated on connect".
	confirm := c.get("/providers/connect/confirm").Body.String()
	for _, want := range []string{"homelab", `data-sb-trust="token"`, "generated on connect", "/webhooks/generic/homelab"} {
		if !strings.Contains(confirm, want) {
			t.Errorf("confirm step: missing %q", want)
		}
	}

	// Execute: the confirm POST carries ONLY the CSRF token — the draft is server-side.
	done := c.do(http.MethodPost, "/providers/connect/confirm", url.Values{"csrf_token": {csrf}})
	if done.Code != http.StatusOK {
		t.Fatalf("confirm POST: got %d (body %.300s)", done.Code, done.Body.String())
	}
	body := done.Body.String()
	m := connectTokenRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("completion must reveal the generated token once: %.400s", body)
	}
	token := m[1]
	if !strings.Contains(body, "https://sb.example.com/webhooks/generic/homelab") {
		t.Error("completion missing the copyable absolute ingestion URL")
	}

	// Registered + enabled + LIVE immediately (scenario "Wizard-created provider is live
	// immediately"): the URL trust-checks calls with no restart anywhere.
	if rec := postWebhook(t, r, "/webhooks/generic/homelab", token, `{"n":1}`); rec.Code != http.StatusAccepted {
		t.Fatalf("delivery with the revealed token: %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	if rec := postWebhook(t, r, "/webhooks/generic/homelab", "wrong-token", `{"n":2}`); rec.Code != http.StatusForbidden {
		t.Fatalf("delivery with a wrong token: %d, want 403", rec.Code)
	}

	// Visible on the Providers view, secret rendered as presence only — never the material.
	view := c.get("/providers").Body.String()
	for _, want := range []string{`id="sb-pr-homelab"`, `data-sb-trust="token"`, `data-sb-secret-status="configured"`} {
		if !strings.Contains(view, want) {
			t.Errorf("providers view missing %q", want)
		}
	}
	if strings.Contains(view, token) {
		t.Fatal("the one-time token re-rendered on the Providers view")
	}

	// The wizard state died with the execute: revisiting the confirm step restarts the flow.
	followTo(t, c.get("/providers/connect/confirm"), "/providers/connect")
}

// TestConnectWizardOpenRequiresAck: selecting open without the explicit acknowledgement re-renders
// the trust step with an inline error and creates nothing; acknowledging proceeds (skipping the
// secret step — open holds none by design) and the resulting line accepts unverified deliveries.
// Governing: SPEC-0017 scenario "Open requires intent"; SPEC-0001 REQ "Explicit Open Trust Mode".
func TestConnectWizardOpenRequiresAck(t *testing.T) {
	r, st, ctx, _ := newReceiverDBRouter(t)
	_, session := mintSession(t, st, ctx, "test|otto", "Otto Open", "otto@example.com")
	c := newWizClient(t, r, session)

	followTo(t, c.get("/providers/connect"), "/providers/connect/source")
	csrf := scrapeCSRF(t, c.get("/providers/connect/source").Body.String())
	followTo(t, c.do(http.MethodPost, "/providers/connect/source",
		url.Values{"csrf_token": {csrf}, "kind": {"generic"}, "name": {"lan"}}), "/providers/connect/trust")

	// Open without the acknowledgement: 400, inline error, nothing created.
	rec := c.do(http.MethodPost, "/providers/connect/trust",
		url.Values{"csrf_token": {csrf}, "trust": {"open"}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "data-sb-wiz-error") {
		t.Fatalf("open without ack: got %d, want 400 with an inline step error", rec.Code)
	}
	if _, err := st.GetAdapter(ctx, "lan"); err == nil {
		t.Fatal("open without ack must create nothing")
	}

	// Acknowledged: proceeds straight to confirm — the open path has no secret step.
	followTo(t, c.do(http.MethodPost, "/providers/connect/trust",
		url.Values{"csrf_token": {csrf}, "trust": {"open"}, "open_ack": {"1"}}), "/providers/connect/confirm")
	confirm := c.get("/providers/connect/confirm").Body.String()
	if !strings.Contains(confirm, `data-sb-trust="open"`) || !strings.Contains(confirm, "none by design") {
		t.Error("open confirm must show the open chip and the none-by-design secret summary")
	}

	done := c.do(http.MethodPost, "/providers/connect/confirm", url.Values{"csrf_token": {csrf}})
	if done.Code != http.StatusOK {
		t.Fatalf("confirm POST: got %d", done.Code)
	}
	if strings.Contains(done.Body.String(), "data-sb-connect-token") {
		t.Error("open completion must reveal no token — there is none")
	}

	// The line exists, is open, and accepts an unverified delivery immediately.
	a, err := st.GetAdapter(ctx, "lan")
	if err != nil || a.TrustMode != "open" || !a.Enabled || a.SecretConfigured {
		t.Fatalf("open line = %+v (err %v), want enabled open with no secret", a, err)
	}
	if rec := postWebhook(t, r, "/webhooks/generic/lan", "", `{"n":1}`); rec.Code != http.StatusAccepted {
		t.Fatalf("open delivery: %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
}

// TestConnectWizardQueuePath: the queue path walks source (redis) → connection settings
// (stream/list/pubsub topology; the broker DSN stays in env) → confirm, ending with the row
// registered, enabled, and visible — carrying exactly the non-secret RegistryConfig shape the
// runner consumes. Governing: SPEC-0017 REQ "Connect Provider Wizard" (queue path); ADR-0014.
func TestConnectWizardQueuePath(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	_, session := mintSession(t, st, ctx, "test|quinn", "Quinn Queue", "quinn@example.com")
	c := newWizClient(t, r, session)

	followTo(t, c.get("/providers/connect"), "/providers/connect/source")
	csrf := scrapeCSRF(t, c.get("/providers/connect/source").Body.String())

	// The queue kind branches past trust/secret straight to settings.
	followTo(t, c.do(http.MethodPost, "/providers/connect/source",
		url.Values{"csrf_token": {csrf}, "kind": {"redis"}, "name": {"redis-builds"}}), "/providers/connect/settings")
	settings := c.get("/providers/connect/settings").Body.String()
	for _, want := range []string{`name="mode" value="stream" checked`, `name="target"`, "SWITCHBOARD_REDIS_URL"} {
		if !strings.Contains(settings, want) {
			t.Errorf("settings step: missing %q", want)
		}
	}
	followTo(t, c.do(http.MethodPost, "/providers/connect/settings",
		url.Values{"csrf_token": {csrf}, "mode": {"stream"}, "target": {"builds"}, "queue": {"ci"}}), "/providers/connect/confirm")

	confirm := c.get("/providers/connect/confirm").Body.String()
	for _, want := range []string{`data-sb-trust="queue"`, "stream builds", "ci"} {
		if !strings.Contains(confirm, want) {
			t.Errorf("queue confirm: missing %q", want)
		}
	}
	done := c.do(http.MethodPost, "/providers/connect/confirm", url.Values{"csrf_token": {csrf}})
	if done.Code != http.StatusOK || !strings.Contains(done.Body.String(), "data-sb-connect-topology") {
		t.Fatalf("queue completion: got %d, want 200 with the topology summary", done.Code)
	}

	// The registry row: queue family, queue trust, enabled, the runner's RegistryConfig shape.
	a, err := st.GetAdapter(ctx, "redis-builds")
	if err != nil {
		t.Fatalf("get adapter: %v", err)
	}
	if a.Family != "queue" || a.TrustMode != "queue" || a.Kind != "redis" || !a.Enabled {
		t.Fatalf("row = %+v, want enabled queue/queue/redis", a)
	}
	var cfg struct {
		Transport, Mode, Stream, Queue string
	}
	if err := json.Unmarshal(a.Config, &cfg); err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.Transport != "redis" || cfg.Mode != "stream" || cfg.Stream != "builds" || cfg.Queue != "ci" {
		t.Fatalf("config = %+v, want redis/stream/builds/ci", cfg)
	}

	// Visible on the view.
	if !strings.Contains(c.get("/providers").Body.String(), `id="sb-pr-redis-builds"`) {
		t.Error("queue line missing from the Providers view")
	}
}

// TestConnectWizardValidates: source-step input validation (unimplemented kinds, malformed and
// reserved names, duplicates), the signed path's forced name + required signing secret, unknown
// step slugs, and the cold-deep-link restart — none of which may create anything.
func TestConnectWizardValidates(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	_, session := mintSession(t, st, ctx, "test|vera", "Vera Valid", "vera@example.com")
	c := newWizClient(t, r, session)

	followTo(t, c.get("/providers/connect"), "/providers/connect/source")
	csrf := scrapeCSRF(t, c.get("/providers/connect/source").Body.String())

	post := func(step string, form url.Values) int {
		form.Set("csrf_token", csrf)
		return c.do(http.MethodPost, "/providers/connect/"+step, form).Code
	}
	badSources := []url.Values{
		{"kind": {"sqs"}, "name": {"queueish"}},     // catalog-only kind: no wizard path
		{"kind": {"generic"}, "name": {"Bad Name"}}, // charset
		{"kind": {"generic"}, "name": {""}},         // required
		{"kind": {"generic"}, "name": {"connect"}},  // reserved (wizard routes)
		{"kind": {"generic"}, "name": {"github"}},   // reserved (signed route collision)
	}
	for _, form := range badSources {
		if code := post("source", form); code != http.StatusBadRequest {
			t.Errorf("source %v: got %d, want 400", form, code)
		}
	}

	// Duplicate name: already-connected providers are refused with an inline error.
	if _, err := st.SeedProvider(ctx, store.ProviderSeed{
		Name: "taken", Family: "webhook", Kind: "generic", TrustMode: "token",
		Secret: "tok-taken", Config: []byte(`{"queue":"taken"}`),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec := c.do(http.MethodPost, "/providers/connect/source",
		url.Values{"csrf_token": {csrf}, "kind": {"generic"}, "name": {"taken"}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "already connected") {
		t.Errorf("duplicate name: got %d, want 400 with the already-connected error", rec.Code)
	}

	// Signed path: whatever name is typed, the registry name IS the kind; the signing secret is
	// required; a provided one seals and the completion reveals no token.
	followTo(t, c.do(http.MethodPost, "/providers/connect/source",
		url.Values{"csrf_token": {csrf}, "kind": {"stripe"}, "name": {"my-stripe"}}), "/providers/connect/trust")
	if !strings.Contains(c.get("/providers/connect/trust").Body.String(), "data-sb-connect-trust-fixed") {
		t.Error("signed kind must render the fixed-signed trust step")
	}
	followTo(t, c.do(http.MethodPost, "/providers/connect/trust",
		url.Values{"csrf_token": {csrf}}), "/providers/connect/secret")
	if code := post("secret", url.Values{}); code != http.StatusBadRequest {
		t.Errorf("signed secret blank: got %d, want 400", code)
	}
	followTo(t, c.do(http.MethodPost, "/providers/connect/secret",
		url.Values{"csrf_token": {csrf}, "secret": {"whsec_test_secret"}}), "/providers/connect/confirm")
	done := c.do(http.MethodPost, "/providers/connect/confirm", url.Values{"csrf_token": {csrf}})
	if done.Code != http.StatusOK {
		t.Fatalf("signed confirm: got %d", done.Code)
	}
	if strings.Contains(done.Body.String(), "data-sb-connect-token") {
		t.Error("a provided signing secret must never be revealed back")
	}
	if strings.Contains(done.Body.String(), "whsec_test_secret") {
		t.Error("the provided secret must never re-render — not even on the completion page")
	}
	a, err := st.GetAdapter(ctx, "stripe")
	if err != nil || a.TrustMode != "signed" || !a.SecretConfigured || a.Kind != "stripe" {
		t.Fatalf("stripe row = %+v (err %v), want a configured signed line named for its kind", a, err)
	}
	if _, err := st.GetAdapter(ctx, "my-stripe"); err == nil {
		t.Fatal("the typed name must not fork a second registry row for a signed kind")
	}

	// Unknown step slugs 404; a cold deep link restarts the flow.
	if rec := c.get("/providers/connect/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown step: got %d, want 404", rec.Code)
	}
	cold := newWizClient(t, r, session)
	followTo(t, cold.get("/providers/connect/trust"), "/providers/connect")

	// None of the rejected submissions created anything.
	for _, name := range []string{"queueish", "connect", "lan-x", "my-stripe"} {
		if _, err := st.GetAdapter(ctx, name); err == nil {
			t.Errorf("validation failure created provider %q", name)
		}
	}
}

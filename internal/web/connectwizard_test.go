package web

// DOM render contract for the connect-provider wizard's step pages and completion
// (templates/connect.html): each routed step is a full page with plain method=post forms (no-JS
// completion), a value-preserving Back link, the branching step tracker (webhook vs queue path),
// the open-mode acknowledgement, and the one-time generated-token reveal on the completion page.
// Assertions key on data-sb-* hooks and ids, never on style classes. The state machine over HTTP
// (redirects, branching, registration, live dispatch) is bound end-to-end in
// internal/server/connect_wizard_test.go.
// Governing: SPEC-0017 REQ "Connect Provider Wizard" (scenarios "Homelab sender lands on token",
// "Open requires intent"); SPEC-0015 REQ "Wizard Interaction Pattern"; ADR-0020, ADR-0003.

import (
	"net/url"
	"strings"
	"testing"
)

// testConnectStepView builds a render-ready step view the way renderConnectStep does (tracker
// tabs, back/action URLs) for template tests that bypass the handler. path is the active step
// path the draft would produce.
func testConnectStepView(step string, path []string) *connectStepView {
	idx := 0
	for i, s := range path {
		if s == step {
			idx = i
		}
	}
	v := &connectStepView{
		Step: step, StepNum: idx + 1, StepTotal: len(path),
		ActionURL: connectWizard.stepPath(step),
	}
	if idx > 0 {
		v.BackURL = connectWizard.stepPath(path[idx-1])
	}
	for i, s := range path {
		v.Steps = append(v.Steps, vendStepTab{Slug: s, Num: i + 1, Current: i == idx, Done: i < idx})
	}
	return v
}

func renderConnectPage(t *testing.T, h *Handler, v view) string {
	t.Helper()
	v.Title = "Connect provider"
	v.Human = testHuman()
	v.CSRF = "tok"
	v.Shell = shell{Active: "providers", DBConnected: true, Initials: "JS"}
	return renderPage(t, h, "connect", v)
}

// TestConnectWizardStepVocabulary pins the wizard's base path, slug vocabulary, and the branching
// step paths: webhook kinds walk source → trust → secret → confirm, open skips the secret step
// (none by design), and the queue kind walks source → settings → confirm.
func TestConnectWizardStepVocabulary(t *testing.T) {
	if connectWizard.base != "/providers/connect" {
		t.Errorf("connect wizard base = %q", connectWizard.base)
	}
	wantSlugs := []string{"source", "trust", "secret", "settings", "confirm"}
	if len(connectWizard.steps) != len(wantSlugs) {
		t.Fatalf("connect wizard has %d slugs, want %d", len(connectWizard.steps), len(wantSlugs))
	}
	for i, s := range wantSlugs {
		if connectWizard.steps[i] != s {
			t.Errorf("slug %d = %q, want %q", i, connectWizard.steps[i], s)
		}
	}

	cases := []struct {
		values url.Values
		want   string
	}{
		{url.Values{}, "source trust secret confirm"},
		{url.Values{"kind": {"generic"}, "trust": {"token"}}, "source trust secret confirm"},
		{url.Values{"kind": {"generic"}, "trust": {"open"}}, "source trust confirm"},
		{url.Values{"kind": {"redis"}}, "source settings confirm"},
	}
	for _, c := range cases {
		if got := strings.Join(connectPathSteps(c.values), " "); got != c.want {
			t.Errorf("connectPathSteps(%v) = %q, want %q", c.values, got, c.want)
		}
	}
}

// TestConnectFirstIncompleteStep pins the draft-completeness walk that guards the confirm execute.
func TestConnectFirstIncompleteStep(t *testing.T) {
	cases := []struct {
		values url.Values
		want   string
	}{
		{url.Values{}, "source"},
		{url.Values{"kind": {"sqs"}, "name": {"x"}}, "source"}, // catalog-only kind never completes source
		{url.Values{"kind": {"generic"}, "name": {"lab"}}, "trust"},
		{url.Values{"kind": {"generic"}, "name": {"lab"}, "trust": {"token"}}, "secret"},
		{url.Values{"kind": {"generic"}, "name": {"lab"}, "trust": {"token"}, "secret_mode": {"generate"}}, ""},
		{url.Values{"kind": {"generic"}, "name": {"lab"}, "trust": {"token"}, "secret_mode": {"provide"}}, "secret"},
		{url.Values{"kind": {"generic"}, "name": {"lab"}, "trust": {"open"}}, ""},
		{url.Values{"kind": {"github"}, "name": {"github"}, "trust": {"signed"}}, "secret"},
		{url.Values{"kind": {"github"}, "name": {"github"}, "trust": {"signed"}, "secret": {"s"}}, ""},
		{url.Values{"kind": {"redis"}, "name": {"builds"}}, "settings"},
		{url.Values{"kind": {"redis"}, "name": {"builds"}, "mode": {"stream"}, "target": {"builds"}}, ""},
	}
	for _, c := range cases {
		if got := firstIncompleteConnectStep(c.values); got != c.want {
			t.Errorf("firstIncompleteConnectStep(%v) = %q, want %q", c.values, got, c.want)
		}
	}
}

// TestConnectStepSourceRendersKindsAndName: step 1 offers ONLY implemented kinds (catalog-only
// SQS/NATS/AMQP never appear — the wizard cannot fake a backend), pre-checks generic as the
// default, and prefills the draft name (value-preserving back nav).
func TestConnectStepSourceRendersKindsAndName(t *testing.T) {
	h := newTestHandler(t)
	v := testConnectStepView("source", []string{"source", "trust", "secret", "confirm"})
	v.Name = "homelab"
	for _, c := range implementedCatalog {
		v.KindOptions = append(v.KindOptions, connectKindOption{
			Kind: c.Kind, Title: c.Title, Family: c.Family, Trust: c.Trust, Desc: c.Desc,
			Checked: c.Kind == "generic",
		})
	}
	body := renderConnectPage(t, h, view{Connect: v})
	for _, want := range []string{
		`data-sb-wizard="connect"`,
		`aria-current="step"`,
		`data-sb-wiz-step="source"`,
		"step 1 of 4",
		`method="post"`, // plain form — no-JS completion
		`action="/providers/connect/source"`,
		`name="csrf_token" value="tok"`,
		`data-sb-connect-kinds`,
		`name="kind" value="github"`, `name="kind" value="stripe"`, `name="kind" value="slack"`,
		`name="kind" value="generic" checked`, // the pit-of-success default
		`name="kind" value="redis"`,
		`name="name"`, `value="homelab"`, `data-sb-connect-name`,
		`href="/providers" data-sb-wiz-cancel`,
		`data-sb-connect-submit`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("source step: missing %q", want)
		}
	}
	// Catalog-only kinds must never be offered.
	for _, forbid := range []string{`value="sqs"`, `value="nats"`, `value="amqp"`} {
		if strings.Contains(body, forbid) {
			t.Errorf("source step must not offer catalog-only kind %q", forbid)
		}
	}
	if strings.Contains(body, "data-sb-wiz-back") {
		t.Error("source step must not render a Back link (it is the first step)")
	}
}

// TestConnectStepTrustGenericDefaultsToken: the generic trust step pre-checks token (SPEC-0017:
// token as the default for generic senders), offers open only alongside the danger callout and
// the explicit acknowledgement checkbox, and never offers signed (no real scheme).
func TestConnectStepTrustGenericDefaultsToken(t *testing.T) {
	h := newTestHandler(t)
	v := testConnectStepView("trust", []string{"source", "trust", "secret", "confirm"})
	v.Kind = "generic"
	v.TrustMode = "token"
	body := renderConnectPage(t, h, view{Connect: v})
	for _, want := range []string{
		`action="/providers/connect/trust"`,
		`data-sb-connect-trust`,
		`name="trust" value="token" checked`, // the default
		`name="trust" value="open"`,
		`name="open_ack" value="1"`, // the acknowledgement, server-enforced
		"data-sb-connect-open-ack",
		"no verification at all", // consequence stated plainly
		`href="/providers/connect/source" data-sb-wiz-back`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("trust step (generic): missing %q", want)
		}
	}
	if strings.Contains(body, `name="trust" value="signed"`) {
		t.Error("generic trust step must not offer signed — no real scheme exists")
	}
	// A draft that chose open re-renders with open checked and the ack preserved.
	v.TrustMode = "open"
	v.OpenAck = true
	open := renderConnectPage(t, h, view{Connect: v})
	for _, want := range []string{`name="trust" value="open" checked`, `name="open_ack" value="1" checked`} {
		if !strings.Contains(open, want) {
			t.Errorf("trust step (open draft): missing %q", want)
		}
	}
}

// TestConnectStepTrustSignedFixed: for kinds with a real scheme the trust step offers NO choice —
// signed is stated as fixed, with no radio group and no weaker mode.
func TestConnectStepTrustSignedFixed(t *testing.T) {
	h := newTestHandler(t)
	v := testConnectStepView("trust", []string{"source", "trust", "secret", "confirm"})
	v.Kind = "github"
	v.SignedKind = true
	v.TrustMode = "signed"
	body := renderConnectPage(t, h, view{Connect: v})
	for _, want := range []string{"data-sb-connect-trust-fixed", `data-sb-trust="signed"`, "No weaker mode is offered"} {
		if !strings.Contains(body, want) {
			t.Errorf("trust step (signed kind): missing %q", want)
		}
	}
	for _, forbid := range []string{`name="trust"`, `name="open_ack"`} {
		if strings.Contains(body, forbid) {
			t.Errorf("signed-kind trust step must not render %q — there is no choice to make", forbid)
		}
	}
}

// TestConnectStepSecretTokenModes: the token secret step defaults to generate (minted at execute,
// shown once at the end); the provide input NEVER echoes a staged secret back into markup, and a
// staged draft says so instead.
func TestConnectStepSecretTokenModes(t *testing.T) {
	h := newTestHandler(t)
	v := testConnectStepView("secret", []string{"source", "trust", "secret", "confirm"})
	v.Kind = "generic"
	v.TrustMode = "token"
	v.SecretMode = "generate"
	body := renderConnectPage(t, h, view{Connect: v})
	for _, want := range []string{
		`action="/providers/connect/secret"`,
		`data-sb-connect-secret-mode`,
		`name="secret_mode" value="generate" checked`, // the default
		`name="secret_mode" value="provide"`,
		`name="secret" value=""`, `data-sb-connect-secret`,
		"shown exactly once",
		`href="/providers/connect/trust" data-sb-wiz-back`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("secret step (token): missing %q", want)
		}
	}
	// A staged provided secret: mode re-checked, value NEVER echoed, note rendered.
	v.SecretMode = "provide"
	v.SecretStaged = true
	staged := renderConnectPage(t, h, view{Connect: v})
	for _, want := range []string{`name="secret_mode" value="provide" checked`, `name="secret" value=""`, "already staged"} {
		if !strings.Contains(staged, want) {
			t.Errorf("secret step (staged draft): missing %q", want)
		}
	}
	// The signed variant is a single required secret input, no mode radios.
	sv := testConnectStepView("secret", []string{"source", "trust", "secret", "confirm"})
	sv.Kind = "stripe"
	sv.TrustMode = "signed"
	signed := renderConnectPage(t, h, view{Connect: sv})
	if !strings.Contains(signed, `name="secret" value=""`) || strings.Contains(signed, "data-sb-connect-secret-mode") {
		t.Error("signed secret step must render a bare secret input, no generate/provide modes")
	}
}

// TestConnectStepSettingsRedis: the queue path's settings step offers the three Redis modes
// (stream recommended + pre-checked), the consume target, the optional todo queue — and states
// that the broker DSN lives in env, never the registry (ADR-0014).
func TestConnectStepSettingsRedis(t *testing.T) {
	h := newTestHandler(t)
	v := testConnectStepView("settings", []string{"source", "settings", "confirm"})
	v.Kind = "redis"
	v.QueueMode = "stream"
	v.Target = "builds"
	v.Queue = "ci"
	body := renderConnectPage(t, h, view{Connect: v})
	for _, want := range []string{
		"step 2 of 3", // the queue path is three steps
		`action="/providers/connect/settings"`,
		`data-sb-connect-mode`,
		`name="mode" value="stream" checked`,
		`name="mode" value="list"`, `name="mode" value="pubsub"`,
		`name="target"`, `value="builds"`, `data-sb-connect-target`,
		`name="queue"`, `value="ci"`, `data-sb-connect-queue`,
		"SWITCHBOARD_REDIS_URL", // the DSN is env, never collected here
		`href="/providers/connect/source" data-sb-wiz-back`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings step: missing %q", want)
		}
	}
	if strings.Contains(body, "data-sb-connect-trust") {
		t.Error("queue path must render no trust choice — the broker connection is the trust")
	}
}

// TestConnectStepConfirmSummarizes: the confirm step summarizes the SERVER-SIDE draft (the form
// posts only the CSRF token), shows the enforced trust chip and the honest secret summary, and
// states that connecting registers the provider enabled.
func TestConnectStepConfirmSummarizes(t *testing.T) {
	h := newTestHandler(t)
	v := testConnectStepView("confirm", []string{"source", "trust", "secret", "confirm"})
	v.Kind = "generic"
	v.Name = "homelab"
	v.Family = "webhook"
	v.TrustMode = "token"
	v.TrustLabel = connectTrustLabel("token")
	v.SecretSummary = "generated on connect · shown exactly once on the next page"
	v.IngestPath = "/webhooks/generic/homelab"
	v.Queue = "homelab"
	body := renderConnectPage(t, h, view{Connect: v})
	for _, want := range []string{
		`action="/providers/connect/confirm"`,
		`data-sb-wiz-summary`,
		"homelab", `data-sb-trust="token"`,
		"data-sb-connect-secret-summary", "shown exactly once",
		"/webhooks/generic/homelab",
		"<strong>enabled</strong>",
		"Connect provider →",
		`href="/providers/connect/secret" data-sb-wiz-back`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("confirm step: missing %q", want)
		}
	}
	// The confirm form must NOT carry the draft as hidden fields — state is server-side.
	for _, bad := range []string{`type="hidden" name="kind"`, `type="hidden" name="name"`, `type="hidden" name="trust"`, `type="hidden" name="secret"`} {
		if strings.Contains(body, bad) {
			t.Errorf("confirm step: draft leaked into hidden field %q — state must be server-side", bad)
		}
	}
}

// TestConnectStepErrorRerenders: a step validation failure re-renders the same page with an inline
// alert — never a dead end (SPEC-0015: the flow is resumable in place).
func TestConnectStepErrorRerenders(t *testing.T) {
	h := newTestHandler(t)
	v := testConnectStepView("trust", []string{"source", "trust", "secret", "confirm"})
	v.Kind = "generic"
	v.TrustMode = "open"
	v.Error = "open means no verification at all — anyone who can reach the URL can inject events. Acknowledge the risk to proceed."
	body := renderConnectPage(t, h, view{Connect: v})
	if !strings.Contains(body, `role="alert"`) || !strings.Contains(body, "data-sb-wiz-error") {
		t.Error("step error must render as an alert with the data-sb-wiz-error hook")
	}
	if !strings.Contains(body, "Acknowledge the risk to proceed.") {
		t.Error("step error text missing")
	}
}

// TestConnectDoneRevealsGeneratedTokenOnce: the completion page shows the copyable ingestion URL
// and — for the generated default — the token in the standard one-time reveal (SPEC-0017 scenario
// "Homelab sender lands on token"). A provided secret is never echoed, not even here; an open line
// states its consequence; the queue completion shows topology + the restart-honesty note.
func TestConnectDoneRevealsGeneratedTokenOnce(t *testing.T) {
	h := newTestHandler(t)
	done := connectDoneView{
		Name: "homelab", Kind: "generic", Family: "webhook", TrustMode: "token",
		Path: "/webhooks/generic/homelab", URL: "https://sb.example.com/webhooks/generic/homelab",
		Token: "aabbccdd-once",
	}
	body := renderConnectPage(t, h, view{ConnectDone: &done})
	for _, want := range []string{
		"data-sb-connect-done",
		`data-sb-trust="token"`,
		"data-sb-connect-url", "https://sb.example.com/webhooks/generic/homelab",
		"data-sb-connect-token", "aabbccdd-once", "shown once",
		"X-Webhook-Token", // how the sender presents it
		`href="/providers" data-sb-connect-done-btn`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("completion (generated token): missing %q", want)
		}
	}

	// Provided secret: sealed, never echoed — no token block at all.
	provided := connectDoneView{
		Name: "github", Kind: "github", Family: "webhook", TrustMode: "signed",
		Path: "/webhooks/github", URL: "https://sb.example.com/webhooks/github",
		SecretProvided: true,
	}
	pbody := renderConnectPage(t, h, view{ConnectDone: &provided})
	if strings.Contains(pbody, "data-sb-connect-token") {
		t.Error("completion for a provided secret must not render a token reveal")
	}
	if !strings.Contains(pbody, `data-sb-secret-status="configured"`) || !strings.Contains(pbody, "never shown again") {
		t.Error("completion for a provided secret must state configured · sealed · never shown again")
	}

	// Open: no secret anywhere, and the consequence stated plainly.
	open := connectDoneView{
		Name: "lan", Kind: "generic", Family: "webhook", TrustMode: "open",
		Path: "/webhooks/generic/lan", URL: "https://sb.example.com/webhooks/generic/lan",
	}
	obody := renderConnectPage(t, h, view{ConnectDone: &open})
	if strings.Contains(obody, "data-sb-connect-token") {
		t.Error("open completion must render no token")
	}
	if !strings.Contains(obody, "accepted unverified") {
		t.Error("open completion must state the consequence plainly")
	}

	// Queue: topology + queue label + restart honesty, no URL, no token.
	queue := connectDoneView{
		Name: "redis-builds", Kind: "redis", Family: "queue", TrustMode: "queue",
		Topology: "stream builds", Queue: "ci",
	}
	qbody := renderConnectPage(t, h, view{ConnectDone: &queue})
	for _, want := range []string{"data-sb-connect-topology", "stream builds", "ci", "next restart", "SWITCHBOARD_REDIS_URL"} {
		if !strings.Contains(qbody, want) {
			t.Errorf("queue completion: missing %q", want)
		}
	}
	for _, forbid := range []string{"data-sb-connect-url", "data-sb-connect-token"} {
		if strings.Contains(qbody, forbid) {
			t.Errorf("queue completion must not render %q", forbid)
		}
	}
}

// TestProvidersViewOffersConnectEntryPoints: the view head and every connectable catalog card link
// into the wizard (seeded with the card's kind); available cards still expose no path at all —
// that contract is pinned by TestProvidersCatalogHonesty.
func TestProvidersViewOffersConnectEntryPoints(t *testing.T) {
	h := newTestHandler(t)
	body := renderPage(t, h, "providers", providersView(h, nil, nil))
	for _, want := range []string{
		`href="/providers/connect" data-sb-connect-start`,
		`href="/providers/connect?kind=generic" data-sb-catalog-connect`,
		`href="/providers/connect?kind=redis" data-sb-catalog-connect`,
		`href="/providers/connect?kind=github" data-sb-catalog-connect`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("providers view missing connect entry point %q", want)
		}
	}
}

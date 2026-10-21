// Verb scoping on the operator CLI: `endpoint vend` grants the narrow todo surface unless the
// operator names more, `--verbs` takes verb names and presets, `--allow-webhook-admin` is the
// explicit opt-in, and `endpoint list` shows what each lane credential actually holds.
// Issue #589. Governing: ADR-0047.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/mcp"
)

func vendBody(t *testing.T, f *fakeDeployment) map[string]any {
	t.Helper()
	if len(f.vends) != 1 {
		t.Fatalf("vend requests = %v, want exactly one", f.vends)
	}
	return f.vends[0]
}

// The default vend sends no verbs at all: the server's narrow default is the single source of what
// "nothing named" grants, and the reveal of a narrow grant carries no admin warning.
func TestVendDefaultsNarrowAndSaysNothingAboutAdmin(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))

	if code := tc.run(t, "endpoint", "vend", "worker-1", "--queue", "lane-m"); code != exitOK {
		t.Fatalf("vend: code %d, stderr %q", code, tc.stderr.String())
	}
	body := vendBody(t, f)
	if _, sent := body["verbs"]; sent {
		t.Errorf("default vend sent verbs %v; want the field absent so the server's default applies", body["verbs"])
	}
	out := tc.stdout.String()
	if strings.Contains(out, "Warning") {
		t.Errorf("a narrow grant must not warn: %s", out)
	}
}

func TestVendVerbsFlagSendsTheResolvedGrant(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))

	if code := tc.run(t, "endpoint", "vend", "worker-2", "--verbs", "todo"); code != exitOK {
		t.Fatalf("vend --verbs todo: code %d, stderr %q", code, tc.stderr.String())
	}
	verbs := grantOf(t, vendBody(t, f))
	if strings.Join(verbs, " ") != strings.Join(mcp.DrainVerbs(), " ") {
		t.Errorf("--verbs todo sent %q, want the todo verbs %q", verbs, mcp.DrainVerbs())
	}
	if strings.Contains(tc.stdout.String(), "Warning") {
		t.Errorf("the todo grant carries no admin verbs and must not warn: %s", tc.stdout.String())
	}
}

// Naming admin verbs, by preset or by the opt-in flag, is the operator asking for them — and the
// reveal says out loud what that credential can do beyond draining its queue.
func TestVendWebhookAdminOptInWarns(t *testing.T) {
	for name, args := range map[string][]string{
		"--allow-webhook-admin": {"endpoint", "vend", "router-1", "--allow-webhook-admin"},
		"--verbs webhook-admin": {"endpoint", "vend", "router-2", "--verbs", "webhook-admin"},
		"--verbs all":           {"endpoint", "vend", "router-3", "--verbs", "all"},
	} {
		tc := newTestCLI(t)
		f := newFakeDeployment(t)
		tc.loggedIn(t, f, tc.clock.Add(time.Hour))
		if code := tc.run(t, args...); code != exitOK {
			t.Fatalf("%s: code %d, stderr %q", name, code, tc.stderr.String())
		}
		verbs := grantOf(t, vendBody(t, f))
		if !mcp.GrantHasWebhookAdmin(verbs) {
			t.Errorf("%s sent %q; want the webhook-admin verbs in the grant", name, verbs)
		}
		mustContain(t, name+" reveal", tc.stdout.String(), "Warning", "webhooks, routing and event history")
	}
}

// A verb name that does not exist stops the command before the deployment mints an agent, an
// endpoint and a webhook — a credential quieter than the operator asked for is the failure to avoid.
func TestVendRefusesUnknownVerbsBeforeMinting(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))

	code := tc.run(t, "endpoint", "vend", "worker-3", "--verbs", "claim_nextt")
	if code != exitUsage {
		t.Fatalf("unknown verb: code %d, want %d (stderr %q)", code, exitUsage, tc.stderr.String())
	}
	mustContain(t, "unknown verb", tc.stderr.String(), "claim_nextt", "preset")
	if len(f.vends) != 0 {
		t.Errorf("an unknown verb must mint nothing; the deployment saw %v", f.vends)
	}

	if code := tc.run(t, "endpoint", "vend", "worker-4", "--verbs", "todo,no_such_verb"); code != exitUsage {
		t.Errorf("one bad verb among good ones: code %d, want %d", code, exitUsage)
	}
}

func TestEndpointListShowsTheVerbGrant(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	f.endpointRows = []map[string]any{
		{"slug": "lane-a-1", "agent_name": "lane-a", "state": "active", "queues": []string{"lane-a"},
			"verbs": []string{"list_todos", "claim", "claim_next", "complete", "fail", "release", "heartbeat", "get_todo"}},
		{"slug": "router-1", "agent_name": "router", "state": "active", "queues": []string{"lane-a", "lane-b"},
			"verbs": mcp.AllVerbs()},
	}

	if code := tc.run(t, "endpoint", "list"); code != exitOK {
		t.Fatalf("endpoint list: code %d, stderr %q", code, tc.stderr.String())
	}
	out := tc.stdout.String()
	mustContain(t, "endpoint list", out, "VERBS", "8",
		fmt.Sprintf("%d (%d webhook-admin)", len(mcp.AllVerbs()), len(mcp.WebhookAdminVerbs())))
	if strings.Contains(out, "rotate_webhook") {
		t.Errorf("the default listing names verbs; it should count them: %s", out)
	}

	tc.stdout.Reset()
	if code := tc.run(t, "endpoint", "list", "-v"); code != exitOK {
		t.Fatalf("endpoint list -v: code %d, stderr %q", code, tc.stderr.String())
	}
	mustContain(t, "endpoint list -v", tc.stdout.String(), "rotate_webhook", "replay_webhook_event")

	// An older server sends no verbs: say nothing rather than a wrong number.
	tc.stdout.Reset()
	f.endpointRows = []map[string]any{{"slug": "old-1", "agent_name": "old", "state": "active", "queues": []string{"inbox"}}}
	if code := tc.run(t, "endpoint", "list"); code != exitOK {
		t.Fatalf("endpoint list against an older server: code %d, stderr %q", code, tc.stderr.String())
	}
	mustContain(t, "endpoint list, no verbs field", tc.stdout.String(), "VERBS")
	if strings.Contains(tc.stdout.String(), "0 ") {
		t.Errorf("an older server must not read as a zero-verb endpoint: %s", tc.stdout.String())
	}
}

// grantOf reads the verb grant out of a captured vend body.
func grantOf(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, err := json.Marshal(body["verbs"])
	if err != nil {
		t.Fatalf("marshal verbs: %v", err)
	}
	var verbs []string
	if err := json.Unmarshal(raw, &verbs); err != nil {
		t.Fatalf("decode verbs from %v: %v", body["verbs"], err)
	}
	return verbs
}

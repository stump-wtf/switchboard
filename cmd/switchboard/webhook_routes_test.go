package main

// The webhook route verbs against the fake deployment: the request each one sends (asserted on the
// calls the fake recorded, not only on the output), slug resolution, --json, the API's errors, and the
// usage mistakes that must never reach the API.
//
// @joestump-agent 10/02/2026 - Added with webhook route list/add/remove (#555).

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	routeOwnerID  = "a1b2c3d4-0000-4000-8000-000000000001"
	routeLaneSID  = "a1b2c3d4-0000-4000-8000-000000000002"
	routeFriendID = "a1b2c3d4-0000-4000-8000-000000000003"
)

// seedRoutes gives the fake one routable webhook (hookID, owned by routeOwnerID) and two endpoints
// of the operator's own, so slugs resolve.
func seedRoutes(f *fakeDeployment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routeOwners = map[string]string{hookID: routeOwnerID}
	f.endpointRows = []map[string]any{
		{"id": routeOwnerID, "slug": "cairn-handoff-router-1363fe15", "agent_name": "router", "state": "active"},
		{"id": routeLaneSID, "slug": "joestump-agent-handoff-s-f3fecc1e", "agent_name": "lane-s", "state": "active"},
	}
}

func TestWebhookRouteAddBySlugThenList(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	seedRoutes(f)

	// No routes yet: the owner, marked, and a hint.
	if code := tc.run(t, "webhook", "route", "list", hookID); code != exitOK {
		t.Fatalf("route list: code %d, stderr %q", code, tc.stderr.String())
	}
	mustContain(t, "empty list", tc.stdout.String(), "Webhook "+hookID+" delivers to:", routeOwnerID,
		"cairn-handoff-router-1363fe15", "owner (always a target)", "No routes")

	// #555's "done when": add by slug. The slug is resolved to the id, and the id is what is sent.
	if code := tc.run(t, "webhook", "route", "add", hookID, "joestump-agent-handoff-s-f3fecc1e"); code != exitOK {
		t.Fatalf("route add: code %d, stderr %q", code, tc.stderr.String())
	}
	mustContain(t, "add output", tc.stdout.String(), "now also delivers to joestump-agent-handoff-s-f3fecc1e ("+routeLaneSID+")",
		"webhook rules get "+hookID)
	// A friend's endpoint is named by id and sent as given, with no slug lookup needed.
	if code := tc.run(t, "webhook", "route", "add", hookID, routeFriendID); code != exitOK {
		t.Fatalf("route add by id: code %d, stderr %q", code, tc.stderr.String())
	}
	f.mu.Lock()
	calls := slices.Clone(f.routeCalls)
	f.mu.Unlock()
	if want := []string{"PUT " + hookID + " " + routeLaneSID, "PUT " + hookID + " " + routeFriendID}; !slices.Equal(calls, want) {
		t.Fatalf("route writes = %q, want %q", calls, want)
	}

	// The list: owner first, then routes oldest grant first, slugs where the operator has them.
	f.mu.Lock()
	f.routeRows[hookID] = []map[string]any{ // as the API sends them: newest grant first
		{"target_endpoint_id": routeFriendID, "granted_at": "2026-10-02T12:05:00Z"},
		{"target_endpoint_id": routeLaneSID, "granted_at": "2026-10-02T12:00:00Z"},
	}
	f.mu.Unlock()
	if code := tc.run(t, "webhook", "route", "list", hookID); code != exitOK {
		t.Fatalf("route list: code %d, stderr %q", code, tc.stderr.String())
	}
	out := tc.stdout.String()
	mustContain(t, "list", out, "ENDPOINT", "SLUG", "GRANTED", "joestump-agent-handoff-s-f3fecc1e", "2026-10-02T12:00:00Z",
		routeFriendID+"  -", "2026-10-02T12:05:00Z")
	if owner, lane, friend := strings.Index(out, routeOwnerID), strings.Index(out, routeLaneSID), strings.Index(out, routeFriendID); owner >= lane || lane >= friend {
		t.Fatalf("list is not in delivery order (owner, then oldest grant first):\n%s", out)
	}
	if strings.Contains(out, "No routes") {
		t.Fatalf("list with routes says there are none:\n%s", out)
	}

	// --json is the API's document, verbatim.
	if code := tc.run(t, "webhook", "route", "list", hookID, "--json"); code != exitOK {
		t.Fatalf("route list --json: code %d", code)
	}
	var doc map[string]any
	if err := json.Unmarshal(tc.stdout.Bytes(), &doc); err != nil || doc["owner_endpoint_id"] != routeOwnerID || len(doc["routes"].([]any)) != 2 {
		t.Fatalf("--json output = %q (%v), want the API document", tc.stdout.String(), err)
	}
	if code := tc.run(t, "webhook", "route", "add", "--json", hookID, routeLaneSID); code != exitOK {
		t.Fatalf("route add --json: code %d", code)
	}
	if err := json.Unmarshal(tc.stdout.Bytes(), &doc); err != nil || doc["routed"] != true || doc["target_endpoint_id"] != routeLaneSID {
		t.Fatalf("add --json output = %q (%v), want the API document", tc.stdout.String(), err)
	}
}

func TestWebhookRouteRemove(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	seedRoutes(f)

	if code := tc.run(t, "webhook", "route", "remove", hookID, "joestump-agent-handoff-s-f3fecc1e"); code != exitOK {
		t.Fatalf("route remove: code %d, stderr %q", code, tc.stderr.String())
	}
	mustContain(t, "remove output", tc.stdout.String(), "no longer delivers to joestump-agent-handoff-s-f3fecc1e ("+routeLaneSID+")")
	if code := tc.run(t, "webhook", "route", "remove", hookID, routeFriendID, "--json"); code != exitOK {
		t.Fatalf("route remove --json: code %d", code)
	}
	var doc map[string]any
	if err := json.Unmarshal(tc.stdout.Bytes(), &doc); err != nil || doc["removed"] != true {
		t.Fatalf("remove --json output = %q (%v), want the API document", tc.stdout.String(), err)
	}
	f.mu.Lock()
	calls := slices.Clone(f.routeCalls)
	f.mu.Unlock()
	if want := []string{"DELETE " + hookID + " " + routeLaneSID, "DELETE " + hookID + " " + routeFriendID}; !slices.Equal(calls, want) {
		t.Fatalf("route writes = %q, want %q", calls, want)
	}
}

// The API's refusals print their message and code and exit 1; a slug that is not the operator's
// fails before any route write.
func TestWebhookRouteErrors(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	seedRoutes(f)
	f.mu.Lock()
	f.unroutable = map[string]bool{routeFriendID: true}
	f.mu.Unlock()

	if code := tc.run(t, "webhook", "route", "add", hookID, routeFriendID); code != exitFailure {
		t.Fatalf("refused target: code %d, want %d", code, exitFailure)
	}
	mustContain(t, "forbidden", tc.stderr.String(), "403 Forbidden", "target endpoint is not routable from this endpoint (forbidden)")

	if code := tc.run(t, "webhook", "route", "list", "6ba7b810-9dad-11d1-80b4-00c04fd430c8"); code != exitFailure {
		t.Fatalf("unknown webhook: code %d, want %d", code, exitFailure)
	}
	mustContain(t, "not found", tc.stderr.String(), "404 Not Found", "webhook not found (not_found)")

	f.mu.Lock()
	before := len(f.routeCalls)
	f.mu.Unlock()
	for _, verb := range []string{"add", "remove"} {
		if code := tc.run(t, "webhook", "route", verb, hookID, "someone-elses-bot"); code != exitFailure {
			t.Fatalf("%s with an unknown slug: code %d, want %d", verb, code, exitFailure)
		}
		mustContain(t, "unknown slug", tc.stderr.String(), `no endpoint of yours has the slug "someone-elses-bot"`, "by its id")
	}
	f.mu.Lock()
	after := len(f.routeCalls)
	f.mu.Unlock()
	if after != before {
		t.Fatalf("an unresolved slug reached the route API (%d writes)", after-before)
	}
}

func TestWebhookRouteUsageMistakes(t *testing.T) {
	tc := newTestCLI(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"webhook", "route", "list"}, "a webhook id is required"},
		{[]string{"webhook", "route", "list", hookID, "extra"}, `unexpected argument "extra"`},
		{[]string{"webhook", "route", "add", hookID}, "an endpoint is required"},
		{[]string{"webhook", "route", "add"}, "a webhook id is required"},
		{[]string{"webhook", "route", "remove", hookID, routeLaneSID, "extra"}, `unexpected argument "extra"`},
		{[]string{"webhook", "route", "add", "--bogus", hookID, routeLaneSID}, "flag provided but not defined"},
		{[]string{"webhook", "route", "bogus"}, `unknown verb "bogus"`},
	} {
		// Not logged in: a usage mistake must be reported as one, before anything needs credentials.
		if code := tc.run(t, c.args...); code != exitUsage {
			t.Fatalf("%v: code %d, want %d (stderr %q)", c.args, code, exitUsage, tc.stderr.String())
		}
		mustContain(t, strings.Join(c.args, " "), tc.stderr.String(), c.want)
	}
}

func TestWebhookRouteHelp(t *testing.T) {
	tc := newTestCLI(t)
	for _, args := range [][]string{{"webhook", "-h"}, {"help", "webhook"}, {"help", "webhook", "route"}, {"webhook", "route", "-h"}} {
		if code := tc.run(t, args...); code != exitOK {
			t.Fatalf("%v: code %d, want %d (stderr %q)", args, code, exitOK, tc.stderr.String())
		}
		mustContain(t, strings.Join(args, " "), tc.stdout.String(), "switchboard webhook route list WEBHOOK_ID",
			"switchboard webhook route add WEBHOOK_ID ENDPOINT", "switchboard webhook route remove WEBHOOK_ID ENDPOINT")
	}
	if code := tc.run(t, "help", "webhook", "route", "add"); code != exitOK {
		t.Fatalf("help webhook route add: code %d", code)
	}
	mustContain(t, "verb help", tc.stdout.String(), "usage: switchboard webhook route add [flags] WEBHOOK_ID ENDPOINT", "-json", "approved friend request")
	if code := tc.run(t, "help"); code != exitOK {
		t.Fatalf("help: code %d", code)
	}
	mustContain(t, "top-level help", tc.stdout.String(), "webhook route add WEBHOOK_ID ENDPOINT")
}

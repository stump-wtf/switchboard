package main

// The webhook and rules verbs against the fake deployment: what each sends (asserted on the wire
// body, not only the output), how it prints, the get --json | set --file round trip, the params
// semantics, and the usage mistakes that must never reach the API.

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

const hookID = "0f7c1c1e-7b1a-4a53-9a57-5b3a3c1f2e10"

// seedRules gives the fake one webhook with two rules (one a work order), a default and params.
func seedRules(f *fakeDeployment) map[string]any {
	doc := map[string]any{
		"webhook_id": hookID, "source_type": "gitea", "target_queue": "triage",
		"default_action": map[string]any{"drop": true},
		"rules": []any{
			map[string]any{"id": "noise", "name": "ci noise", "expr": `.kind == "workflow_run"`, "action": map[string]any{"drop": true}},
			map[string]any{"id": "lane-s", "expr": `.issue.labels | any(.name == "size/S")`,
				"action": map[string]any{"queue": "lane-s", "exclusive": true, "once": true, "work_order": true}},
		},
		"params": map[string]any{"repo_prefixes": []any{"stump.wtf/"}, "max": 9007199254740993},
		"grant":  map[string]any{"queues": []any{"triage", "lane-s"}, "endpoints": []any{"ep-1"}},
	}
	f.mu.Lock()
	f.rulesDocs = map[string]any{hookID: doc}
	f.mu.Unlock()
	return doc
}

func TestWebhookListTable(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))

	if code := tc.run(t, "webhook", "list"); code != exitOK || !strings.Contains(tc.stdout.String(), "No webhooks yet") {
		t.Fatalf("empty list: code %d, stdout %q", code, tc.stdout.String())
	}
	f.mu.Lock()
	f.webhookRows = []map[string]any{
		{"webhook_id": hookID, "endpoint_slug": "forge-router-ab12", "endpoint_state": "active", "source_type": "gitea",
			"target_queue": "triage", "rule_count": 2, "has_default_action": true, "has_params": true},
		{"webhook_id": "wh-old", "endpoint_slug": "old-bot-ffff", "endpoint_state": "revoked", "source_type": "generic",
			"target_queue": "inbox", "rule_count": 0},
	}
	f.mu.Unlock()
	if code := tc.run(t, "webhook", "list"); code != exitOK {
		t.Fatalf("list: code %d, stderr %q", code, tc.stderr.String())
	}
	mustContain(t, "webhook table", tc.stdout.String(), "WEBHOOK", "ENDPOINT", "RULES",
		hookID, "forge-router-ab12", "2 (+default, params)", "wh-old", "revoked")

	if code := tc.run(t, "webhook", "list", "--json"); code != exitOK {
		t.Fatalf("list --json: code %d", code)
	}
	var doc map[string]any
	if err := json.Unmarshal(tc.stdout.Bytes(), &doc); err != nil || len(doc["webhooks"].([]any)) != 2 {
		t.Fatalf("--json output = %q (%v), want the API document", tc.stdout.String(), err)
	}
}

func TestWebhookRulesGetPrintsTheConfiguration(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	seedRules(f)

	if code := tc.run(t, "webhook", "rules", "get", hookID); code != exitOK {
		t.Fatalf("rules get: code %d, stderr %q", code, tc.stderr.String())
	}
	out := tc.stdout.String()
	mustContain(t, "rules get", out, "Webhook "+hookID+" (gitea, target queue triage)",
		"noise", "ci noise", `.kind == "workflow_run"`, "drop",
		"lane-s", "queue lane-s [WORK ORDER, exclusive, once]",
		"Default:  drop", "repo_prefixes = [\"stump.wtf/\"]", "Grant:    queues triage, lane-s; endpoints ep-1")
	if strings.Index(out, "noise") > strings.Index(out, "lane-s ") {
		t.Fatalf("rules are not in evaluation order:\n%s", out)
	}

	// An unknown (or someone else's) webhook: the API's message and code, exit 1.
	if code := tc.run(t, "webhook", "rules", "get", "nope"); code != exitFailure {
		t.Fatalf("unknown webhook: code %d, want %d", code, exitFailure)
	}
	mustContain(t, "not found", tc.stderr.String(), "404 Not Found", "webhook not found (not_found)")
}

// get --json is a valid set --file: the document the API printed goes back verbatim, and saving it
// changes nothing.
func TestWebhookRulesGetJSONRoundTripsIntoSet(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	before := seedRules(f)

	if code := tc.run(t, "webhook", "rules", "get", "--json", hookID); code != exitOK {
		t.Fatalf("rules get --json: code %d", code)
	}
	saved := append([]byte(nil), tc.stdout.Bytes()...)
	tc.readFile = func(name string) ([]byte, error) {
		if name != "rules.json" {
			return nil, errors.New("unexpected file " + name)
		}
		return saved, nil
	}
	if code := tc.run(t, "webhook", "rules", "set", hookID, "--file", "rules.json"); code != exitOK {
		t.Fatalf("rules set: code %d, stderr %q", code, tc.stderr.String())
	}
	mustContain(t, "set output", tc.stdout.String(), "Saved.", "lane-s")
	if len(f.ruleSets) != 1 {
		t.Fatalf("PUTs = %v, want one", f.ruleSets)
	}
	sent := f.ruleSets[0]
	delete(sent, "webhook")
	var want map[string]any
	_ = json.Unmarshal(saved, &want)
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("PUT body = %v, want the get --json document %v", sent, want)
	}
	// A large integer in params survives: the file is sent as written, not re-encoded.
	if !strings.Contains(string(saved), "9007199254740993") {
		t.Fatalf("get --json lost the large param: %s", saved)
	}
	if after := f.rulesDocs[hookID]; !reflect.DeepEqual(after.(map[string]any)["rules"], jsonRoundTrip(t, before["rules"])) {
		t.Fatalf("round trip changed the rules: %v", after)
	}
}

func jsonRoundTrip(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

// A file without params keeps the stored ones and says so; "params": null is sent as the clear.
func TestWebhookRulesSetParamsSemantics(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	seedRules(f)

	tc.stdin = strings.NewReader(`{"rules": [{"id": "only", "expr": "true", "action": {"queue": "triage"}}]}`)
	if code := tc.run(t, "webhook", "rules", "set", hookID, "--file", "-"); code != exitOK {
		t.Fatalf("set from stdin: code %d, stderr %q", code, tc.stderr.String())
	}
	mustContain(t, "kept note", tc.stdout.String(), "stored params were kept", "repo_prefixes")
	if _, has := f.ruleSets[0]["params"]; has {
		t.Fatalf("a file without params sent one: %v", f.ruleSets[0])
	}

	tc.stdin = strings.NewReader(`{"rules": [], "params": null}`)
	if code := tc.run(t, "webhook", "rules", "set", hookID, "-f", "-"); code != exitOK {
		t.Fatalf("set with params:null: code %d, stderr %q", code, tc.stderr.String())
	}
	if v, has := f.ruleSets[1]["params"]; !has || v != nil {
		t.Fatalf("params:null not sent as the clear: %v", f.ruleSets[1])
	}
	mustContain(t, "cleared", tc.stdout.String(), "Saved.", "No rules", "Params:   none")
}

func TestWebhookRulesSetUsageMistakes(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	seedRules(f)
	files := map[string]string{"notjson.json": "{rules", "array.json": `[]`, "norules.json": `{"params": {}}`}
	tc.readFile = func(name string) ([]byte, error) {
		if b, ok := files[name]; ok {
			return []byte(b), nil
		}
		return nil, errors.New("no such file")
	}
	for _, args := range [][]string{
		{"webhook", "rules"},
		{"webhook", "rules", "set"},
		{"webhook", "rules", "set", hookID},
		{"webhook", "rules", "set", hookID, "extra", "--file", "array.json"},
		{"webhook", "rules", "set", hookID, "--file", "notjson.json"},
		{"webhook", "rules", "set", hookID, "--file", "array.json"},
		{"webhook", "rules", "set", hookID, "--file", "norules.json"},
		{"webhook", "rules", "set", hookID, "--file", "missing.json"},
		{"webhook", "rules", "set", hookID, "--bogus"},
		{"webhook", "rules", "get"},
		{"webhook", "rules", "bogus"},
		{"webhook", "rules", "test", hookID},
		{"webhook", "rules", "test", hookID, "--event", "7", "--payload", "p.json"},
		{"webhook", "rules", "test", hookID, "--payload", "p.json", "--header", "no-colon"},
	} {
		tc.stderr.Reset()
		if code := tc.run(t, args...); code != exitUsage {
			t.Fatalf("%v: code %d, want %d (stderr %q)", args, code, exitUsage, tc.stderr.String())
		}
	}
	if len(f.ruleSets) != 0 || len(f.ruleTests) != 0 {
		t.Fatalf("usage mistakes must never reach the API: sets %v, tests %v", f.ruleSets, f.ruleTests)
	}
}

func TestWebhookRulesTestSendsTheDryRun(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	seedRules(f)
	f.testAnswer = map[string]any{
		"decision": map[string]any{"drop": false, "queue": "lane-s", "endpoints": []any{"ep-1"}, "disposition": "routed", "faulted": false},
		"trace":    map[string]any{"stage": "rule", "rule_id": "lane-s", "rule_index": 1, "action": map[string]any{"queue": "lane-s"}},
		"once_key": "issue:stump.wtf/x#7|lane-s", "work_order": map[string]any{"lane": "lane-s"},
	}
	tc.readFile = func(name string) ([]byte, error) {
		switch name {
		case "candidate.json":
			return []byte(`{"webhook_id": "ignored", "rules": [{"id": "lane-s", "expr": "true", "action": {"queue": "lane-s"}}], "params": null}`), nil
		case "payload.json":
			return []byte(`{"action": "opened", "issue": {"number": 7}}`), nil
		}
		return nil, errors.New("no such file")
	}

	// SPEC-0035 scenario "Test against a stored delivery", from a terminal.
	if code := tc.run(t, "webhook", "rules", "test", hookID, "--event", "812", "--file", "candidate.json"); code != exitOK {
		t.Fatalf("rules test --event: code %d, stderr %q", code, tc.stderr.String())
	}
	mustContain(t, "test output", tc.stdout.String(), "Routed to queue lane-s on ep-1.", "Matched:  rule lane-s",
		"Once key: issue:stump.wtf/x#7|lane-s", "Work order: attached")
	sent := f.ruleTests[0]
	if sent["event_id"] != float64(812) || sent["omit_envelope"] != true || sent["payload"] != nil || sent["webhook_id"] != nil {
		t.Fatalf("test body = %v, want event 812 without payload or the file's read-only fields", sent)
	}
	if rules, _ := sent["rules"].([]any); len(rules) != 1 {
		t.Fatalf("candidate rules not sent: %v", sent)
	}
	// "params": null in the file means "no params", as it does for set, so the dry run gets {}.
	if p, ok := sent["params"].(map[string]any); !ok || len(p) != 0 {
		t.Fatalf("params:null sent as %v, want {}", sent["params"])
	}

	// A sample payload with headers, testing the saved rules.
	if code := tc.run(t, "webhook", "rules", "test", hookID, "--payload", "@payload.json",
		"--header", "X-Gitea-Event: issues", "--envelope"); code != exitOK {
		t.Fatalf("rules test --payload: code %d, stderr %q", code, tc.stderr.String())
	}
	sent = f.ruleTests[1]
	if pl, _ := sent["payload"].(map[string]any); pl["action"] != "opened" || sent["event_id"] != nil || sent["rules"] != nil {
		t.Fatalf("payload test body = %v", sent)
	}
	if h, _ := sent["headers"].(map[string]any); h["X-Gitea-Event"] != "issues" {
		t.Fatalf("headers = %v", sent["headers"])
	}
	if _, has := sent["omit_envelope"]; has {
		t.Fatalf("--envelope still omitted it: %v", sent)
	}

	// A faulted decision says it is blocking.
	f.testAnswer = map[string]any{
		"decision": map[string]any{"faulted": true, "disposition": "faulted",
			"fault": map[string]any{"rule_id": "adder", "rule_index": 0, "cause": "error", "detail": "cannot add number and string"}},
		"trace": map[string]any{"stage": "fault"},
	}
	if code := tc.run(t, "webhook", "rules", "test", hookID, "--event", "9"); code != exitOK {
		t.Fatalf("faulted test: code %d", code)
	}
	mustContain(t, "faulted output", tc.stdout.String(), "FAULTED (blocking)", "rule adder: error — cannot add number and string")
}

func TestWebhookGroupHelp(t *testing.T) {
	tc := newTestCLI(t)
	for _, args := range [][]string{{"webhook", "-h"}, {"help", "webhook"}, {"help", "webhook", "rules"}, {"webhook", "rules", "-h"}} {
		if code := tc.run(t, args...); code != exitOK {
			t.Fatalf("%v: code %d, want %d (stderr %q)", args, code, exitOK, tc.stderr.String())
		}
		mustContain(t, "webhook help", tc.stdout.String(),
			"switchboard webhook rules get WEBHOOK_ID", "switchboard webhook rules test WEBHOOK_ID", "switchboard webhook rules set WEBHOOK_ID")
	}
	if code := tc.run(t, "help", "webhook", "rules", "set"); code != exitOK {
		t.Fatalf("help webhook rules set: code %d", code)
	}
	mustContain(t, "verb help", tc.stdout.String(), "usage: switchboard webhook rules set [flags] WEBHOOK_ID", "-file", "KEEPS the stored params")
	if code := tc.run(t, "help", "webhook", "rules", "bogus"); code != exitUsage {
		t.Fatalf("help for an unknown nested verb: code %d, want %d", code, exitUsage)
	}
	mustContain(t, "nested unknown", tc.stderr.String(), `unknown verb "bogus"`)
}

package routing

// Routing Cookbook Contract
//
// The user guide docs/guides/10-routing-cookbook.md publishes copy-paste rule sets. This test reads
// every code block titled `set_webhook_rules · NAME` straight out of that page (a title rather than an
// HTML comment, which the site's MDX build rejects), validates it exactly as a save would, and routes
// realistic GitHub, Gitea, and cairn deliveries through it — in-process and through the sandbox
// child, which must agree. A recipe edited into something that no longer does what its prose says
// fails here, and so does a recipe with no cases.
//
// Governing: SPEC-0020 REQ "Deterministic Rule Evaluation", REQ "Rule Validation at Save Time",
// REQ "Routing Trace"; ADR-0024, ADR-0025.
//
// @joestump-agent 09/11/2026 - Added with the user guides so the cookbook cannot drift from the evaluator.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const cookbookPath = "../../docs/guides/10-routing-cookbook.md"

var cookbookBlock = regexp.MustCompile("(?s)```json title=\"set_webhook_rules · ([a-z0-9-]+)\"\\n(.*?)\\n```")

// cookbookGrant is what the recipes assume: an owning endpoint granted every queue they name as a
// webhook queue, and a webhook with routes to alice's and bob's endpoints, each scoped to every queue.
func cookbookGrant() Grant {
	queues := []string{"inbox", "reviews", "small", "medium", "large", "hold", "triage"}
	return Grant{
		TargetQueue:    "inbox",
		Queues:         queues,
		Endpoints:      []string{owner, epB, epC},
		EndpointQueues: map[string][]string{owner: queues, epB: queues, epC: queues},
	}
}

// loadCookbook parses the published recipes, substituting the endpoint-id placeholders.
func loadCookbook(t *testing.T) map[string]Config {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(cookbookPath))
	if err != nil {
		t.Fatalf("read cookbook: %v", err)
	}
	doc := strings.NewReplacer("<alice endpoint id>", epB, "<bob endpoint id>", epC).Replace(string(raw))
	out := map[string]Config{}
	for _, m := range cookbookBlock.FindAllStringSubmatch(doc, -1) {
		var body struct {
			WebhookID string         `json:"webhook_id"`
			Rules     []Rule         `json:"rules"`
			Default   *Action        `json:"default_action"`
			Params    map[string]any `json:"params"`
		}
		dec := json.NewDecoder(strings.NewReader(m[2]))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			t.Fatalf("cookbook block %q is not valid set_webhook_rules JSON: %v", m[1], err)
		}
		if body.WebhookID == "" {
			t.Fatalf("cookbook block %q has no webhook_id: it must be a complete set_webhook_rules body", m[1])
		}
		if _, dup := out[m[1]]; dup {
			t.Fatalf("cookbook block %q appears twice", m[1])
		}
		out[m[1]] = Config{Rules: body.Rules, Default: body.Default, Params: body.Params}
	}
	if len(out) == 0 {
		t.Fatal("no cookbook blocks found; the title format changed")
	}
	return out
}

type cookbookCase struct {
	name    string
	source  string
	headers map[string]string
	body    string
	want    Decision
	// params, when non-nil, replaces the recipe's published params for this case (a mistyped or
	// missing allowlist must fail closed).
	params map[string]any
}

func gh(event string) map[string]string {
	return map[string]string{"X-GitHub-Event": event, "X-GitHub-Delivery": "d-1", "Content-Type": "application/json"}
}

func gt(event string) map[string]string {
	return map[string]string{"X-Gitea-Event": event, "X-GitHub-Event": event, "X-Gitea-Delivery": "d-1", "Content-Type": "application/json"}
}

var everyTarget = []string{owner, epB, epC}

func routed(queue, ruleID string, eps ...string) Decision {
	if len(eps) == 0 {
		eps = everyTarget
	}
	return Decision{Queue: queue, Endpoints: eps, Trace: Trace{Stage: StageRule, RuleID: ruleID}}
}

func once(d Decision) Decision      { d.Once = true; return d }
func workOrder(d Decision) Decision { d.WorkOrder = true; return d }

func dropped(ruleID string) Decision {
	return Decision{Drop: true, Trace: Trace{Stage: StageRule, RuleID: ruleID}}
}

func droppedByDefault() Decision {
	return Decision{Drop: true, Trace: Trace{Stage: StageDefault, Cause: CauseNoMatch}}
}

func toTargetQueue() Decision {
	return Decision{Queue: "inbox", Endpoints: everyTarget, Trace: Trace{Stage: StageDefault, Cause: CauseNoMatch}}
}

func issue(action, author, sender string, labels ...string) string {
	ls := make([]map[string]any, 0, len(labels))
	for i, l := range labels {
		ls = append(ls, map[string]any{"id": 500 + i, "name": l})
	}
	b, _ := json.Marshal(map[string]any{
		"action": action,
		"issue": map[string]any{"number": 7, "title": "flaky test", "state": "open", "html_url": "https://forge.example/o/r/issues/7",
			"user": map[string]string{"login": author}, "labels": ls},
		"repository": map[string]any{"full_name": "o/r"},
		"sender":     map[string]string{"login": sender},
	})
	return string(b)
}

func reviewRequest(action, author, reviewer string) string {
	body := map[string]any{
		"action":       action,
		"number":       306,
		"pull_request": map[string]any{"number": 306, "user": map[string]string{"login": author}},
		"sender":       map[string]string{"login": author},
	}
	if reviewer != "" {
		body["requested_reviewer"] = map[string]string{"login": reviewer}
	} else if action == "review_requested" {
		body["requested_team"] = map[string]string{"name": "maintainers"}
	}
	b, _ := json.Marshal(body)
	return string(b)
}

func cairnArtifactBody(kind, actor, title string, tags ...string) string {
	data := map[string]any{"id": "hx7Qm2", "share_type": "markdown", "title": title, "url": "https://cairn.example/a/hx7Qm2",
		"channel": "mcp", "actor_id": actor, "on_behalf_of": "claude-code/2.1.0"}
	if tags != nil {
		data["tags"] = tags
	}
	b, _ := json.Marshal(map[string]any{
		"source": "cairn", "kind": kind, "event_id": "8f14e45f-ceea-4e7d-a0c6-3b2f8a0d9c11", "created_at": "2026-09-11T08:30:00Z", "data": data,
	})
	return string(b)
}

var cairnHeaders = map[string]string{"X-Cairn-Event": "artifact.created", "Content-Type": "application/json"}

func cookbookCases() map[string][]cookbookCase {
	const alice = "alice@example.com"
	return map[string][]cookbookCase{
		"drop-ci-noise": {
			{name: "github ping", source: "github", headers: gh("ping"), body: `{"zen":"Keep it logically awesome.","hook_id":1}`, want: dropped("ping")},
			{name: "github workflow_run", source: "github", headers: gh("workflow_run"), body: `{"action":"requested","workflow_run":{"id":1}}`, want: dropped("ci-noise")},
			{name: "gitea workflow_job", source: "gitea", headers: gt("workflow_job"), body: `{"action":"completed"}`, want: dropped("ci-noise")},
			{name: "github status", source: "github", headers: gh("status"), body: `{"state":"success"}`, want: dropped("ci-noise")},
			{name: "an issue still reaches the target queue", source: "github", headers: gh("issues"), body: issue("opened", "alice", "alice"), want: toTargetQueue()},
			{name: "a generic delivery has no kind and is untouched", source: "generic", headers: map[string]string{"Content-Type": "application/json"}, body: `{"hello":"switchboard"}`, want: toTargetQueue()},
		},
		"size-lanes": {
			{name: "gitea label_updated to size/M", source: "gitea", headers: gt("issues"), body: issue("label_updated", "alice", "bob", "size/M"), want: once(routed("medium", "size-m"))},
			{name: "github labeled size/S", source: "github", headers: gh("issues"), body: issue("labeled", "alice", "bob", "bug", "size/S"), want: once(routed("small", "size-s"))},
			{name: "github opened already size/L", source: "github", headers: gh("issues"), body: issue("opened", "alice", "alice", "size/L"), want: once(routed("large", "size-l"))},
			{name: "size/XL is held", source: "gitea", headers: gt("issues"), body: issue("reopened", "alice", "alice", "size/XL"), want: once(routed("hold", "size-xl"))},
			{name: "two sizes: the first rule wins", source: "gitea", headers: gt("issues"), body: issue("label_updated", "alice", "bob", "size/L", "size/S"), want: once(routed("small", "size-s"))},
			{name: "opened with no labels goes to triage", source: "github", headers: gh("issues"), body: issue("opened", "alice", "alice"), want: once(routed("triage", "unsized"))},
			{name: "opened with only non-size labels goes to triage", source: "gitea", headers: gt("issues"), body: issue("opened", "alice", "alice", "bug"), want: once(routed("triage", "unsized"))},
			{name: "a non-size label on an unsized issue is dropped", source: "github", headers: gh("issues"), body: issue("labeled", "alice", "bob", "bug"), want: droppedByDefault()},
			{name: "closing a sized issue is dropped", source: "github", headers: gh("issues"), body: issue("closed", "alice", "alice", "size/S"), want: droppedByDefault()},
			{name: "a pull request has no .issue and is dropped", source: "github", headers: gh("pull_request"), body: reviewRequest("opened", "alice", "bob"), want: droppedByDefault()},
			{name: "a generic delivery shaped like an issue is dropped", source: "generic", headers: map[string]string{"Content-Type": "application/json"}, body: issue("opened", "alice", "alice", "size/S"), want: droppedByDefault()},
		},
		"trusted-authors": {
			{name: "trusted author and sender pass through", source: "github", headers: gh("issues"), body: issue("opened", "alice", "alice"), want: toTargetQueue()},
			{name: "a stranger's issue is dropped", source: "gitea", headers: gt("issues"), body: issue("opened", "mallory", "mallory"), want: dropped("untrusted")},
			{name: "a stranger labeling a trusted issue is dropped", source: "github", headers: gh("issues"), body: issue("labeled", "alice", "mallory", "size/S"), want: dropped("untrusted")},
			{name: "a trusted labeler cannot promote a stranger's issue", source: "github", headers: gh("issues"), body: issue("labeled", "mallory", "alice", "size/S"), want: dropped("untrusted")},
			{name: "a trusted comment on a trusted issue passes", source: "github", headers: gh("issue_comment"), body: issue("created", "alice", "bob"), want: toTargetQueue()},
			{name: "a trusted pull request passes", source: "gitea", headers: gt("pull_request"), body: reviewRequest("opened", "bob", ""), want: toTargetQueue()},
			{name: "a stranger's pull request is dropped", source: "github", headers: gh("pull_request"), body: reviewRequest("opened", "mallory", ""), want: dropped("untrusted")},
			{name: "missing params trust no one", source: "github", headers: gh("issues"), body: issue("opened", "alice", "alice"), want: dropped("untrusted"), params: map[string]any{}},
			{name: "a string instead of a list trusts no one", source: "github", headers: gh("issues"), body: issue("opened", "alice", "alice"), want: dropped("untrusted"), params: map[string]any{"trusted": "alice"}},
			{name: "a number instead of a list trusts no one", source: "github", headers: gh("issues"), body: issue("opened", "alice", "alice"), want: dropped("untrusted"), params: map[string]any{"trusted": 7}},
		},
		"review-requests": {
			{name: "gitea: bob asked to review alice's PR reaches only bob", source: "gitea", headers: gt("pull_request"), body: reviewRequest("review_requested", "alice", "bob"), want: routed("reviews", "review-bob", epC)},
			{name: "github: alice asked to review bob's PR reaches only alice", source: "github", headers: gh("pull_request"), body: reviewRequest("review_requested", "bob", "alice"), want: routed("reviews", "review-alice", epB)},
			{name: "a review of your own PR is never delivered", source: "gitea", headers: gt("pull_request"), body: reviewRequest("review_requested", "alice", "alice"), want: dropped("self-review")},
			{name: "a team request lands nowhere", source: "github", headers: gh("pull_request"), body: reviewRequest("review_requested", "alice", ""), want: dropped("other-review-requests")},
			{name: "a removed request lands nowhere", source: "gitea", headers: gt("pull_request"), body: reviewRequest("review_request_removed", "alice", "bob"), want: dropped("other-review-requests")},
			{name: "a request for someone without a rule lands nowhere", source: "github", headers: gh("pull_request"), body: reviewRequest("review_requested", "alice", "carol"), want: dropped("other-review-requests")},
			{name: "other pull request activity is dropped", source: "github", headers: gh("pull_request"), body: reviewRequest("opened", "alice", "bob"), want: droppedByDefault()},
		},
		"review-requests-own-webhook": {
			{name: "a request for alice reaches alice's pool", source: "gitea", headers: gt("pull_request"), body: reviewRequest("review_requested", "bob", "alice"), want: toTargetQueue()},
			{name: "a request for bob is dropped on alice's webhook", source: "github", headers: gh("pull_request"), body: reviewRequest("review_requested", "alice", "bob"), want: dropped("review-request-not-for-me")},
			{name: "alice opening her own PR is not a review trigger", source: "gitea", headers: gt("pull_request"), body: reviewRequest("opened", "alice", ""), want: dropped("own-pr-review-trigger")},
			{name: "alice pushing to her own PR is not a review trigger", source: "github", headers: gh("pull_request"), body: reviewRequest("synchronize", "alice", ""), want: dropped("own-pr-review-trigger")},
			{name: "bob's PR activity still reaches the pool", source: "github", headers: gh("pull_request"), body: reviewRequest("opened", "bob", ""), want: toTargetQueue()},
			{name: "issues still reach the pool", source: "github", headers: gh("issues"), body: issue("opened", "bob", "bob"), want: toTargetQueue()},
			{name: "without an identity every review request fails closed", source: "gitea", headers: gt("pull_request"), body: reviewRequest("review_requested", "bob", "alice"), want: dropped("review-request-not-for-me"), params: map[string]any{}},
		},
		"renovate-dashboard": {
			{name: "github dashboard edit", source: "github", headers: gh("issues"), body: `{"action":"edited","issue":{"title":"Dependency Dashboard","user":{"login":"renovate[bot]"}},"sender":{"login":"renovate[bot]"}}`, want: dropped("renovate-dashboard")},
			{name: "gitea renovate-bot issue", source: "gitea", headers: gt("issues"), body: `{"action":"opened","issue":{"title":"Action Required: Fix Renovate Configuration","user":{"login":"renovate-bot"}},"sender":{"login":"renovate-bot"}}`, want: dropped("renovate-dashboard")},
			{name: "a comment on the dashboard", source: "github", headers: gh("issue_comment"), body: `{"action":"created","issue":{"title":"Dependency Dashboard","user":{"login":"renovate[bot]"}},"sender":{"login":"alice"}}`, want: dropped("renovate-dashboard")},
			{name: "a person's issue passes through", source: "github", headers: gh("issues"), body: issue("opened", "alice", "alice"), want: toTargetQueue()},
		},
		"cairn-handoff": {
			{name: "a trusted lane:m handoff reaches medium with a work order", source: "cairn", headers: cairnHeaders, body: cairnArtifactBody("artifact.created", alice, "Handoff: fix the reaper test", "handoff", "lane:m", "size:m"), want: workOrder(once(routed("medium", "lane-m")))},
			{name: "a trusted lane:s handoff reaches small", source: "cairn", headers: cairnHeaders, body: cairnArtifactBody("artifact.created", alice, "x", "lane:s", "handoff"), want: workOrder(once(routed("small", "lane-s")))},
			{name: "a trusted handoff with no lane goes to triage", source: "cairn", headers: cairnHeaders, body: cairnArtifactBody("artifact.created", alice, "x", "handoff", "lane:auto"), want: workOrder(once(routed("triage", "handoff-triage")))},
			{name: "an untagged artifact is dropped", source: "cairn", headers: cairnHeaders, body: cairnArtifactBody("artifact.created", alice, "notes", "standup"), want: dropped("not-a-handoff")},
			{name: "a cairn without tags drops everything", source: "cairn", headers: cairnHeaders, body: cairnArtifactBody("artifact.created", alice, "Handoff: x"), want: dropped("not-a-handoff")},
			{name: "a stranger's handoff is dropped whatever it is tagged", source: "cairn", headers: cairnHeaders, body: cairnArtifactBody("artifact.created", "mallory", "x", "handoff", "lane:l", "trusted"), want: dropped("untrusted-actor")},
			{name: "a mistyped allowlist trusts no one", source: "cairn", headers: cairnHeaders, body: cairnArtifactBody("artifact.created", alice, "x", "handoff", "lane:m"), want: dropped("untrusted-actor"), params: map[string]any{"cairn_actors": alice}},
		},
		"cairn-title-handoff": {
			{name: "a trusted [handoff:reviews] artifact reaches reviews", source: "cairn", headers: cairnHeaders, body: cairnArtifactBody("artifact.created", alice, "[handoff:reviews] audit the backup job"), want: workOrder(once(routed("reviews", "handoff-reviews")))},
			{name: "the same title from anyone else is dropped", source: "cairn", headers: cairnHeaders, body: cairnArtifactBody("artifact.created", "mallory", "[handoff:reviews] audit the backup job"), want: droppedByDefault()},
			{name: "a trusted ordinary artifact is dropped", source: "cairn", headers: cairnHeaders, body: cairnArtifactBody("artifact.created", alice, "notes from standup"), want: droppedByDefault()},
			{name: "without an allowlist nothing routes", source: "cairn", headers: cairnHeaders, body: cairnArtifactBody("artifact.created", alice, "[handoff:reviews] x"), want: droppedByDefault(), params: map[string]any{}},
		},
	}
}

func cookbookInput(c cookbookCase) EnvelopeInput {
	h := http.Header{}
	for k, v := range c.headers {
		h.Set(k, v)
	}
	trust, verified := "signed", true
	if c.source == "generic" {
		trust, verified = "token", false
	}
	return EnvelopeInput{
		Source: c.source, Kind: EventKind(c.source, h.Get, []byte(c.body)), WebhookID: "wh-cookbook",
		TrustMode: trust, Verified: verified, ContentType: h.Get("Content-Type"), Headers: c.headers, Body: []byte(c.body),
	}
}

func TestRoutingCookbook(t *testing.T) {
	recipes := loadCookbook(t)
	cases := cookbookCases()
	for name := range recipes {
		if len(cases[name]) == 0 {
			t.Errorf("cookbook recipe %q has no test cases", name)
		}
	}
	sb := testSandbox(t)
	g := cookbookGrant()
	for name, cs := range cases {
		recipe, ok := recipes[name]
		if !ok {
			t.Errorf("test cases exist for %q but the cookbook no longer publishes it", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			if err := Validate(recipe, g); err != nil {
				t.Fatalf("published recipe does not save: %v", err)
			}
			for _, c := range cs {
				cfg := recipe
				if c.params != nil {
					cfg.Params = c.params
				}
				in := cookbookInput(c)
				got := InProcess{}.Route(context.Background(), cfg, g, in)
				// A fault is only acceptable where a case deliberately breaks the params.
				if len(got.Trace.Faults) > 0 && c.params == nil {
					t.Errorf("%s: rules faulted: %+v", c.name, got.Trace.Faults)
				}
				if got.Drop != c.want.Drop || got.Queue != c.want.Queue || !slices.Equal(got.Endpoints, c.want.Endpoints) ||
					got.Once != c.want.Once || got.WorkOrder != c.want.WorkOrder ||
					got.Trace.Stage != c.want.Trace.Stage || got.Trace.Cause != c.want.Trace.Cause || got.Trace.RuleID != c.want.Trace.RuleID {
					t.Errorf("%s:\n got drop=%v queue=%q endpoints=%v once=%v work_order=%v stage=%s cause=%s rule=%s\nwant drop=%v queue=%q endpoints=%v once=%v work_order=%v stage=%s cause=%s rule=%s",
						c.name, got.Drop, got.Queue, got.Endpoints, got.Once, got.WorkOrder, got.Trace.Stage, got.Trace.Cause, got.Trace.RuleID,
						c.want.Drop, c.want.Queue, c.want.Endpoints, c.want.Once, c.want.WorkOrder, c.want.Trace.Stage, c.want.Trace.Cause, c.want.Trace.RuleID)
				}
				if child := sb.Route(context.Background(), cfg, g, in); !reflect.DeepEqual(child, got) {
					t.Errorf("%s: sandbox decision differs from in-process:\n got %+v\nwant %+v", c.name, child, got)
				}
			}
		})
	}
}

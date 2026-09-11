package routing

// Unit tests for subjects, at-most-once keys, work orders, and the .issue / cairn envelope fields.
//
// Governing: ADR-0025, SPEC-0020 REQ "Work Orders", REQ "At-Most-Once Work Orders".

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

func sampleInput(t *testing.T, name string, patch map[string]any) EnvelopeInput {
	t.Helper()
	return caseInput(t, packCase{Sample: name, Patch: patch})
}

func TestSubjectOfForgeIssues(t *testing.T) {
	gitea := sampleInput(t, "gitea-issue-label-updated", nil)
	s := SubjectOf(gitea.Source, gitea.Headers, gitea.Body)
	if s == nil || s.Type != SubjectIssue || s.Provider != "gitea" || s.Repo != "stump.wtf/switchboard" || s.Number != 212 ||
		s.Author != "joestump" || s.Sender != "joestump-agent" || s.EventType != "issue_label" || s.Action != "label_updated" ||
		s.URL != "https://gitea.stump.rocks/stump.wtf/switchboard/issues/212" {
		t.Fatalf("gitea subject = %+v", s)
	}
	if !slices.Equal(s.Labels, []string{"size/M"}) {
		t.Fatalf("gitea labels = %v", s.Labels)
	}

	github := sampleInput(t, "github-issues-labeled", nil)
	gs := SubjectOf(github.Source, github.Headers, github.Body)
	if gs == nil || gs.Provider != "github" || gs.Label != "size/S" || gs.Repo != "joestump/claude-skills" || gs.Number != 41 {
		t.Fatalf("github subject = %+v", gs)
	}

	// A generic (token) webhook carrying forge headers is recognized too, with the provider inferred.
	generic := sampleInput(t, "gitea-issue-opened", nil)
	if gen := SubjectOf("generic", generic.Headers, generic.Body); gen == nil || gen.Provider != "gitea" {
		t.Fatalf("generic-with-gitea-headers subject = %+v", gen)
	}

	// Pull requests are not issues, even as label events or review requests.
	for _, name := range []string{"gitea-pull-request-label", "gitea-pull-request-review-requested"} {
		pr := sampleInput(t, name, nil)
		if s := SubjectOf(pr.Source, pr.Headers, pr.Body); s != nil {
			t.Fatalf("%s produced subject %+v", name, s)
		}
	}
	if s := SubjectOf("gitea", map[string]string{"X-Gitea-Event": "issues"}, []byte("not json")); s != nil {
		t.Fatalf("non-JSON body produced subject %+v", s)
	}
	if s := SubjectOf("stripe", generic.Headers, generic.Body); s != nil {
		t.Fatalf("a non-forge source produced subject %+v", s)
	}
}

func TestSubjectOfCairn(t *testing.T) {
	in := sampleInput(t, "cairn-artifact-created", map[string]any{"data": map[string]any{
		"tags": []any{"handoff", "lane:m", 7, map[string]any{"x": 1}, "reply:cairn-comment"},
	}})
	s := SubjectOf(in.Source, in.Headers, in.Body)
	if s == nil || s.Type != SubjectCairnArtifact || s.Handle != "mcp://cairn/hx7Qm2" || s.ActorID != "joestump-agent" ||
		s.OnBehalfOf != "claude-code/2.1.0" || s.Key() != "cairn:hx7Qm2" {
		t.Fatalf("cairn subject = %+v", s)
	}
	if !slices.Equal(s.Tags, []string{"handoff", "lane:m", "reply:cairn-comment"}) {
		t.Fatalf("cairn tags = %v, want only the string entries, in order", s.Tags)
	}
	untagged := sampleInput(t, "cairn-artifact-created", map[string]any{"data": map[string]any{"tags": nil}})
	if s := SubjectOf(untagged.Source, untagged.Headers, untagged.Body); s == nil || len(s.Tags) != 0 {
		t.Fatalf("untagged cairn subject = %+v", s)
	}
}

// The once key is the same for every delivery about one issue routed to one queue — the opened event
// and the later label event collapse — and differs per queue, so a re-size still re-routes.
func TestOnceKey(t *testing.T) {
	opened := sampleInput(t, "gitea-issue-opened", nil)
	labeled := sampleInput(t, "gitea-issue-label-updated", nil)
	a := OnceKey(SubjectOf(opened.Source, opened.Headers, opened.Body), "lane-m")
	b := OnceKey(SubjectOf(labeled.Source, labeled.Headers, labeled.Body), "lane-m")
	c := OnceKey(SubjectOf(labeled.Source, labeled.Headers, labeled.Body), "lane-l")
	if a == "" || a != b || a == c || !strings.HasPrefix(a, "once:") {
		t.Fatalf("once keys = %q %q %q; want same subject+queue equal, other queue different", a, b, c)
	}
	if OnceKey(nil, "triage") != "" {
		t.Fatalf("a nil subject must not key")
	}
}

func TestBuildWorkOrder(t *testing.T) {
	in := sampleInput(t, "cairn-artifact-created", nil)
	idx := 7
	d := Decision{Queue: "lane-m", Endpoints: []string{"ep"}, WorkOrder: true,
		Trace: Trace{Stage: StageRule, RuleIndex: &idx, RuleID: "cairn-lane-m", RuleName: "handoff pinned to lane:m"}}
	wo := BuildWorkOrder(d, in, SubjectOf(in.Source, in.Headers, in.Body))
	raw, err := json.Marshal(wo)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	subject, _ := m["subject"].(map[string]any)
	auth, _ := m["authorized_by"].(map[string]any)
	tags, _ := subject["tags"].([]any)
	if m["version"] != float64(WorkOrderVersion) || m["lane"] != "lane-m" || m["verified"] != true ||
		m["trust_mode"] != "signed" || subject["handle"] != "mcp://cairn/hx7Qm2" || subject["actor_id"] != "joestump-agent" ||
		subject["on_behalf_of"] != "claude-code/2.1.0" || len(tags) != 7 || auth["rule_id"] != "cairn-lane-m" {
		t.Fatalf("work order = %s", raw)
	}
	// Semi-trust travels with every work order, verbatim.
	authority, _ := m["authority"].(string)
	for _, must := range []string{"semi-trusted", "prompt injection", "never disclose secrets", "never expand scope", "clamps"} {
		if !strings.Contains(authority, must) {
			t.Fatalf("authority %q is missing %q", authority, must)
		}
	}
}

func TestEnvelopeIssueAndCairnProjections(t *testing.T) {
	env := Envelope(sampleInput(t, "gitea-issue-label-updated", nil))
	for _, expr := range []string{
		`.issue.provider == "gitea" and .issue.repo == "stump.wtf/switchboard" and .issue.number == 212`,
		`.issue.action == "label_updated" and .issue.label_event and .issue.event_type == "issue_label"`,
		`.issue.author == "joestump" and .issue.sender == "joestump-agent" and .issue.state == "open"`,
		`.issue.labels == ["size/M"] and .issue.label == null and .issue.body_size > 0`,
		`.issue.url == "https://gitea.stump.rocks/stump.wtf/switchboard/issues/212" and .issue.key == "gitea:stump.wtf/switchboard#212"`,
		`.kind == "issues" and .artifact == null`,
	} {
		if !matches(t, env, expr) {
			t.Fatalf("gitea envelope: %s did not match", expr)
		}
	}
	if !matches(t, Envelope(sampleInput(t, "github-issues-labeled", nil)), `.issue.label == "size/S" and .issue.provider == "github"`) {
		t.Fatalf("github .issue.label missing")
	}
	if !matches(t, Envelope(sampleInput(t, "gitea-pull-request-label", nil)), `.issue == null`) {
		t.Fatalf("a pull request projected as an issue")
	}
	cairn := Envelope(sampleInput(t, "cairn-artifact-created", nil))
	for _, expr := range []string{
		`.artifact.tags | index("handoff") != null`,
		`[.artifact.tags[]? | select(startswith("lane:"))][0] == "lane:m"`,
		`.artifact.on_behalf_of == "claude-code/2.1.0" and .artifact.handle == "mcp://cairn/hx7Qm2" and .artifact.actor_id == "joestump-agent"`,
		`.issue == null and .artifact.labels == null`,
	} {
		if !matches(t, cairn, expr) {
			t.Fatalf("cairn envelope: %s did not match", expr)
		}
	}
}

// $params is bound in every expression; nothing else is, and an unset params object is empty rather
// than a compile error.
func TestParamsVariable(t *testing.T) {
	g := grant()
	cfg := Config{
		Params: map[string]any{"allow": []any{"joestump"}, "n": 3},
		Rules:  []Rule{rule("p", `($params.allow | index("joestump")) != null and $params.n == 3`, Action{Queue: "forge"})},
	}
	if err := Validate(cfg, g); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if d := Evaluate(context.Background(), cfg, g, event(`{}`)); d.Queue != "forge" {
		t.Fatalf("params rule = %+v", d)
	}
	// Unset params bind an empty object, so `$params.allow` is null, `null | index(...)` is null, and
	// an allowlist rule simply does not match: fail closed, with no fault to chase.
	cfg.Params = nil
	if d := Evaluate(context.Background(), cfg, g, event(`{}`)); d.Trace.Stage != StageDefault || d.Trace.Cause != CauseNoMatch {
		t.Fatalf("unset params = %+v, want the allowlist rule not to match and the default to apply", d)
	}
	big := Config{Params: map[string]any{"blob": strings.Repeat("x", MaxParamsBytes)}}
	if err := Validate(big, g); err == nil || !strings.HasPrefix(err.Error(), "params: ") {
		t.Fatalf("oversized params = %v, want a params validation error", err)
	}
}

func TestExclusiveAndWorkOrderFlagsValidate(t *testing.T) {
	g := grant()
	g.EndpointQueues = map[string][]string{owner: {"inbox"}, epB: {"forge"}, epC: {"forge"}}
	for name, a := range map[string]Action{
		"drop with exclusive":  {Drop: true, Exclusive: true},
		"drop with once":       {Drop: true, Once: true},
		"drop with work order": {Drop: true, WorkOrder: true},
	} {
		if err := Validate(Config{Rules: []Rule{rule("r", "true", a)}}, g); err == nil {
			t.Fatalf("%s validated", name)
		}
	}
	if err := Validate(Config{Rules: []Rule{rule("r", "true", Action{Queue: "handoff", Exclusive: true})}}, g); err == nil {
		t.Fatalf("exclusive to a queue no target is scoped to validated")
	}
	ok := Config{Rules: []Rule{rule("r", "true", Action{Queue: "forge", Exclusive: true, Once: true, WorkOrder: true})}}
	if err := Validate(ok, g); err != nil {
		t.Fatalf("valid exclusive rule rejected: %v", err)
	}
	d := Evaluate(context.Background(), ok, g, event(`{}`))
	if len(d.Endpoints) != 1 || d.Endpoints[0] != epB || !d.Once || !d.WorkOrder {
		t.Fatalf("exclusive decision = %+v, want only %s (first scoped in grant order)", d, epB)
	}
	// Narrowing and exclusivity compose: endpoints pick the candidates, exclusivity picks one of them.
	narrowed := Config{Rules: []Rule{rule("r", "true", Action{Queue: "forge", Endpoints: []string{epC}, Exclusive: true})}}
	if d := Evaluate(context.Background(), narrowed, g, event(`{}`)); len(d.Endpoints) != 1 || d.Endpoints[0] != epC {
		t.Fatalf("narrowed exclusive = %+v, want only %s", d, epC)
	}
	// A scope that shrinks after save sends the delivery to the default, never to an unscoped target.
	g.EndpointQueues = map[string][]string{owner: {"inbox"}}
	if d := Evaluate(context.Background(), ok, g, event(`{}`)); d.Trace.Cause != CauseRuleNotGranted {
		t.Fatalf("exclusive with no scoped target = %+v, want %s", d, CauseRuleNotGranted)
	}
}

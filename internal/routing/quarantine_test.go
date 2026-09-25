package routing

// The quarantine action and .release (SPEC-0026 REQ-6, REQ-7, REQ-10): {"quarantine": true} is a
// third terminal action that takes no flags, "quarantine" is refused as a rule's queue, a matching
// quarantine rule holds the delivery, and a released delivery carries .release and released_by.

import (
	"context"
	"strings"
	"testing"
)

func TestQuarantineActionValidation(t *testing.T) {
	bad := map[string]Action{
		"reserved queue":            {Queue: QueueQuarantine},
		"quarantine and drop":       {Quarantine: true, Drop: true},
		"quarantine and queue":      {Quarantine: true, Queue: "forge"},
		"quarantine with endpoints": {Quarantine: true, Endpoints: []string{owner}},
		"quarantine with once":      {Quarantine: true, Once: true},
		"nothing":                   {},
	}
	for name, a := range bad {
		err := Validate(Config{Rules: []Rule{rule("r", `true`, a)}}, grant())
		if err == nil {
			t.Fatalf("%s accepted", name)
		}
		if ve := validationCode(t, err); ve.Code != CodeInvalidRule {
			t.Fatalf("%s: code %s, want %s", name, ve.Code, CodeInvalidRule)
		}
	}
	if err := Validate(Config{Rules: []Rule{rule("r", `true`, Action{Quarantine: true})}, Default: &Action{Quarantine: true}}, grant()); err != nil {
		t.Fatalf("quarantine action refused: %v", err)
	}
	if err := Validate(Config{Rules: []Rule{rule("r", `true`, Action{Queue: QueueQuarantine})}}, grant()); !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved-name error %v does not say why", err)
	}
	if QueueGranted(QueueQuarantine, Grant{TargetQueue: QueueQuarantine, Queues: []string{QueueQuarantine}}) {
		t.Fatal("the quarantine queue was granted")
	}
}

func TestQuarantineActionDecision(t *testing.T) {
	cfg := Config{Rules: []Rule{rule("outsiders", `.actor.author_trusted == false`, Action{Quarantine: true})},
		Default: &Action{Queue: "forge"}}
	f := false
	env := Envelope(EnvelopeInput{Source: "github", Actor: &ActorTrust{AuthorTrusted: &f}})
	d := Evaluate(context.Background(), cfg, grant(), env)
	if !d.Quarantine || d.Queue != "" || len(d.Endpoints) != 0 || d.Disposition() != DispositionQuarantined ||
		d.QuarantineReason() != QuarantineRuleAction || d.Trace.RuleID != "outsiders" {
		t.Fatalf("decision = %+v, want quarantined by rule outsiders", d)
	}
	faulted := Evaluate(context.Background(), Config{Rules: []Rule{rule("boom", `error("x")`, Action{Drop: true})}}, grant(), env)
	if faulted.Disposition() != DispositionFaulted || faulted.QuarantineReason() != QuarantineRuleFault {
		t.Fatalf("faulted decision = %+v, want the faulted disposition held as rule_fault", faulted)
	}
	untrusted := UntrustedDecision(&ActorTrust{})
	if untrusted.Disposition() != DispositionQuarantined || untrusted.QuarantineReason() != QuarantineUntrustedActor {
		t.Fatalf("untrusted decision = %+v", untrusted)
	}
	if r := Evaluate(context.Background(), Config{}, grant(), env).QuarantineReason(); r != "" {
		t.Fatalf("a routed decision reports quarantine reason %q", r)
	}
}

// REQ-10: .release is null on a fresh delivery and {by, at} on a released one; rules can read it; the
// work order carries released_by.
func TestReleaseEnvelopeAndWorkOrder(t *testing.T) {
	fresh := Envelope(EnvelopeInput{Source: "github"})
	if fresh["release"] != nil {
		t.Fatalf("fresh .release = %v, want null", fresh["release"])
	}
	in := EnvelopeInput{Source: "github", Release: &Release{By: "human:h1", At: "2026-09-25T12:00:00Z"}}
	env := Envelope(in)
	rel, _ := env["release"].(map[string]any)
	if rel["by"] != "human:h1" || rel["at"] != "2026-09-25T12:00:00Z" {
		t.Fatalf(".release = %v", env["release"])
	}
	cfg := Config{Rules: []Rule{rule("by-human", `.release.by | startswith("human:")`, Action{Queue: "forge"})}}
	if d := Evaluate(context.Background(), cfg, grant(), env); d.Queue != "forge" {
		t.Fatalf("decision = %+v, want the release rule to match", d)
	}
	if wo := BuildWorkOrder(Decision{Queue: "forge"}, in, nil); wo.ReleasedBy != "human:h1" {
		t.Fatalf("work order released_by = %q", wo.ReleasedBy)
	}
}

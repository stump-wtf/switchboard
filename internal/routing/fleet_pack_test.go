package routing

// The checked-in fleet rule pack (docs/routing/rule-packs/fleet.json), tested as installed: loaded
// from the file an operator hands to set_webhook_rules, validated against a lanes grant, and run
// through the real evaluator — in-process for every case, and through the sandbox child for all of
// them — against realistic Gitea, GitHub, and cairn deliveries (testdata/fleet). If a case here fails,
// the pack in docs routes differently than its README says.
//
// Governing: ADR-0025, SPEC-0020 REQ "Work Orders", REQ "At-Most-Once Work Orders"; ADR-0024.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

const packPath = "../../docs/routing/rule-packs/fleet.json"

var laneQueues = []string{"triage", "lane-local", "lane-zai-flash", "lane-zai", "lane-hyper", "lane-vision", "hold"}

// laneGrant models the production topology: a router endpoint owns the webhook (scoped to no lane)
// and one pool endpoint per lane is routed to it, each scoped to exactly its lane queue.
func laneGrant() (Grant, map[string]string) {
	g := Grant{TargetQueue: "triage", Queues: laneQueues, EndpointQueues: map[string][]string{}}
	byQueue := map[string]string{}
	router := "00000000-0000-0000-0000-00000000000a"
	g.Endpoints = append(g.Endpoints, router)
	g.EndpointQueues[router] = []string{"router"}
	for i, q := range laneQueues {
		ep := "00000000-0000-0000-0000-00000000010" + string(rune('0'+i))
		g.Endpoints = append(g.Endpoints, ep)
		g.EndpointQueues[ep] = []string{q}
		byQueue[q] = ep
	}
	return g, byQueue
}

func loadPack(t *testing.T) Config {
	t.Helper()
	raw, err := os.ReadFile(packPath)
	if err != nil {
		t.Fatalf("read pack: %v", err)
	}
	var cfg Config
	dec := json.NewDecoder(bytesReader(raw))
	dec.DisallowUnknownFields() // the pack must be exactly a set_webhook_rules body (less webhook_id)
	if err := dec.Decode(&cfg); err != nil {
		t.Fatalf("decode pack: %v", err)
	}
	return cfg
}

type packSample struct {
	Source  string            `json:"source"`
	Headers map[string]string `json:"headers"`
	Body    map[string]any    `json:"body"`
}

type packCase struct {
	Name      string         `json:"name"`
	Sample    string         `json:"sample"`
	Patch     map[string]any `json:"patch"`
	Verified  *bool          `json:"verified"`
	TrustMode string         `json:"trust_mode"`
	Want      struct {
		Drop  bool   `json:"drop"`
		Queue string `json:"queue"`
		Rule  string `json:"rule"`
	} `json:"want"`
}

func loadCases(t *testing.T) []packCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/fleet/cases.json")
	if err != nil {
		t.Fatalf("read cases: %v", err)
	}
	var cases []packCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("decode cases: %v", err)
	}
	return cases
}

// caseInput builds the envelope input for a case: the sample, deep-merged with the case's patch
// (objects merge, everything else replaces), verified and signed unless the case says otherwise.
func caseInput(t *testing.T, c packCase) EnvelopeInput {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata/fleet/samples", c.Sample+".json"))
	if err != nil {
		t.Fatalf("read sample %s: %v", c.Sample, err)
	}
	var s packSample
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode sample %s: %v", c.Sample, err)
	}
	body, err := json.Marshal(deepMerge(s.Body, c.Patch))
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	in := EnvelopeInput{Source: s.Source, WebhookID: "wh-router", TrustMode: "signed", Verified: true,
		ContentType: "application/json", Headers: s.Headers, Body: body}
	in.Kind = EventKind(s.Source, func(k string) string { return headerValue(s.Headers, k) }, body)
	if c.Verified != nil {
		in.Verified = *c.Verified
	}
	if c.TrustMode != "" {
		in.TrustMode = c.TrustMode
	}
	return in
}

func deepMerge(base, patch map[string]any) map[string]any {
	out := make(map[string]any, len(base))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range patch {
		if pm, ok := v.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = deepMerge(bm, pm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

func checkCase(t *testing.T, c packCase, d Decision, byQueue map[string]string) {
	t.Helper()
	if c.Want.Rule != "" && d.Trace.RuleID != c.Want.Rule {
		t.Fatalf("%s: matched rule %q (%s), want %q; faults %+v", c.Name, d.Trace.RuleID, d.Trace.Cause, c.Want.Rule, d.Trace.Faults)
	}
	if len(d.Trace.Faults) > 0 {
		t.Fatalf("%s: rules faulted: %+v", c.Name, d.Trace.Faults)
	}
	if c.Want.Drop {
		if !d.Drop {
			t.Fatalf("%s: routed to %s, want drop", c.Name, d.Queue)
		}
		return
	}
	if d.Drop || d.Queue != c.Want.Queue {
		t.Fatalf("%s: decision %+v, want queue %s", c.Name, d, c.Want.Queue)
	}
	// Exactly one executing endpoint — the lane's own — and a work order at most once.
	if !slices.Equal(d.Endpoints, []string{byQueue[c.Want.Queue]}) || !d.Once || !d.WorkOrder {
		t.Fatalf("%s: endpoints %v once %v work_order %v, want only %s as an exclusive once work order",
			c.Name, d.Endpoints, d.Once, d.WorkOrder, byQueue[c.Want.Queue])
	}
}

func TestFleetPackValidatesAgainstTheLanesGrant(t *testing.T) {
	cfg := loadPack(t)
	g, _ := laneGrant()
	if err := Validate(cfg, g); err != nil {
		t.Fatalf("the checked-in fleet pack does not validate: %v", err)
	}
	if len(cfg.Rules) > MaxRules {
		t.Fatalf("pack has %d rules, over the %d limit", len(cfg.Rules), MaxRules)
	}
	// Without a pool endpoint scoped to a lane, the exclusive rules must refuse to save rather than
	// fan a work order out to whoever happens to be routed.
	missing := g
	missing.EndpointQueues = map[string][]string{}
	if err := Validate(cfg, missing); err == nil {
		t.Fatalf("pack validated with no endpoint scoped to any lane queue")
	}
}

func TestFleetPackRoutesFixtures(t *testing.T) {
	cfg := loadPack(t)
	g, byQueue := laneGrant()
	cases := loadCases(t)
	if len(cases) < 20 {
		t.Fatalf("only %d fixture cases loaded", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			checkCase(t, c, InProcess{}.Route(context.Background(), cfg, g, caseInput(t, c)), byQueue)
		})
	}
}

// Every case again, through the production path: the out-of-process sandbox child. Decisions must be
// identical to in-process evaluation.
func TestFleetPackRoutesFixturesThroughTheSandbox(t *testing.T) {
	cfg := loadPack(t)
	g, byQueue := laneGrant()
	sb := testSandbox(t)
	for _, c := range loadCases(t) {
		in := caseInput(t, c)
		got := sb.Route(context.Background(), cfg, g, in)
		checkCase(t, c, got, byQueue)
		if want := (InProcess{}).Route(context.Background(), cfg, g, in); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: sandbox %+v differs from in-process %+v", c.Name, got, want)
		}
	}
}

// Without params (an agent that set rules and forgot the allowlists) every trust rule fails closed:
// nothing reaches a lane.
func TestFleetPackFailsClosedWithoutParams(t *testing.T) {
	cfg := loadPack(t)
	cfg.Params = nil
	g, _ := laneGrant()
	for _, c := range loadCases(t) {
		d := InProcess{}.Route(context.Background(), cfg, g, caseInput(t, c))
		if !d.Drop && d.Queue != "hold" {
			t.Fatalf("%s: routed to %s with no allowlists", c.Name, d.Queue)
		}
	}
}

// When both identities have a pool endpoint scoped to the same lane, exactly one executes: the first
// in route order, every time.
func TestExclusiveDeliveryPicksOneIdentityDeterministically(t *testing.T) {
	cfg := loadPack(t)
	g, byQueue := laneGrant()
	second := "00000000-0000-0000-0000-0000000002ff"
	g.Endpoints = append(g.Endpoints, second)
	g.EndpointQueues[second] = []string{"lane-zai-flash"}
	c := packCase{Sample: "cairn-artifact-created"}
	for i := 0; i < 5; i++ {
		d := InProcess{}.Route(context.Background(), cfg, g, caseInput(t, c))
		if !slices.Equal(d.Endpoints, []string{byQueue["lane-zai-flash"]}) {
			t.Fatalf("run %d: endpoints %v, want only the first-routed %s", i, d.Endpoints, byQueue["lane-zai-flash"])
		}
	}
}

package mcp

// The endpoint default lease on MCP: every claim path applies per-call > endpoint default > server
// default; an edit applies to the next call on a live session; get_default_lease and
// set_default_lease read and write the calling endpoint's own default and nothing else; they are
// allowlisted like every verb, and never friend-grantable; and an edit re-registers the lease-bound
// tools on the endpoint's other live sessions.
//
// Governing: ADR-0043, SPEC-0006 REQ "Lease Lifecycle and Crash Safety", REQ "Endpoint Default
// Lease Verbs", SPEC-0007 REQ "Endpoint Default Lease".

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/lease"
	"github.com/stump-wtf/switchboard/internal/store"
)

func intp(n int) *int { return &n }

// lastClaimTTL is the lease the most recent claim committed with.
func (f *fakeStore) lastClaimTTL(t *testing.T) time.Duration {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.claims) == 0 {
		t.Fatal("no claim was committed")
	}
	return f.claims[len(f.claims)-1].TTL
}

// lastHeartbeatTTL is the lease the most recent heartbeat extended by.
func (f *fakeStore) lastHeartbeatTTL(t *testing.T) time.Duration {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.heartbeatTTLs) == 0 {
		t.Fatal("no heartbeat was committed")
	}
	return f.heartbeatTTLs[len(f.heartbeatTTLs)-1]
}

// TestLeasePrecedenceOnEveryClaimPath: claim, claim_next and heartbeat each resolve their lease as
// per-call lease_ttl_seconds (clamped) > the endpoint's default > the server default. The heartbeat
// rows are the trap the default removes: a bare heartbeat on a configured endpoint extends by the
// endpoint's default, not 300s.
func TestLeasePrecedenceOnEveryClaimPath(t *testing.T) {
	cases := []struct {
		name     string
		endpoint *int
		perCall  int
		want     time.Duration
	}{
		{"server default", nil, 0, lease.DefaultTTL},
		{"endpoint default", intp(3600), 0, time.Hour},
		{"per-call beats endpoint default", intp(3600), 120, 2 * time.Minute},
		{"per-call beats server default", nil, 900, 15 * time.Minute},
		{"per-call is clamped to the cap", intp(3600), lease.MaxSeconds + 5, lease.MaxTTL},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			f := newFakeStore()
			f.defaultLeases[defaultTestEndpointID] = c.endpoint
			f.putTodo(pendingTodo("td_claim"))
			f.putTodo(pendingTodo("td_next"))
			cs := session(t, ctx, f, []string{"reviews"}, []string{"claim", "claim_next", "heartbeat"})
			args := func(extra map[string]any) map[string]any {
				if c.perCall != 0 {
					extra["lease_ttl_seconds"] = c.perCall
				}
				return extra
			}

			var claimed claimOut
			callOK(t, ctx, cs, "claim", args(map[string]any{"id": "td_claim"}), &claimed)
			if got := f.lastClaimTTL(t); got != c.want {
				t.Errorf("claim lease = %v, want %v", got, c.want)
			}
			var next claimNextOut
			callOK(t, ctx, cs, "claim_next", args(map[string]any{}), &next)
			if next.Todo == nil || next.Todo.ID != "td_next" {
				t.Fatalf("claim_next = %+v, want td_next", next)
			}
			if got := f.lastClaimTTL(t); got != c.want {
				t.Errorf("claim_next lease = %v, want %v", got, c.want)
			}
			var hb todoOut
			callOK(t, ctx, cs, "heartbeat", args(map[string]any{"id": "td_claim"}), &hb)
			if got := f.lastHeartbeatTTL(t); got != c.want {
				t.Errorf("heartbeat lease = %v, want %v", got, c.want)
			}
		})
	}
}

// TestDefaultLeaseEditAppliesToTheNextCallOnALiveSession: the verbs read the live default, so a
// human's edit lands on the next claim and heartbeat without a reconnect.
func TestDefaultLeaseEditAppliesToTheNextCallOnALiveSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	f.putTodo(pendingTodo("td_1"))
	cs := session(t, ctx, f, []string{"reviews"}, []string{"claim", "heartbeat"})

	var claimed claimOut
	callOK(t, ctx, cs, "claim", map[string]any{"id": "td_1"}, &claimed)
	if got := f.lastClaimTTL(t); got != lease.DefaultTTL {
		t.Fatalf("claim before the edit = %v, want the server default", got)
	}
	f.mu.Lock()
	f.defaultLeases[defaultTestEndpointID] = intp(1800)
	f.mu.Unlock()
	var hb todoOut
	callOK(t, ctx, cs, "heartbeat", map[string]any{"id": "td_1"}, &hb)
	if got := f.lastHeartbeatTTL(t); got != 30*time.Minute {
		t.Fatalf("heartbeat after the edit = %v, want the new 30m default", got)
	}
}

// TestLeaseReadFailureFailsTheClaimClosed: when the default cannot be read, the claim is refused as
// internal rather than granted a lease the endpoint's human did not choose.
func TestLeaseReadFailureFailsTheClaimClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	f.putTodo(pendingTodo("td_1"))
	f.defaultLeaseErr = errors.New("db down")
	cs := session(t, ctx, f, []string{"reviews"}, []string{"claim"})
	callErr(t, ctx, cs, "claim", map[string]any{"id": "td_1"}, codeInternal)
	if got := f.todoState("td_1"); got != "pending" {
		t.Fatalf("a refused claim changed the todo to %q", got)
	}
	// An explicit lease needs no read, so it still works.
	var claimed claimOut
	callOK(t, ctx, cs, "claim", map[string]any{"id": "td_1", "lease_ttl_seconds": 600}, &claimed)
}

// TestDefaultLeaseVerbsReadAndWriteOnlyTheCaller: get_default_lease reads the caller's own default;
// set_default_lease writes it across the bounds (60 and 86400 stored, 59 and 86401 refused with
// invalid_argument and nothing changed, null resets) and never another endpoint's, even when the
// call names one.
func TestDefaultLeaseVerbsReadAndWriteOnlyTheCaller(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	other := endpointIDFor("agent-b-22222222")
	f.defaultLeases[other] = intp(600)
	vend(t, f, "agent-b-22222222", []string{"reviews"}, []string{"claim"})
	cs := session(t, ctx, f, []string{"reviews"}, []string{"get_default_lease", "set_default_lease"})

	var got defaultLeaseOut
	callOK(t, ctx, cs, "get_default_lease", map[string]any{}, &got)
	if got.DefaultLeaseTTLSeconds != nil || got.EffectiveLeaseTTLSeconds != lease.DefaultSeconds ||
		got.ServerDefaultSeconds != lease.DefaultSeconds || got.MinDefaultSeconds != lease.MinDefaultSeconds ||
		got.MaxSeconds != lease.MaxSeconds {
		t.Fatalf("get_default_lease on an unconfigured endpoint = %+v", got)
	}

	for _, ok := range []int{lease.MinDefaultSeconds, lease.MaxSeconds, 3600} {
		var set defaultLeaseOut
		callOK(t, ctx, cs, "set_default_lease", map[string]any{"default_lease_ttl_seconds": ok}, &set)
		if set.DefaultLeaseTTLSeconds == nil || *set.DefaultLeaseTTLSeconds != ok || set.EffectiveLeaseTTLSeconds != ok {
			t.Fatalf("set %d answered %+v", ok, set)
		}
	}
	for _, bad := range []int{lease.MinDefaultSeconds - 1, lease.MaxSeconds + 1} {
		callErr(t, ctx, cs, "set_default_lease", map[string]any{"default_lease_ttl_seconds": bad}, codeInvalidArgument)
	}
	callOK(t, ctx, cs, "get_default_lease", map[string]any{}, &got)
	if got.DefaultLeaseTTLSeconds == nil || *got.DefaultLeaseTTLSeconds != 3600 {
		t.Fatalf("after refused writes the default is %+v, want 3600", got.DefaultLeaseTTLSeconds)
	}

	// An absent key is not a reset: the schema requires it.
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "set_default_lease", Arguments: map[string]any{}})
	if err == nil && !res.IsError {
		t.Fatal("set_default_lease with no default_lease_ttl_seconds succeeded, want a refusal")
	}
	// Naming another endpoint reaches nothing: the input has no such field, and the schema refuses
	// the call outright rather than ignoring it.
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "set_default_lease",
		Arguments: map[string]any{"default_lease_ttl_seconds": 7200, "endpoint_id": other}})
	if err == nil && !res.IsError {
		t.Fatal("set_default_lease accepted an endpoint_id argument, want a refusal")
	}
	if d := f.defaultLeases[other]; d == nil || *d != 600 {
		t.Fatalf("another endpoint's default changed to %v", d)
	}

	var reset defaultLeaseOut
	callOK(t, ctx, cs, "set_default_lease", map[string]any{"default_lease_ttl_seconds": nil}, &reset)
	if reset.DefaultLeaseTTLSeconds != nil || reset.EffectiveLeaseTTLSeconds != lease.DefaultSeconds {
		t.Fatalf("null reset answered %+v", reset)
	}
	if d := f.defaultLeases[defaultTestEndpointID]; d != nil {
		t.Fatalf("after reset the stored default is %d, want nil", *d)
	}
	if d := f.defaultLeases[other]; d == nil || *d != 600 {
		t.Fatalf("another endpoint's default changed to %v", d)
	}
}

// TestDefaultLeaseVerbsAreAllowlisted: without the grant the verbs are neither advertised nor
// callable (forbidden, nothing changed), like every other verb.
func TestDefaultLeaseVerbsAreAllowlisted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	cs := session(t, ctx, f, []string{"reviews"}, []string{"claim", "get_default_lease"})
	tools := toolNames(t, ctx, cs)
	if tools["get_default_lease"] == nil {
		t.Fatal("granted get_default_lease is not advertised")
	}
	if tools["set_default_lease"] != nil {
		t.Fatal("set_default_lease is advertised without its grant")
	}
	callErr(t, ctx, cs, "set_default_lease", map[string]any{"default_lease_ttl_seconds": 3600}, codeForbidden)
	if d := f.defaultLeases[defaultTestEndpointID]; d != nil {
		t.Fatalf("a forbidden set_default_lease stored %d", *d)
	}
}

// TestLeaseVerbsAreDefaultGrantedButNeverFriendGrantable: the lease verbs sit in AllVerbs, the basics
// grant, beside webhook self-management; a friend edge can never carry them, because a friend
// endpoint's settings belong to the approver (ADR-0038 F3).
func TestLeaseVerbsAreDefaultGrantedButNeverFriendGrantable(t *testing.T) {
	for _, v := range LeaseVerbs() {
		if !slices.Contains(AllVerbs(), v) {
			t.Errorf("%s is not in AllVerbs, the default grant", v)
		}
	}
	if got := store.FriendGrantable(LeaseVerbs()); len(got) != 0 {
		t.Fatalf("store.FriendGrantable(LeaseVerbs()) = %v, want none", got)
	}
}

// TestDefaultLeaseEditRefreshesLiveSessions: an edit made on one session re-registers claim,
// claim_next and heartbeat on the endpoint's other live sessions, so their descriptions state the
// new default; a session of another endpoint is untouched. RefreshEndpointLease is the same hook the
// human API and the web UI ring.
func TestDefaultLeaseEditRefreshesLiveSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	verbs := []string{"claim", "claim_next", "heartbeat", "set_default_lease"}
	token := vend(t, f, defaultTestSlug, []string{"reviews"}, verbs)
	otherToken := vend(t, f, "agent-b-22222222", []string{"reviews"}, verbs)
	ts, h := newTestServerHandler(t, f)
	open := func(slug, tok string) *sdk.ClientSession {
		cs, err := connect(t, ctx, ts.URL+"/mcp/"+slug, tok)
		if err != nil {
			t.Fatalf("connect %s: %v", slug, err)
		}
		t.Cleanup(func() { _ = cs.Close() })
		return cs
	}
	setter, watcher, stranger := open(defaultTestSlug, token), open(defaultTestSlug, token), open("agent-b-22222222", otherToken)

	var out defaultLeaseOut
	callOK(t, ctx, setter, "set_default_lease", map[string]any{"default_lease_ttl_seconds": 2700}, &out)
	for name, cs := range map[string]*sdk.ClientSession{"setter": setter, "watcher": watcher} {
		tools := toolNames(t, ctx, cs)
		for _, tool := range []string{"claim", "claim_next", "heartbeat"} {
			if d := tools[tool].Description; !strings.Contains(d, "2700 seconds (this endpoint's default)") {
				t.Errorf("%s session's %s description after the edit: %q", name, tool, d)
			}
		}
	}
	if d := toolNames(t, ctx, stranger)["claim"].Description; !strings.Contains(d, strconv.Itoa(lease.DefaultSeconds)+" seconds (the server default)") {
		t.Errorf("another endpoint's session changed: %q", d)
	}

	// The human surfaces ring the same hook; a reset goes back to the server default's text.
	h.RefreshEndpointLease(defaultTestEndpointID, nil)
	if d := toolNames(t, ctx, watcher)["heartbeat"].Description; !strings.Contains(d, "default "+strconv.Itoa(lease.DefaultSeconds)+",") {
		t.Errorf("after a reset the heartbeat description reads %q", d)
	}
}

// TestNewSessionStatesTheConfiguredDefault: a session that starts after an edit gets instructions
// and descriptions built from the stored default (the auth resolve carries it).
func TestNewSessionStatesTheConfiguredDefault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	f.defaultLeases[defaultTestEndpointID] = intp(5400)
	cs := session(t, ctx, f, []string{"reviews"}, []string{"claim_next"})
	if got := cs.InitializeResult().Instructions; !strings.Contains(got, "lease for 5400 seconds (this endpoint's default)") {
		t.Fatalf("instructions do not state the configured default:\n%s", got)
	}
	if d := toolNames(t, ctx, cs)["claim_next"].Description; !strings.Contains(d, "lease for 5400 seconds") {
		t.Fatalf("claim_next description does not state the configured default: %q", d)
	}
}

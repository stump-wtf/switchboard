package mcp

// Tests for the replay_webhook_event delivery path (replay.go): owned-target resolution and the hard
// replay_target_required error, the shared SSRF guard at call time across every refused range, the
// pinned dial-time guard (DNS rebinding, mixed answers), the happy path against a live consumer,
// downstream non-2xx reported (not raised), replay-safe header projection, and the per-endpoint
// replay rate limit.
//
// The consumer in these tests is a loopback httptest server, which the guard (correctly) refuses.
// So the delivering tests resolve a public name through a scripted resolver and replace ONLY the
// final socket connect: resolution, the call-time Validate and the dial-time address check all run
// the production code, and each test asserts which approved address the connect received.
//
// Governing: ADR-0038, SPEC-0033 REQ "Owned Replay Targets" (F9); SPEC-0005 REQ "Replay Safety",
// "Redirect Validation" + "Rate Limiting" security requirements.

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/push"
	"github.com/stump-wtf/switchboard/internal/store"
)

// publicIP is a TEST-NET-3 address: not private, loopback, link-local or shared, so the guard
// treats it as a public target.
const publicIP = "203.0.113.10"

// seedEvent seeds one replayable event with the given payload and headers.
func seedEvent(f *fakeStore, id int64, payload string, headers []byte) {
	f.putEvent(store.EventHistoryDetail{
		EventHistoryItem: store.EventHistoryItem{ID: id, Provider: "github", EventType: "push",
			TrustMode: "signed", Verified: true, PayloadSize: len(payload)},
		ContentType: "application/json",
		Headers:     headers,
		Payload:     []byte(payload),
	})
}

// scriptedResolver answers each lookup of a host from its script, one entry per call, repeating the
// last entry once the script runs out. It counts lookups so a test can see the pinned dial resolve
// exactly once.
type scriptedResolver struct {
	mu      sync.Mutex
	script  map[string][][]string
	lookups map[string]int
}

func newScriptedResolver(script map[string][][]string) *scriptedResolver {
	return &scriptedResolver{script: script, lookups: map[string]int{}}
}

func (r *scriptedResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	answers := r.script[host]
	if len(answers) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	i := r.lookups[host]
	r.lookups[host]++
	if i >= len(answers) {
		i = len(answers) - 1
	}
	out := make([]net.IPAddr, 0, len(answers[i]))
	for _, s := range answers[i] {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out, nil
}

// replayRig is one endpoint's MCP session over a Handler whose replay guard resolves through a
// scripted resolver and whose final connect is recorded, then sent to consumer (or refused when
// consumer is empty).
type replayRig struct {
	h        *Handler
	cs       *sdk.ClientSession
	mu       sync.Mutex
	connects []string
}

func (r *replayRig) connected() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.connects...)
}

// newReplayRig vends slug against f, installs a test guard (http allowed so a plain httptest
// consumer works; every address rule is the default) over res, and connects a client.
func newReplayRig(t *testing.T, ctx context.Context, f *fakeStore, slug string, res push.Resolver, consumer string) *replayRig {
	t.Helper()
	token := vend(t, f, slug, []string{"reviews"}, eventVerbNames)
	h := New(f, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(h.Close)
	rig := &replayRig{h: h}
	g := newReplayGuard(res, push.WithAllowHTTP(true))
	g.connect = func(ctx context.Context, network, address string) (net.Conn, error) {
		rig.mu.Lock()
		rig.connects = append(rig.connects, address)
		rig.mu.Unlock()
		if consumer == "" {
			return nil, &net.OpError{Op: "dial", Net: network, Err: io.ErrUnexpectedEOF}
		}
		var d net.Dialer
		return d.DialContext(ctx, network, consumer)
	}
	h.replayGuard = g
	srv := httptest.NewServer(routesFor(h))
	t.Cleanup(srv.Close)
	cs, err := connect(t, ctx, srv.URL+"/mcp/"+slug, token)
	if err != nil {
		t.Fatalf("initialize handshake: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	rig.cs = cs
	return rig
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

type replayOut struct {
	ID             int64  `json:"id"`
	TargetURL      string `json:"target_url"`
	Delivered      bool   `json:"delivered"`
	ResponseStatus *int   `json:"response_status"`
	ResponseMs     int64  `json:"response_ms"`
}

// No target_url and no owned target: replay_target_required, and nothing is dialed.
// Governing: SPEC-0033 scenario "Instance trusted target is gone (F9)".
func TestReplayNoTargetIsReplayTargetRequired(t *testing.T) {
	ctx := testCtx(t)
	f := newFakeStore()
	seedEvent(f, 1, `{"zen":"go"}`, nil)
	rig := newReplayRig(t, ctx, f, defaultTestSlug, newScriptedResolver(nil), "")

	callErr(t, ctx, rig.cs, "replay_webhook_event", map[string]any{"id": 1}, codeReplayTargetRequired)
	if c := rig.connected(); len(c) != 0 {
		t.Fatalf("a replay with no target dialed %v", c)
	}
}

// Scenario "Instance trusted target is gone (F9)", end to end over the production guard: an internal
// host that the old replay_allowed_targets would have trusted is reached neither by default nor by
// naming it, and it never sees a request.
func TestReplayInstanceTrustedTargetIsGone(t *testing.T) {
	ctx := testCtx(t)
	var hits atomic.Int64
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(internal.Close)

	f := newFakeStore()
	seedEvent(f, 7, `{}`, nil)
	cs := session(t, ctx, f, []string{"reviews"}, eventVerbNames) // the production guard

	callErr(t, ctx, cs, "replay_webhook_event", map[string]any{"id": 7}, codeReplayTargetRequired)
	callErr(t, ctx, cs, "replay_webhook_event", map[string]any{"id": 7, "target_url": internal.URL + "/x"}, codeInvalidArgument)
	if n := hits.Load(); n != 0 {
		t.Fatalf("the internal host received %d request(s)", n)
	}
}

// Every refused range and scheme is invalid_argument at call time, before any dial. Owned targets
// get no exemption: the same table is replayed as the endpoint's owned default.
// Governing: SPEC-0033 REQ "Owned Replay Targets", SPEC-0005 scenario "Non-http scheme is rejected
// before any request".
func TestReplaySSRFRejectionTable(t *testing.T) {
	ctx := testCtx(t)
	refused := []struct{ name, target string }{
		{"loopback v4", "https://127.0.0.1:8080/hook"},
		{"loopback v4 range", "https://127.9.9.9/hook"},
		{"private 10/8", "https://10.0.0.5/hook"},
		{"private 172.16/12", "https://172.16.4.4/hook"},
		{"private 192.168/16", "https://192.168.1.10/hook"},
		{"shared cgnat", "https://100.64.1.1/hook"},
		{"link-local", "https://169.254.10.10/hook"},
		{"cloud metadata", "https://169.254.169.254/latest/meta-data/"},
		{"unspecified", "https://0.0.0.0/hook"},
		{"ipv6 loopback", "https://[::1]:9000/hook"},
		{"ipv6 mapped loopback", "https://[::ffff:127.0.0.1]/hook"},
		{"ipv6 ula", "https://[fc00::1]/hook"},
		{"ipv6 link-local", "https://[fe80::1]/hook"},
		{"name resolving private", "https://internal.example/hook"},
		{"name resolving mixed", "https://mixed.example/hook"},
		{"unresolvable name", "https://nowhere.example/hook"},
		{"non-http scheme file", "file:///etc/passwd"},
		{"non-http scheme gopher", "gopher://" + publicIP + "/1"},
		{"relative", "/just/a/path"},
	}
	res := newScriptedResolver(map[string][][]string{
		"internal.example": {{"10.1.2.3"}},
		"mixed.example":    {{publicIP, "192.168.0.9"}},
	})
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeStore()
			seedEvent(f, 1, `{}`, nil)
			rig := newReplayRig(t, ctx, f, defaultTestSlug, res, "")
			callErr(t, ctx, rig.cs, "replay_webhook_event",
				map[string]any{"id": 1, "target_url": tc.target}, codeInvalidArgument)
			f.replayTargets[endpointIDFor(defaultTestSlug)] = []string{tc.target}
			callErr(t, ctx, rig.cs, "replay_webhook_event", map[string]any{"id": 1}, codeInvalidArgument)
			if c := rig.connected(); len(c) != 0 {
				t.Fatalf("a refused target dialed %v", c)
			}
		})
	}
}

// A refused target tells the caller the refusal's class and nothing about the server's network: a
// name that resolves to an internal address never echoes that address, and a failed lookup never
// echoes the resolver's error. Owned targets take the same path.
// Governing: SPEC-0033 REQ "Owned Replay Targets" (audit F9).
func TestReplayRefusalNamesNoResolvedAddress(t *testing.T) {
	ctx := testCtx(t)
	res := newScriptedResolver(map[string][][]string{
		"db.corp.internal": {{"10.20.30.40"}},
		"v6.corp.internal": {{"fd12:3456::7"}},
	})
	cases := []struct{ target, want string }{
		{"https://db.corp.internal/", "target_url resolves to a disallowed address"},
		{"https://v6.corp.internal/", "target_url resolves to a disallowed address"},
		{"https://nowhere.corp.internal/", "target_url host could not be resolved"},
	}
	for _, tc := range cases {
		f := newFakeStore()
		seedEvent(f, 1, `{}`, nil)
		rig := newReplayRig(t, ctx, f, defaultTestSlug, res, "")
		for _, args := range []map[string]any{{"id": 1, "target_url": tc.target}, {"id": 1}} {
			f.replayTargets[endpointIDFor(defaultTestSlug)] = []string{tc.target}
			msg := callErr(t, ctx, rig.cs, "replay_webhook_event", args, codeInvalidArgument)
			if msg != codeInvalidArgument+": "+tc.want {
				t.Fatalf("%s: message = %q, want %q", tc.target, msg, codeInvalidArgument+": "+tc.want)
			}
			for _, leak := range []string{"10.20.30.40", "fd12", "no such host", "SSRF"} {
				if strings.Contains(msg, leak) {
					t.Fatalf("%s: message %q leaks %q", tc.target, msg, leak)
				}
			}
		}
	}
}

// The production guard is https-only: a public plain-http target is refused.
func TestReplayProductionGuardRequiresHTTPS(t *testing.T) {
	ctx := testCtx(t)
	f := newFakeStore()
	seedEvent(f, 1, `{}`, nil)
	cs := session(t, ctx, f, []string{"reviews"}, eventVerbNames)
	msg := callErr(t, ctx, cs, "replay_webhook_event",
		map[string]any{"id": 1, "target_url": "http://" + publicIP + "/hook"}, codeInvalidArgument)
	if want := codeInvalidArgument + ": target_url must use https"; msg != want {
		t.Fatalf("message = %q, want %q", msg, want)
	}
}

// The endpoint's first owned target is the default: the stored payload is delivered, the response
// reports the target, and the one connect went to the address the guard approved.
// Governing: SPEC-0033 REQ "Owned Replay Targets", SPEC-0005 REQ "Replay Safety".
func TestReplayOwnedDefaultTargetDelivers(t *testing.T) {
	ctx := testCtx(t)
	var gotBody, gotCT atomic.Value
	consumer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody.Store(string(b))
		gotCT.Store(r.Header.Get("Content-Type"))
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(consumer.Close)

	f := newFakeStore()
	f.replayTargets[endpointIDFor(defaultTestSlug)] = []string{"http://replay.example/consume", "http://second.example/"}
	seedEvent(f, 42, `{"action":"opened"}`, []byte(`{"Content-Type":"application/json","X-GitHub-Event":"pull_request"}`))
	res := newScriptedResolver(map[string][][]string{"replay.example": {{publicIP}}})
	rig := newReplayRig(t, ctx, f, defaultTestSlug, res, consumer.Listener.Addr().String())

	var out replayOut
	callOK(t, ctx, rig.cs, "replay_webhook_event", map[string]any{"id": 42}, &out)
	if out.ID != 42 || out.TargetURL != "http://replay.example/consume" || !out.Delivered {
		t.Fatalf("replay = %+v, want event 42 delivered to the first owned target", out)
	}
	if out.ResponseStatus == nil || *out.ResponseStatus != http.StatusAccepted {
		t.Fatalf("response_status = %v, want 202", out.ResponseStatus)
	}
	if b, _ := gotBody.Load().(string); b != `{"action":"opened"}` {
		t.Fatalf("downstream body = %q, want the stored raw payload", b)
	}
	if ct, _ := gotCT.Load().(string); ct != "application/json" {
		t.Fatalf("downstream Content-Type = %q, want the replay-safe stored header", ct)
	}
	if c := rig.connected(); len(c) != 1 || c[0] != net.JoinHostPort(publicIP, "80") {
		t.Fatalf("connects = %v, want exactly the approved %s:80", c, publicIP)
	}
}

// An explicit target_url wins over the owned default and is delivered once the guard approves it.
func TestReplayExplicitTargetDelivers(t *testing.T) {
	ctx := testCtx(t)
	consumer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(consumer.Close)

	f := newFakeStore()
	f.replayTargets[endpointIDFor(defaultTestSlug)] = []string{"http://owned.example/"}
	seedEvent(f, 7, `{}`, nil)
	rig := newReplayRig(t, ctx, f, defaultTestSlug, newScriptedResolver(nil), consumer.Listener.Addr().String())

	var out replayOut
	callOK(t, ctx, rig.cs, "replay_webhook_event",
		map[string]any{"id": 7, "target_url": "http://" + publicIP + ":8081/x"}, &out)
	if !out.Delivered || out.ResponseStatus == nil || *out.ResponseStatus != 200 || out.TargetURL != "http://"+publicIP+":8081/x" {
		t.Fatalf("explicit replay = %+v, want delivered 200 to the named target", out)
	}
	if c := rig.connected(); len(c) != 1 || c[0] != publicIP+":8081" {
		t.Fatalf("connects = %v, want exactly %s:8081", c, publicIP)
	}
}

// DNS rebinding: the name is public when validated at call time and private when the dialer resolves
// it. The dial is refused before any connect, and the call is replay_failed.
func TestReplayDialTimeRebindIsRefused(t *testing.T) {
	ctx := testCtx(t)
	f := newFakeStore()
	seedEvent(f, 3, `{}`, nil)
	for name, second := range map[string][]string{
		"rebind to private":   {"10.0.0.7"},
		"rebind to loopback":  {"127.0.0.1"},
		"rebind to mixed":     {publicIP, "169.254.169.254"},
		"rebind to ipv6 ula ": {"fd00::7"},
	} {
		t.Run(name, func(t *testing.T) {
			res := newScriptedResolver(map[string][][]string{"rebind.example": {{publicIP}, second}})
			rig := newReplayRig(t, ctx, f, defaultTestSlug, res, "127.0.0.1:1")
			callErr(t, ctx, rig.cs, "replay_webhook_event",
				map[string]any{"id": 3, "target_url": "http://rebind.example/"}, codeReplayFailed)
			if c := rig.connected(); len(c) != 0 {
				t.Fatalf("a rebound name was dialed: %v", c)
			}
			if n := res.lookups["rebind.example"]; n != 2 {
				t.Fatalf("lookups = %d, want 2 (call-time validate, then one pinned dial resolution)", n)
			}
		})
	}
}

// No path dials without the guard: the production connect step checks the address itself as the
// socket connects, so even a caller that skipped dialContext cannot reach a refused address, and the
// production client refuses a loopback URL outright.
func TestReplayProductionDialRefusesRefusedAddresses(t *testing.T) {
	ctx := testCtx(t)
	var hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)

	g := newReplayGuard(nil)
	if conn, err := g.connect(ctx, "tcp", ts.Listener.Addr().String()); err == nil {
		_ = conn.Close()
		t.Fatal("the production connect step reached a loopback address")
	}
	if conn, err := g.dialContext(ctx, "tcp", ts.Listener.Addr().String()); err == nil {
		_ = conn.Close()
		t.Fatal("the guarded dialer reached a loopback address")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL, http.NoBody)
	if resp, err := g.client().Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("the replay client reached a loopback address")
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the loopback server received %d request(s)", n)
	}
}

// A downstream non-2xx is reported as delivered:true with the status, not raised as replay_failed
// (which is reserved for transport failures). Governing: SPEC-0005 scenario "Downstream non-2xx is
// reported, not raised".
func TestReplayDownstreamNon2xxReported(t *testing.T) {
	ctx := testCtx(t)
	consumer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(consumer.Close)

	f := newFakeStore()
	f.replayTargets[endpointIDFor(defaultTestSlug)] = []string{"http://" + publicIP + "/"}
	seedEvent(f, 5, `{}`, nil)
	rig := newReplayRig(t, ctx, f, defaultTestSlug, newScriptedResolver(nil), consumer.Listener.Addr().String())

	var out replayOut
	callOK(t, ctx, rig.cs, "replay_webhook_event", map[string]any{"id": 5}, &out)
	if !out.Delivered || out.ResponseStatus == nil || *out.ResponseStatus != http.StatusInternalServerError {
		t.Fatalf("replay = %+v, want delivered with status 500", out)
	}
}

// A failed POST's error — which replay logs at WARN — carries the underlying cause, never the target
// URL: a token in the query string must not reach the log. Governing: SPEC-0005 REQ "Replay Safety".
func TestReplayFailureErrorOmitsTargetURL(t *testing.T) {
	ctx := testCtx(t)
	g := newReplayGuard(newScriptedResolver(map[string][][]string{"replay.example": {{publicIP}}}))
	g.connect = func(_ context.Context, network, _ string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: network, Err: io.ErrUnexpectedEOF}
	}
	target, err := url.Parse("https://replay.example/hook?token=s3cr3t-in-query")
	if err != nil {
		t.Fatal(err)
	}
	delivered, _, _, err := doReplay(ctx, g.client(), target, []byte(`{}`), http.Header{})
	if delivered || err == nil {
		t.Fatalf("doReplay = delivered %v, err %v; want a transport failure", delivered, err)
	}
	for _, leak := range []string{"s3cr3t-in-query", "token=", "/hook"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("replay error %q leaks %q", err, leak)
		}
	}
	if !strings.Contains(err.Error(), io.ErrUnexpectedEOF.Error()) {
		t.Fatalf("replay error %q lost the underlying cause", err)
	}
}

// A target that accepts nothing is a transport failure — replay_failed, with no leaked detail.
// Governing: SPEC-0005 REQ "Stable Error Shape".
func TestReplayTransportFailureRaisesReplayFailed(t *testing.T) {
	ctx := testCtx(t)
	f := newFakeStore()
	seedEvent(f, 9, `{}`, nil)
	rig := newReplayRig(t, ctx, f, defaultTestSlug, newScriptedResolver(nil), "")

	msg := callErr(t, ctx, rig.cs, "replay_webhook_event",
		map[string]any{"id": 9, "target_url": "http://" + publicIP + "/x"}, codeReplayFailed)
	if msg != codeReplayFailed+": replay could not reach the target" {
		t.Fatalf("replay_failed message = %q, want the fixed text", msg)
	}
}

// An unknown event id resolves to not_found before any outbound work.
// Governing: SPEC-0005 scenario "Unknown id raises not_found".
func TestReplayUnknownIdIsNotFound(t *testing.T) {
	ctx := testCtx(t)
	f := newFakeStore()
	f.replayTargets[endpointIDFor(defaultTestSlug)] = []string{"http://" + publicIP + "/x"}
	rig := newReplayRig(t, ctx, f, defaultTestSlug, newScriptedResolver(nil), "")

	callErr(t, ctx, rig.cs, "replay_webhook_event", map[string]any{"id": 999}, "not_found")
	if c := rig.connected(); len(c) != 0 {
		t.Fatalf("an unknown id dialed %v", c)
	}
}

// Owned targets belong to their endpoint: a second endpoint's replay with no target never resolves
// the first endpoint's list, and dials nothing.
func TestReplayOwnedTargetsAreNotShared(t *testing.T) {
	ctx := testCtx(t)
	f := newFakeStore()
	f.replayTargets[endpointIDFor("agent-a-11111111")] = []string{"http://" + publicIP + "/a-only"}
	seedEvent(f, 4, `{}`, nil)
	rigB := newReplayRig(t, ctx, f, "agent-b-22222222", newScriptedResolver(nil), "")

	callErr(t, ctx, rigB.cs, "replay_webhook_event", map[string]any{"id": 4}, codeReplayTargetRequired)
	if c := rigB.connected(); len(c) != 0 {
		t.Fatalf("B's default replay dialed %v", c)
	}
}

// Once the per-endpoint replay budget is exhausted, further replays return rate_limited.
// Governing: SPEC-0005 "Rate Limiting" security requirement.
func TestReplayRateLimited(t *testing.T) {
	ctx := testCtx(t)
	consumer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(consumer.Close)

	f := newFakeStore()
	f.replayTargets[endpointIDFor(defaultTestSlug)] = []string{"http://" + publicIP + "/"}
	seedEvent(f, 1, `{}`, nil)
	rig := newReplayRig(t, ctx, f, defaultTestSlug, newScriptedResolver(nil), consumer.Listener.Addr().String())
	// A tiny bucket: 2 immediate replays, then throttled (refill rate negligible for the test).
	rig.h.replayRL = newRateLimiter(0.0001, 2)

	var out replayOut
	callOK(t, ctx, rig.cs, "replay_webhook_event", map[string]any{"id": 1}, &out)
	callOK(t, ctx, rig.cs, "replay_webhook_event", map[string]any{"id": 1}, &out)
	callErr(t, ctx, rig.cs, "replay_webhook_event", map[string]any{"id": 1}, "rate_limited")
}

// The replay-safe header projection drops hop-by-hop and credential headers and any header still
// carrying the ingest redaction sentinel, while keeping benign metadata headers.
func TestReplaySafeHeadersDropsSecretsAndHopByHop(t *testing.T) {
	in := map[string]string{
		"Content-Type":        "application/json",
		"X-GitHub-Event":      "push",
		"X-Hub-Signature-256": redactionMarker, // redacted secret → dropped
		"Authorization":       "Bearer sekret", // credential → dropped
		"Cookie":              "sid=abc",       // credential → dropped
		"Connection":          "keep-alive",    // hop-by-hop → dropped
		"Host":                "evil.example",  // set by client → dropped
	}
	out := replaySafeHeaders(in)
	if out.Get("Content-Type") != "application/json" || out.Get("X-GitHub-Event") != "push" {
		t.Fatalf("benign headers dropped: %v", out)
	}
	for _, drop := range []string{"X-Hub-Signature-256", "Authorization", "Cookie", "Connection", "Host"} {
		if out.Get(drop) != "" {
			t.Fatalf("header %q was forwarded, want dropped", drop)
		}
	}
}

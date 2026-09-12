package mcp

// Integration tests for channel doorbells over the Streamable HTTP notification stream
// (SPEC-0014 REQ "Channels Push over the HTTP Stream" + "Concurrency Safety"; SPEC-0011 push
// semantics). The stream side is driven with a raw HTTP client so the tests observe the exact
// wire frames a harness would: the SDK client has no receiver for custom notification methods.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/stump-wtf/switchboard/internal/store"
)

// TestChannelCapabilityAdvertised: initialize advertises the experimental claude/channel
// capability alongside tools, plus doorbell-over-durable-queue instructions.
// Governing: SPEC-0011 REQ "Channel Capability on the Vended Session".
func TestChannelCapabilityAdvertised(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := &fakeStore{byHash: map[string]store.AuthEndpoint{}}
	token := vend(t, f, "agent-a-11111111", []string{"reviews"}, []string{"list_todos"})
	ts := newTestServer(t, f)

	cs, err := connect(t, ctx, ts.URL+"/mcp/agent-a-11111111", token)
	if err != nil {
		t.Fatalf("initialize handshake: %v", err)
	}
	defer func() { _ = cs.Close() }()

	init := cs.InitializeResult()
	if init.Capabilities == nil || init.Capabilities.Experimental == nil {
		t.Fatal("no experimental capabilities advertised")
	}
	if _, ok := init.Capabilities.Experimental["claude/channel"]; !ok {
		t.Fatalf(`capabilities.experimental["claude/channel"] not advertised: %v`, init.Capabilities.Experimental)
	}
	if init.Capabilities.Tools == nil {
		t.Fatal("tools capability must be advertised alongside the channel capability")
	}
	if init.ServerInfo.Name != serverName {
		t.Fatalf("server name = %q, want %q", init.ServerInfo.Name, serverName)
	}
	for _, want := range []string{"doorbell", "durable", "list_todos"} {
		if !strings.Contains(init.Instructions, want) {
			t.Fatalf("instructions missing %q: %q", want, init.Instructions)
		}
	}
}

// --- raw Streamable HTTP client helpers ---

// rawSession drives the wire protocol directly: initialize POST (capturing Mcp-Session-Id),
// notifications/initialized POST, and an optional hanging GET notification stream.
type rawSession struct {
	t     *testing.T
	url   string
	token string
	id    string
}

func rawInitialize(t *testing.T, url, token string) *rawSession {
	t.Helper()
	resp := rawPost(t, url, token,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d, want 200", resp.StatusCode)
	}
	id := resp.Header.Get(sessionIDHeader)
	if id == "" {
		t.Fatal("initialize response missing Mcp-Session-Id")
	}
	s := &rawSession{t: t, url: url, token: token, id: id}
	s.post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	return s
}

func (s *rawSession) post(body string) *http.Response {
	s.t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.url, strings.NewReader(body))
	if err != nil {
		s.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set(sessionIDHeader, s.id)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("post: %v", err)
	}
	s.t.Cleanup(func() { _ = resp.Body.Close() })
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp
}

// notification is one decoded doorbell frame from the SSE stream.
type notification struct {
	Method string `json:"method"`
	Params struct {
		Content string            `json:"content"`
		Meta    map[string]string `json:"meta"`
	} `json:"params"`
}

// openStream opens the hanging GET notification stream and returns a channel of decoded
// server→client notifications plus a channel closed when the stream ends (EOF / server close).
func (s *rawSession) openStream() (<-chan notification, <-chan struct{}) {
	s.t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.url, nil)
	if err != nil {
		s.t.Fatalf("build stream request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set(sessionIDHeader, s.id)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("open stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		s.t.Fatalf("stream status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	s.t.Cleanup(func() { _ = resp.Body.Close() })

	events := make(chan notification, 32)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var data strings.Builder
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "data:"):
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			case line == "":
				if data.Len() > 0 {
					var n notification
					if json.Unmarshal([]byte(data.String()), &n) == nil {
						events <- n
					}
					data.Reset()
				}
			}
		}
	}()
	return events, closed
}

// publishUntilDelivered retries a publish until the subscriber sees a doorbell for the todo,
// absorbing the benign race between the GET stream arriving at the server and its stream writer
// being registered. Duplicate deliveries from retries are fine: SPEC-0011 makes duplicates
// harmless by contract (idempotent claim).
func publishUntilDelivered(t *testing.T, h *Handler, td store.Todo, events <-chan notification) notification {
	t.Helper()
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	h.PublishTodoReady(td)
	for {
		select {
		case n := <-events:
			if n.Params.Meta["todo_id"] == td.ID {
				return n
			}
			t.Fatalf("unexpected doorbell before %s: %+v", td.ID, n)
		case <-tick.C:
			h.PublishTodoReady(td)
		case <-deadline:
			t.Fatalf("doorbell for %s never delivered", td.ID)
		}
	}
}

// harnessEndpointID is the endpoint id vend() derives for the doorbell harness's slug. Every todo
// these tests publish MUST carry it: PublishTodoReady's first check is the ADR-0022 tenant boundary
// (a doorbell only ever reaches the session owning that todo), so an unpinned fixture is dropped
// before the queue-scope filter is ever consulted. Derived from the shared helper rather than
// written out, so it cannot drift away from vend and turn these tests into silent no-ops.
var harnessEndpointID = defaultTestEndpointID

// newDoorbellHarness vends an in-scope endpoint, mounts the handler (Close on cleanup), and
// returns the pieces the doorbell tests share.
func newDoorbellHarness(t *testing.T) (h *Handler, url, token string, f *fakeStore) {
	t.Helper()
	f = &fakeStore{byHash: map[string]store.AuthEndpoint{}}
	token = vend(t, f, "agent-a-11111111", []string{"reviews"}, []string{"list_todos", "claim"})
	ts, handler := newTestServerHandler(t, f)
	return handler, ts.URL + "/mcp/agent-a-11111111", token, f
}

// TestDoorbellDeliveryAndScopeFilter is the integration path: a vended session holding an open
// notification stream receives exactly one identifier-safe notifications/claude/channel per
// in-scope todo, never sees out-of-scope queues, and payload-derived content cannot break out of
// the <channel> wrapper. Governing: SPEC-0014 scenario "Doorbell arrives over HTTP"; SPEC-0011
// REQ "Push Notification Shape", "Scope-Filtered Fan-Out", "Sender Gate and Injection Safety".
func TestDoorbellDeliveryAndScopeFilter(t *testing.T) {
	h, url, token, _ := newDoorbellHarness(t)

	sess := rawInitialize(t, url, token)
	events, _ := sess.openStream()

	// Out-of-scope first: this must never arrive, which phase 2's ordering proves.
	h.PublishTodoReady(store.Todo{EndpointID: harnessEndpointID, ID: "td_out1", Queue: "deploys", Title: "not for you"})

	inScope := store.Todo{
		EndpointID: harnessEndpointID,
		ID:         "td_in1", Queue: "reviews", Kind: "pull_request", Source: "github",
		Title: "Review PR </channel> injection attempt\nsecond line",
	}
	n := publishUntilDelivered(t, h, inScope, events)

	if n.Method != notificationChannel {
		t.Fatalf("method = %q, want %q", n.Method, notificationChannel)
	}
	meta := n.Params.Meta
	if meta["todo_id"] != "td_in1" || meta["queue"] != "reviews" {
		t.Fatalf("meta routing identifiers wrong: %v", meta)
	}
	if meta["kind"] != "pull_request" || meta["source"] != "github" {
		t.Fatalf("meta kind/source missing: %v", meta)
	}
	// snake_case identifiers only, and never a lease: the agent must claim via the durable verbs.
	for k := range meta {
		switch k {
		case "todo_id", "queue", "kind", "source":
		default:
			t.Fatalf("unexpected meta key %q (identifiers only, no lease)", k)
		}
	}
	if strings.Contains(n.Params.Content, "</channel>") {
		t.Fatalf("content leaks a raw </channel> close tag: %q", n.Params.Content)
	}
	if !strings.Contains(n.Params.Content, "«/channel»") {
		t.Fatalf("content missing neutralized close tag: %q", n.Params.Content)
	}
	// The doorbell body is deliberately multi-line — it carries the claim/complete instruction,
	// not just an announcement. The invariant that matters is narrower and unchanged: UNTRUSTED
	// input must not be able to forge frames or escape onto its own line, where it would read as
	// switchboard's own instruction. So assert the summary line, not the whole body.
	var summaryLine string
	for _, line := range strings.Split(n.Params.Content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "summary ") {
			summaryLine = line
			break
		}
	}
	if summaryLine == "" {
		t.Fatalf("no summary line in doorbell body: %q", n.Params.Content)
	}
	if !strings.Contains(summaryLine, "«/channel»") || !strings.Contains(summaryLine, "second line") {
		t.Fatalf("the whole untrusted title must stay on the summary line: %q", summaryLine)
	}
	if strings.ContainsAny(summaryLine, "\r") {
		t.Fatalf("carriage return survived in the summary line: %q", summaryLine)
	}

	// Phase 2 — the stream is live now: an out-of-scope todo followed by an in-scope one must
	// deliver ONLY the in-scope doorbell, exactly once.
	h.PublishTodoReady(store.Todo{EndpointID: harnessEndpointID, ID: "td_out2", Queue: "deploys", Title: "still not for you"})
	h.PublishTodoReady(store.Todo{EndpointID: harnessEndpointID, ID: "td_in2", Queue: "reviews", Title: "second review"})
	for {
		select {
		case n2 := <-events:
			if n2.Params.Meta["todo_id"] == "td_in1" {
				continue // benign duplicate from the retry loop above
			}
			if n2.Params.Meta["todo_id"] != "td_in2" {
				t.Fatalf("out-of-scope doorbell delivered: %+v", n2)
			}
			// Exactly one for td_in2 and nothing else afterwards.
			select {
			case extra := <-events:
				if extra.Params.Meta["todo_id"] != "td_in1" {
					t.Fatalf("extra doorbell after td_in2: %+v", extra)
				}
			case <-time.After(300 * time.Millisecond):
			}
			return
		case <-time.After(10 * time.Second):
			t.Fatal("in-scope doorbell td_in2 never delivered")
		}
	}
}

// TestNoStreamNothingBufferedNothingLost: publishing with no open notification stream drops the
// doorbell (never blocks, never buffers into a later stream) — the todo itself stays in the
// durable queue for pull, which the store guarantees independently of this hub.
// Governing: SPEC-0014 scenario "No stream, no loss"; SPEC-0011 lossy delivery.
func TestNoStreamNothingBufferedNothingLost(t *testing.T) {
	h, url, token, _ := newDoorbellHarness(t)

	sess := rawInitialize(t, url, token)

	// No GET stream is open: the pump must drop this on the floor.
	h.PublishTodoReady(store.Todo{EndpointID: harnessEndpointID, ID: "td_lost", Queue: "reviews", Title: "no stream yet"})
	waitForDrainedDoorbells(t, h, sess.id)

	events, _ := sess.openStream()
	select {
	case n := <-events:
		t.Fatalf("dropped doorbell must not be replayed on a later stream: %+v", n)
	case <-time.After(500 * time.Millisecond):
	}
}

// waitForDrainedDoorbells waits until the session's pump has consumed its buffer, so a subsequent
// stream open cannot race a not-yet-attempted write.
func waitForDrainedDoorbells(t *testing.T, h *Handler, sid string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		s := h.sessions[sid]
		h.mu.Unlock()
		if s == nil {
			t.Fatal("session vanished while waiting for pump")
		}
		if len(s.doorbells) == 0 {
			// Buffer handed to the pump; give the in-flight write a moment to fail and drop.
			time.Sleep(100 * time.Millisecond)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("doorbell buffer never drained")
}

// TestRevocationClosesLiveStream: revoking an endpoint closes its open notification stream
// promptly, and once the store stops resolving the credential, subsequent requests get 401.
// Governing: SPEC-0014 REQ "Concurrency Safety" scenario "Revocation closes live streams".
func TestRevocationClosesLiveStream(t *testing.T) {
	h, url, token, f := newDoorbellHarness(t)

	sess := rawInitialize(t, url, token)
	_, closed := sess.openStream()

	// Revoke: the web layer flips the DB row (simulated by dropping the hash from the fake
	// store) and invokes the hook that tears down live sessions.
	f.revoke(token)
	h.CloseEndpointSessions("ep-agent-a-11111111")

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("open notification stream did not close promptly on revocation")
	}
	if resp := rawPost(t, url, token, `{"jsonrpc":"2.0","id":9,"method":"ping"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-revocation status = %d, want 401", resp.StatusCode)
	}
}

// TestSessionBoundToEndpoint: a session id minted on endpoint A is unusable with endpoint B's
// credential on B's path — the registry never leaks sessions across endpoints.
func TestSessionBoundToEndpoint(t *testing.T) {
	f := &fakeStore{byHash: map[string]store.AuthEndpoint{}}
	tokenA := vend(t, f, "agent-a-11111111", []string{"reviews"}, []string{"list_todos"})
	tokenB := vend(t, f, "agent-b-22222222", []string{"deploys"}, []string{"list_todos"})
	ts, _ := newTestServerHandler(t, f)

	sessA := rawInitialize(t, ts.URL+"/mcp/agent-a-11111111", tokenA)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp/agent-b-22222222",
		strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+tokenB)
	req.Header.Set(sessionIDHeader, sessA.id)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-endpoint session reuse status = %d, want 404", resp.StatusCode)
	}
}

// TestConcurrentPublishSubscribeClose exercises the session registry and fan-out under
// contention — sessions connecting, streaming, and closing while publishers hammer the hub —
// so `go test -race` guards the shared state (SPEC-0014 REQ "Concurrency Safety").
func TestConcurrentPublishSubscribeClose(t *testing.T) {
	f := &fakeStore{byHash: map[string]store.AuthEndpoint{}}
	token := vend(t, f, "agent-a-11111111", []string{"reviews"}, []string{"list_todos"})
	ts, h := newTestServerHandler(t, f)
	url := ts.URL + "/mcp/agent-a-11111111"

	stop := make(chan struct{})
	var pubWG sync.WaitGroup
	for i := 0; i < 4; i++ {
		pubWG.Add(1)
		go func(i int) {
			defer pubWG.Done()
			n := 0
			for {
				select {
				case <-stop:
					return
				default:
					n++
					h.PublishTodoReady(store.Todo{EndpointID: harnessEndpointID, ID: fmt.Sprintf("td_%d_%d", i, n), Queue: "reviews", Title: "load"})
				}
			}
		}(i)
	}

	var sessWG sync.WaitGroup
	for i := 0; i < 4; i++ {
		sessWG.Add(1)
		go func() {
			defer sessWG.Done()
			sess := rawInitialize(t, url, token)
			events, _ := sess.openStream()
			// Consume a few doorbells (or time out quietly), then tear the session down.
			for j := 0; j < 3; j++ {
				select {
				case <-events:
				case <-time.After(time.Second):
				}
			}
			req, _ := http.NewRequest(http.MethodDelete, url, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set(sessionIDHeader, sess.id)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}()
	}

	sessWG.Wait()
	close(stop)
	pubWG.Wait()
	h.Close() // idempotent with the cleanup Close; joins every pump/reaper goroutine
}

// TestDoorbellNeverCrossesEndpointsOnASharedQueue is the regression test for the cross-tenant leak
// ADR-0022 closes: two endpoints holding the SAME queue name — the common case, where both agents
// scope to "reviews" or "github" — must never see each other's work. Before endpoint ownership, a
// todo was matched by its free-form queue string alone, so agent A's webhook delivery rang agent
// B's doorbell, across humans.
//
// The delivery to A is not incidental: it is the control that proves the fixture is wired to a
// session that CAN receive, so B's silence is a real tenant filter rather than a todo that never
// matched anything. Governing: ADR-0022, SPEC-0003 REQ "Endpoint Ownership (Tenant Isolation)",
// SPEC-0011 REQ "Scope-Filtered Fan-Out".
func TestDoorbellNeverCrossesEndpointsOnASharedQueue(t *testing.T) {
	f := &fakeStore{byHash: map[string]store.AuthEndpoint{}}
	const slugA, slugB = "agent-a-11111111", "agent-b-22222222"
	tokenA := vend(t, f, slugA, []string{"reviews"}, []string{"list_todos", "claim"})
	tokenB := vend(t, f, slugB, []string{"reviews"}, []string{"list_todos", "claim"})
	ts, h := newTestServerHandler(t, f)

	sessA := rawInitialize(t, ts.URL+"/mcp/"+slugA, tokenA)
	eventsA, _ := sessA.openStream()
	sessB := rawInitialize(t, ts.URL+"/mcp/"+slugB, tokenB)
	eventsB, _ := sessB.openStream()

	// Owned by A, on the queue BOTH sessions hold. Queue scope alone cannot tell these apart.
	owned := store.Todo{
		EndpointID: endpointIDFor(slugA),
		ID:         "td_a_only", Queue: "reviews", Kind: "pull_request", Source: "github",
		Title: "A's private work",
	}
	publishUntilDelivered(t, h, owned, eventsA)

	// B shares the queue and has an open stream, so anything it receives here is a tenant breach.
	select {
	case n := <-eventsB:
		t.Fatalf("cross-tenant doorbell leak: endpoint %s received a todo owned by %s: %+v",
			endpointIDFor(slugB), owned.EndpointID, n)
	case <-time.After(time.Second):
	}
}

// TestSystemTodoIsNeverPushedToAgentSessions: a todo with no owning endpoint is operator/Board-only
// and must never ring an agent's doorbell. The ordering is the proof — the system todo is published
// first, so if the behavior were unprotected it would arrive before the owned todo that follows it.
//
// This asserts the contract, not one line: PublishTodoReady's empty-EndpointID early return is
// redundant defense-in-depth, because a session's endpointID is never empty and the tenant check
// below it already drops these. Deleting either guard alone still passes; deleting both fails here.
// The redundancy is worth keeping — it documents the intent and survives a future refactor that
// loosens the tenant comparison. Governing: ADR-0022.
func TestSystemTodoIsNeverPushedToAgentSessions(t *testing.T) {
	h, url, token, _ := newDoorbellHarness(t)

	sess := rawInitialize(t, url, token)
	events, _ := sess.openStream()

	// No EndpointID: a system row. On the session's own queue, so only the guard can stop it.
	h.PublishTodoReady(store.Todo{ID: "td_system", Queue: "reviews", Title: "operator-only"})

	owned := store.Todo{EndpointID: harnessEndpointID, ID: "td_owned", Queue: "reviews", Title: "real work"}
	n := publishUntilDelivered(t, h, owned, events)
	if got := n.Params.Meta["todo_id"]; got != "td_owned" {
		t.Fatalf("system todo was pushed to an agent session: first doorbell was %q, want td_owned", got)
	}
}

// --- unicast dispatch ---

// The doorbell rings ONE session per todo, not all of them.
//
// An endpoint is vended to exactly one agent (ADR-0008), so several sessions on it are the same
// logical agent running as competing consumers. Waking all of them was never a correctness problem
// — claim_next hands each caller a different row — but every wake is a model turn, so N instances
// burnt N invocations to do one todo's work. This asserts the dispatch decision, not the billing.
func TestDoorbellRingsExactlyOneSessionPerTodo(t *testing.T) {
	f := &fakeStore{byHash: map[string]store.AuthEndpoint{}}
	const slug = "worker-pool-33333333"
	token := vend(t, f, slug, []string{"reviews"}, []string{"claim_next"})
	ts, h := newTestServerHandler(t, f)

	// Three workers sharing one endpoint, each with an open notification stream.
	streams := make([]<-chan notification, 0, 3)
	for i := 0; i < 3; i++ {
		s := rawInitialize(t, ts.URL+"/mcp/"+slug, token)
		ev, _ := s.openStream()
		streams = append(streams, ev)
	}

	h.PublishTodoReady(store.Todo{
		EndpointID: endpointIDFor(slug), ID: "td_solo", Queue: "reviews",
		Kind: "pull_request", Source: "github", Title: "one worker's job",
	})

	rang := 0
	for _, ev := range streams {
		select {
		case n := <-ev:
			if got := n.Params.Meta["todo_id"]; got != "td_solo" {
				t.Errorf("unexpected doorbell payload %q", got)
			}
			rang++
		case <-time.After(750 * time.Millisecond):
		}
	}
	if rang != 1 {
		t.Fatalf("%d of 3 sessions were rung, want exactly 1 — waking the whole pool costs N model turns per todo", rang)
	}
}

// Successive todos rotate across workers rather than always landing on the same one, so a pool
// actually shares load instead of electing a permanent winner.
func TestDoorbellRotatesAcrossWorkers(t *testing.T) {
	f := &fakeStore{byHash: map[string]store.AuthEndpoint{}}
	const slug = "rotate-pool-44444444"
	token := vend(t, f, slug, []string{"reviews"}, []string{"claim_next"})
	ts, h := newTestServerHandler(t, f)

	const workers = 3
	streams := make([]<-chan notification, 0, workers)
	for i := 0; i < workers; i++ {
		s := rawInitialize(t, ts.URL+"/mcp/"+slug, token)
		ev, _ := s.openStream()
		streams = append(streams, ev)
	}

	// Publish one todo per worker; each should land somewhere different.
	for i := 0; i < workers; i++ {
		h.PublishTodoReady(store.Todo{
			EndpointID: endpointIDFor(slug), ID: fmt.Sprintf("td_%d", i), Queue: "reviews",
			Kind: "pull_request", Source: "github", Title: "work",
		})
	}

	rung := make([]int, workers)
	total := 0
	deadline := time.After(3 * time.Second)
	for total < workers {
		progressed := false
		for i, ev := range streams {
			select {
			case <-ev:
				rung[i]++
				total++
				progressed = true
			default:
			}
		}
		if !progressed {
			select {
			case <-deadline:
				t.Fatalf("only %d of %d doorbells delivered (per-worker: %v)", total, workers, rung)
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	for i, n := range rung {
		if n != 1 {
			t.Errorf("worker %d rung %d times, want 1 — rotation should spread %d todos over %d workers (got %v)",
				i, n, workers, workers, rung)
		}
	}
}

// Unicast must not weaken the tenant boundary: picking "one" session means one of the OWNING
// endpoint's sessions, never a bystander that happens to share a queue name.
func TestUnicastDoorbellStillNeverCrossesEndpoints(t *testing.T) {
	f := &fakeStore{byHash: map[string]store.AuthEndpoint{}}
	const slugA, slugB = "tenant-a-55555555", "tenant-b-66666666"
	tokenA := vend(t, f, slugA, []string{"reviews"}, []string{"claim_next"})
	tokenB := vend(t, f, slugB, []string{"reviews"}, []string{"claim_next"})
	ts, h := newTestServerHandler(t, f)

	// B attaches TWO sessions and A only one, so a selection bug that ignored ownership would be
	// more likely to pick a B session than A's.
	sessA := rawInitialize(t, ts.URL+"/mcp/"+slugA, tokenA)
	eventsA, _ := sessA.openStream()
	var bStreams []<-chan notification
	for i := 0; i < 2; i++ {
		s := rawInitialize(t, ts.URL+"/mcp/"+slugB, tokenB)
		ev, _ := s.openStream()
		bStreams = append(bStreams, ev)
	}

	owned := store.Todo{
		EndpointID: endpointIDFor(slugA), ID: "td_a_only", Queue: "reviews",
		Kind: "pull_request", Source: "github", Title: "A's private work",
	}
	publishUntilDelivered(t, h, owned, eventsA)

	for i, ev := range bStreams {
		select {
		case n := <-ev:
			t.Fatalf("cross-tenant leak under unicast: B session %d received %+v", i, n)
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// --- deaf-consumer visibility ---

// A session that is attached and initialized but cannot receive is indistinguishable from a healthy
// one from every external signal: the session is connected, the queue fills, and the agent does
// nothing. Diagnosing one instance of that cost most of a working session, because both outcomes of
// a push were Debug-only. This asserts the warning that makes it sayable — and that it is emitted
// ONCE per failure run, not once per push, so a busy queue cannot turn it into a flood.
func TestDoorbellWarnsOnceWhenAConsumerIsPersistentlyDeaf(t *testing.T) {
	var buf syncBuffer
	h := New(&fakeStore{byHash: map[string]store.AuthEndpoint{}},
		slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(h.Close)

	s := &mcpSession{
		id: "sess-deaf", endpointID: "ep-1", slug: "deaf-agent-1234",
		queues: map[string]bool{"forge": true}, doorbells: make(chan store.Todo, doorbellBuffer),
		conn: failingConn{},
	}
	h.wg.Add(1)
	go h.pump(s)

	for i := 0; i < doorbellDeafThreshold+5; i++ {
		s.doorbells <- store.Todo{EndpointID: "ep-1", ID: "td", Queue: "forge", Title: "work"}
	}
	close(s.doorbells)
	out := waitForLog(t, &buf, "doorbell undeliverable")
	if n := strings.Count(out, "doorbell undeliverable"); n != 1 {
		t.Fatalf("warned %d times, want exactly 1 — one line per broken session, not per push\n%s", n, out)
	}
	for _, want := range []string{"deaf-agent-1234", "sess-deaf", "consecutive_failures"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning is missing %q — it must name which consumer is deaf\n%s", want, out)
		}
	}
}

// A single failure is routine (the agent simply has no stream open at that instant) and must stay
// quiet, or the warning is noise and gets ignored.
func TestDoorbellDoesNotWarnOnAnIsolatedFailure(t *testing.T) {
	var buf syncBuffer
	h := New(&fakeStore{byHash: map[string]store.AuthEndpoint{}},
		slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(h.Close)

	s := &mcpSession{
		id: "sess-blip", endpointID: "ep-1", slug: "blippy-1234",
		queues: map[string]bool{"forge": true}, doorbells: make(chan store.Todo, doorbellBuffer),
		conn: failingConn{},
	}
	h.wg.Add(1)
	go h.pump(s)
	s.doorbells <- store.Todo{EndpointID: "ep-1", ID: "td", Queue: "forge", Title: "work"}
	close(s.doorbells)
	// Give the pump time to drain and (wrongly) warn, so the assertion is meaningful rather than
	// just winning a race.
	waitForLog(t, &buf, "doorbell dropped")
	time.Sleep(100 * time.Millisecond)
	if strings.Contains(buf.String(), "doorbell undeliverable") {
		t.Fatalf("warned on a single failure; that is routine and must stay at Debug\n%s", buf.String())
	}
}

// --- helpers for the deaf-consumer tests ---

// syncBuffer is a bytes.Buffer safe for the pump goroutine to write into while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// failingConn is a Connection whose every Write fails, standing in for the real condition: a
// session that is attached and initialized but holds no notification stream, so the transport
// rejects the push.
type failingConn struct{}

func (failingConn) Read(context.Context) (jsonrpc.Message, error) {
	return nil, errors.New("failingConn: no reads")
}
func (failingConn) Write(context.Context, jsonrpc.Message) error {
	return errors.New("no open notification stream")
}
func (failingConn) Close() error      { return nil }
func (failingConn) SessionID() string { return "sess-failing" }

// waitForLog blocks until want appears in buf, or fails the test. The pump runs on its own
// goroutine and h.wg cannot be waited on here — it also tracks the handler's janitor, which lives
// until Close.
func waitForLog(t *testing.T, buf *syncBuffer, want string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if out := buf.String(); strings.Contains(out, want) {
			return out
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in the log; got:\n%s", want, buf.String())
	return ""
}

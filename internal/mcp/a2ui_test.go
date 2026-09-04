package mcp

// Integration tests for the A2UI-over-MCP resource surface (a2ui.go): scope-gated resource
// templates, queue-view rendering, todo-detail rendering, A2UI payload validity, audience
// annotations, and the stable error codes — driven through the real SDK client and server
// over HTTP against the fake store.
//
// Governing: #102 Epic "A2UI resources over MCP".

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/joestump/switchboard/internal/store"
)

// --- unit tests for the pure renderers ---

// TestRenderQueueA2UIWithTodos verifies the queue renderer produces a valid A2UI payload with
// one Card per todo and correct structural references.
func TestRenderQueueA2UIWithTodos(t *testing.T) {
	now := time.Now().Add(-5 * time.Minute)
	todos := []store.Todo{
		{ID: "td_1", Queue: "reviews", Title: "Review PR #42", State: "pending", Attempt: 0, MaxAttempts: 5, CreatedAt: now},
		{ID: "td_2", Queue: "reviews", Title: "Review PR #43", State: "pending", Attempt: 1, MaxAttempts: 5, Owner: "agent:ag-1", CreatedAt: now},
	}
	p := renderQueueA2UI("reviews", todos)

	if p.Version != "v0.9" {
		t.Fatalf("version = %q, want v0.9", p.Version)
	}
	if p.UpdateComponents.SurfaceID == "" {
		t.Fatal("surfaceId is empty")
	}

	ids := componentIDs(p)
	root := findComponent(p, "root")
	if root == nil || root.Component != "Card" {
		t.Fatal("root component must be a Card")
	}
	if root.Child != "col" {
		t.Fatalf("root child = %q, want col", root.Child)
	}

	// Both todo cards must exist.
	for i := range todos {
		cardID := "todo-card-" + itoa(i)
		if !contains(ids, cardID) {
			t.Fatalf("missing todo card %s", cardID)
		}
	}

	// All child references must resolve to real components.
	if err := validateRefs(p); err != nil {
		t.Fatalf("dangling reference: %v", err)
	}

	// The list and every todo card must be REACHABLE from the root through container components
	// (regression: the renderer once parented "list" under the h2 title Text, which a
	// spec-conformant renderer never draws — flat id membership alone green-lit that tree).
	seen := renderedIDs(p)
	if !seen["list"] {
		t.Fatal("list is not reachable from the root through container components")
	}
	for i := range todos {
		if cardID := "todo-card-" + itoa(i); !seen[cardID] {
			t.Fatalf("%s is not reachable from the root", cardID)
		}
	}
}

// TestRenderQueueA2UIEmpty verifies an empty queue renders a placeholder, not an empty list.
func TestRenderQueueA2UIEmpty(t *testing.T) {
	p := renderQueueA2UI("deploys", nil)

	empty := findComponent(p, "empty")
	if empty == nil {
		t.Fatal("empty queue must render an 'empty' text component")
	}
	if !strings.Contains(empty.Text, "No pending") {
		t.Fatalf("empty text = %q, want 'No pending'", empty.Text)
	}

	// No list component should exist on an empty queue.
	if findComponent(p, "list") != nil {
		t.Fatal("empty queue must not have a list component")
	}

	// The empty branch must not leave dangling references (regression: the Column once kept a
	// "count" child that the branch never emitted) and the placeholder must actually render.
	if err := validateRefs(p); err != nil {
		t.Fatalf("dangling reference: %v", err)
	}
	if !renderedIDs(p)["empty"] {
		t.Fatal("empty placeholder is not reachable from the root through container components")
	}
}

// TestRenderTodoA2UI verifies the detail renderer produces a valid payload with action buttons.
func TestRenderTodoA2UI(t *testing.T) {
	now := time.Now().Add(-1 * time.Hour)
	lease := now.Add(5 * time.Minute)
	todo := store.Todo{
		ID: "td_99", Queue: "ci", Title: "CI failed on main", State: "claimed",
		Attempt: 2, MaxAttempts: 5, Owner: "agent:ag-1",
		LeaseExpiresAt: &lease, CreatedAt: now,
		Payload: []byte(`{"repo":"switchboard","branch":"main"}`),
	}
	p := renderTodoA2UI(todo)

	if p.Version != "v0.9" {
		t.Fatalf("version = %q, want v0.9", p.Version)
	}

	// Action buttons must exist with child Text labels.
	for _, btnID := range []string{"btn-claim", "btn-complete", "btn-fail"} {
		btn := findComponent(p, btnID)
		if btn == nil {
			t.Fatalf("missing button %s", btnID)
		}
		if btn.Component != "Button" {
			t.Fatalf("%s component = %q, want Button", btnID, btn.Component)
		}
		if btn.Child == "" {
			t.Fatalf("%s has no child label", btnID)
		}
		label := findComponent(p, btn.Child)
		if label == nil || label.Text == "" {
			t.Fatalf("%s child label %q is missing or empty", btnID, btn.Child)
		}
	}

	// Buttons must not carry a text/label field directly (A2UI spec anti-pattern).
	for _, btnID := range []string{"btn-claim", "btn-complete", "btn-fail"} {
		btn := findComponent(p, btnID)
		if btn.Text != "" {
			t.Fatalf("button %s must not have a text field (use child Text instead)", btnID)
		}
	}

	// Payload section must exist for a todo with payload.
	if findComponent(p, "payload") == nil {
		t.Fatal("todo with payload must render a payload section")
	}

	if err := validateRefs(p); err != nil {
		t.Fatalf("dangling reference: %v", err)
	}
}

// TestRenderTodoA2UINoPayload verifies a todo without a payload omits the payload section.
func TestRenderTodoA2UINoPayload(t *testing.T) {
	todo := store.Todo{
		ID: "td_100", Queue: "reviews", Title: "no payload", State: "pending",
		Attempt: 0, MaxAttempts: 5, CreatedAt: time.Now(),
	}
	p := renderTodoA2UI(todo)

	if findComponent(p, "payload") != nil {
		t.Fatal("todo without payload must not render a payload section")
	}
	if err := validateRefs(p); err != nil {
		t.Fatalf("dangling reference: %v", err)
	}
}

// TestRenderTodoA2UIButtonsHaveNoText is a spec-specific regression guard: a Button with a text
// field is the #1 A2UI authoring mistake, and a payload round-trip must never introduce one.
func TestRenderTodoA2UIButtonsHaveNoText(t *testing.T) {
	p := renderTodoA2UI(store.Todo{ID: "td_x", Queue: "q", Title: "x", State: "pending", CreatedAt: time.Now()})
	for _, c := range p.UpdateComponents.Components {
		if c.Component == "Button" && c.Text != "" {
			t.Fatalf("button %s has text %q — must use child Text instead", c.ID, c.Text)
		}
	}
}

// --- unit tests for helpers ---

func TestURISegment(t *testing.T) {
	cases := []struct {
		uri, segment, want string
	}{
		{"switchboard://queue/reviews/a2ui", "queue", "reviews"},
		{"switchboard://todo/td_123/a2ui", "todo", "td_123"},
		// An A2UI host's optional ?w= width hint rides on the /a2ui suffix; the
		// segment extraction must strip it rather than choke on "a2ui?w=...".
		{"switchboard://queue/reviews/a2ui?w=120", "queue", "reviews"},
		{"switchboard://todo/td_123/a2ui?w=80", "todo", "td_123"},
		{"switchboard://queue//a2ui", "queue", ""},
		{"switchboard://todo/td_1/nota2ui", "todo", ""},
		{"switchboard://queue/reviews/extra/a2ui", "queue", ""},
		{"", "queue", ""},
	}
	for _, tc := range cases {
		got := uriSegment(tc.uri, tc.segment)
		if got != tc.want {
			t.Errorf("uriSegment(%q, %q) = %q, want %q", tc.uri, tc.segment, got, tc.want)
		}
	}
}

func TestSanitizeID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"reviews", "reviews"},
		{"td_123", "td-123"},
		{"queue/sub", "queue-sub"},
		{"ok!@#", "ok---"},
		{"", ""},
	}
	for _, tc := range cases {
		got := sanitizeID(tc.in)
		if got != tc.want {
			t.Errorf("sanitizeID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAgeSince(t *testing.T) {
	if got := ageSince(time.Now().Add(-30 * time.Second)); !strings.HasSuffix(got, "s") {
		t.Errorf("ageSince(30s ago) = %q, want suffix 's'", got)
	}
	if got := ageSince(time.Now().Add(-5 * time.Minute)); !strings.HasSuffix(got, "m") {
		t.Errorf("ageSince(5m ago) = %q, want suffix 'm'", got)
	}
	if got := ageSince(time.Now().Add(-3 * time.Hour)); !strings.HasSuffix(got, "h") {
		t.Errorf("ageSince(3h ago) = %q, want suffix 'h'", got)
	}
	if got := ageSince(time.Now().Add(-48 * time.Hour)); !strings.HasSuffix(got, "d") {
		t.Errorf("ageSince(48h ago) = %q, want suffix 'd'", got)
	}
}

func TestInsertBefore(t *testing.T) {
	s := []string{"a", "b", "c"}
	got := insertBefore(s, "c", "x", "y")
	want := []string{"a", "b", "x", "y", "c"}
	if !sliceEq(got, want) {
		t.Fatalf("insertBefore = %v, want %v", got, want)
	}

	// Target not found: append at end.
	got = insertBefore(s, "z", "x")
	want = []string{"a", "b", "c", "x"}
	if !sliceEq(got, want) {
		t.Fatalf("insertBefore (target not found) = %v, want %v", got, want)
	}
}

// --- integration tests (real SDK over HTTP) ---

// a2uiSession is session() with the A2UI capability flag enabled (ADR-0023 default-off gate):
// these tests exercise the gated surface, so they flip the flag on the very handler the test
// server serves, then connect as usual.
func a2uiSession(t *testing.T, ctx context.Context, f *fakeStore, queues, verbs []string) *sdk.ClientSession {
	t.Helper()
	token := vend(t, f, "agent-a-11111111", queues, verbs)
	ts, h := newTestServerHandler(t, f)
	h.SetA2UIEnabled(true)
	cs, err := connect(t, ctx, ts.URL+"/mcp/agent-a-11111111", token)
	if err != nil {
		t.Fatalf("initialize handshake: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestA2UIQueueResourceHappyPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	f.putTodo(store.Todo{EndpointID: defaultTestEndpointID, ID: "td_1", Queue: "reviews",
		Title: "Review PR #42", State: "pending", Payload: []byte(`{"pr":42}`)})
	f.putTodo(store.Todo{EndpointID: defaultTestEndpointID, ID: "td_2", Queue: "reviews",
		Title: "Review PR #43", State: "pending"})
	// A done todo must not appear in the pending-only view.
	f.putTodo(store.Todo{EndpointID: defaultTestEndpointID, ID: "td_3", Queue: "reviews",
		Title: "Already done", State: "done"})
	cs := a2uiSession(t, ctx, f, []string{"reviews"}, []string{"list_todos"})

	res, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "switchboard://queue/reviews/a2ui"})
	if err != nil {
		t.Fatalf("resources/read: %v", err)
	}
	if len(res.Contents) != 1 {
		t.Fatalf("contents len = %d, want 1", len(res.Contents))
	}
	if res.Contents[0].MIMEType != a2uiMIMEType {
		t.Fatalf("MIMEType = %q, want %q", res.Contents[0].MIMEType, a2uiMIMEType)
	}

	var p a2uiPayload
	if err := json.Unmarshal([]byte(res.Contents[0].Text), &p); err != nil {
		t.Fatalf("unmarshal a2ui payload: %v", err)
	}
	if p.Version != "v0.9" {
		t.Fatalf("version = %q, want v0.9", p.Version)
	}
	if err := validateRefs(p); err != nil {
		t.Fatalf("dangling reference: %v", err)
	}
	// The done todo must not be present in the text payload.
	if strings.Contains(res.Contents[0].Text, "Already done") {
		t.Fatal("done todo must not appear in pending-only queue view")
	}
	if !strings.Contains(res.Contents[0].Text, "Review PR #42") {
		t.Fatal("pending todo title must appear in the payload")
	}
}

// TestA2UIResourceWidthHint: an A2UI host (Crush's read_mcp_resource) appends a ?w= width hint
// to every /a2ui URI it reads. The surfaces ignore the value, but the SDK's {?w} template
// matching must still route the read, and uriSegment must strip the query before extracting the
// path variable — a template registered without {?w} 404s on the hinted URI, and a uriSegment
// that doesn't strip the query extracts nothing. Both queue and todo surfaces must accept it.
func TestA2UIResourceWidthHint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	f.putTodo(store.Todo{EndpointID: defaultTestEndpointID, ID: "td_1", Queue: "reviews",
		Title: "Review PR #42", State: "pending", Payload: []byte(`{"pr":42}`)})
	cs := a2uiSession(t, ctx, f, []string{"reviews"}, []string{"list_todos"})

	for _, uri := range []string{
		"switchboard://queue/reviews/a2ui?w=120",
		"switchboard://todo/td_1/a2ui?w=80",
	} {
		res, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri})
		if err != nil {
			t.Fatalf("resources/read %s: %v", uri, err)
		}
		if len(res.Contents) != 1 || res.Contents[0].MIMEType != a2uiMIMEType {
			t.Fatalf("read %s: contents = %+v, want one %q part", uri, res.Contents, a2uiMIMEType)
		}
		if !strings.Contains(res.Contents[0].Text, "Review PR #42") {
			t.Fatalf("read %s: payload missing the todo title", uri)
		}
	}
}

// TestA2UIQueueResourceEmptyQueue: reading a queue with no pending todos returns a valid
// payload with the placeholder message.
func TestA2UIQueueResourceEmptyQueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	cs := a2uiSession(t, ctx, f, []string{"reviews"}, []string{"list_todos"})

	res, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "switchboard://queue/reviews/a2ui"})
	if err != nil {
		t.Fatalf("resources/read: %v", err)
	}
	var p a2uiPayload
	if err := json.Unmarshal([]byte(res.Contents[0].Text), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if findComponent(p, "empty") == nil {
		t.Fatal("empty queue must render a placeholder")
	}
	if err := validateRefs(p); err != nil {
		t.Fatalf("dangling reference: %v", err)
	}
}

// TestA2UIQueueResourceForbiddenQueue: reading a queue outside the endpoint's scope is a
// stable forbidden error. Governing: SPEC-0006 REQ "Scope Enforcement at the Boundary".
func TestA2UIQueueResourceForbiddenQueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	f.putTodo(store.Todo{EndpointID: defaultTestEndpointID, ID: "td_1", Queue: "deploys",
		Title: "deploy", State: "pending"})
	cs := a2uiSession(t, ctx, f, []string{"reviews"}, []string{"list_todos"})

	_, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "switchboard://queue/deploys/a2ui"})
	if err == nil {
		t.Fatal("reading an out-of-scope queue must fail")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("error = %v, want 'forbidden'", err)
	}
}

// TestA2UITodoResourceHappyPath: reading switchboard://todo/{id}/a2ui returns a valid detail
// payload with metadata, payload summary, and action buttons.
func TestA2UITodoResourceHappyPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	lease := time.Now().Add(5 * time.Minute)
	f.putTodo(store.Todo{
		EndpointID: defaultTestEndpointID, ID: "td_99", Queue: "reviews",
		Title: "Review PR #99", State: "claimed", Attempt: 1, MaxAttempts: 5,
		Owner: "agent:ag-1", LeaseExpiresAt: &lease,
		Payload: []byte(`{"repo":"switchboard","branch":"main"}`),
	})
	cs := a2uiSession(t, ctx, f, []string{"reviews"}, []string{"list_todos"})

	res, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "switchboard://todo/td_99/a2ui"})
	if err != nil {
		t.Fatalf("resources/read: %v", err)
	}
	if res.Contents[0].MIMEType != a2uiMIMEType {
		t.Fatalf("MIMEType = %q, want %q", res.Contents[0].MIMEType, a2uiMIMEType)
	}

	var p a2uiPayload
	if err := json.Unmarshal([]byte(res.Contents[0].Text), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := validateRefs(p); err != nil {
		t.Fatalf("dangling reference: %v", err)
	}

	// Action buttons are present.
	for _, btnID := range []string{"btn-claim", "btn-complete", "btn-fail"} {
		if findComponent(p, btnID) == nil {
			t.Fatalf("missing %s", btnID)
		}
	}

	// Payload appears.
	if findComponent(p, "payload") == nil {
		t.Fatal("payload section missing")
	}
}

// TestA2UITodoResourceNotFound: reading a todo that does not exist returns not_found.
func TestA2UITodoResourceNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	cs := a2uiSession(t, ctx, f, []string{"reviews"}, []string{"list_todos"})

	_, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "switchboard://todo/nonexistent/a2ui"})
	if err == nil {
		t.Fatal("reading a nonexistent todo must fail")
	}
	if !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("error = %v, want 'not_found'", err)
	}
}

// TestA2UITodoResourceStoreFailure: a store failure on GetTodo surfaces as a stable internal
// error, not a raw panic or stack trace.
func TestA2UITodoResourceStoreFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	f.failErr = errors.New("db is down")
	cs := a2uiSession(t, ctx, f, []string{"reviews"}, []string{"list_todos"})

	_, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "switchboard://todo/td_1/a2ui"})
	if err == nil {
		t.Fatal("reading with a store failure must return an error")
	}
	if !strings.Contains(err.Error(), "internal") {
		t.Fatalf("error = %v, want 'internal'", err)
	}
}

// TestA2UIResourceNotAdvertisedWithoutScope: an endpoint without list_todos scope must not
// advertise the A2UI resource templates at all. Governing: #102 scope gating.
func TestA2UIResourceNotAdvertisedWithoutScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	// Vend with only claim scope — no list_todos.
	cs := a2uiSession(t, ctx, f, []string{"reviews"}, []string{"claim"})

	templates, err := cs.ListResourceTemplates(ctx, nil)
	if err != nil {
		t.Fatalf("list resource templates: %v", err)
	}
	for _, rt := range templates.ResourceTemplates {
		if strings.Contains(rt.URITemplate, "a2ui") {
			t.Fatalf("A2UI template advertised without list_todos scope: %s", rt.URITemplate)
		}
	}
}

// TestA2UIResourceAdvertisedWithScope: an endpoint with list_todos scope sees both A2UI
// resource templates.
func TestA2UIResourceAdvertisedWithScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	cs := a2uiSession(t, ctx, f, []string{"reviews"}, []string{"list_todos"})

	templates, err := cs.ListResourceTemplates(ctx, nil)
	if err != nil {
		t.Fatalf("list resource templates: %v", err)
	}
	var sawQueue, sawTodo bool
	for _, rt := range templates.ResourceTemplates {
		if rt.MIMEType != a2uiMIMEType {
			continue
		}
		switch rt.URITemplate {
		case "switchboard://queue/{queue}/a2ui{?w}":
			sawQueue = true
		case "switchboard://todo/{id}/a2ui{?w}":
			sawTodo = true
		}
	}
	if !sawQueue {
		t.Fatal("queue_a2ui template not advertised")
	}
	if !sawTodo {
		t.Fatal("todo_a2ui template not advertised")
	}
}

// TestA2UIQueueResourceStoreFailure: a store failure on ListTodos surfaces as internal.
func TestA2UIQueueResourceStoreFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	f.failErr = errors.New("db is down")
	cs := a2uiSession(t, ctx, f, []string{"reviews"}, []string{"list_todos"})

	_, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: "switchboard://queue/reviews/a2ui"})
	if err == nil {
		t.Fatal("reading with a store failure must return an error")
	}
	if !strings.Contains(err.Error(), "internal") {
		t.Fatalf("error = %v, want 'internal'", err)
	}
}

// TestA2UIInstructionsMentionSurface: the session instructions now mention the A2UI resources.
func TestA2UIInstructionsMentionSurface(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	cs := a2uiSession(t, ctx, f, []string{"reviews"}, []string{"list_todos"})

	init := cs.InitializeResult()
	if !strings.Contains(init.Instructions, "a2ui") {
		t.Fatalf("instructions do not mention a2ui: %q", init.Instructions)
	}
}

// --- A2UI structural validation helpers ---

// validateRefs walks every component's child/children and confirms each target exists in the
// components list. This catches the most common A2UI authoring bug: a dangling reference that
// renders as "[a2tea: missing component ...]".
func validateRefs(p a2uiPayload) error {
	idSet := map[string]bool{}
	for _, c := range p.UpdateComponents.Components {
		idSet[c.ID] = true
	}
	for _, c := range p.UpdateComponents.Components {
		if c.Child != "" && !idSet[c.Child] {
			return errors.New("component " + c.ID + " references missing child " + c.Child)
		}
		for _, child := range c.Children {
			if !idSet[child] {
				return errors.New("component " + c.ID + " references missing child " + child)
			}
		}
	}
	return nil
}

// renderedIDs walks the component tree from "root" via Child/Children and returns the set of ids
// a renderer actually draws. Text components are leaves in A2UI — a spec-conformant renderer
// draws nothing beneath them — so the walk does not descend into a Text's children. This is what
// catches a subtree mis-parented under a leaf: validateRefs alone accepts it because every ref
// still resolves.
func renderedIDs(p a2uiPayload) map[string]bool {
	byID := map[string]a2uiComponent{}
	for _, c := range p.UpdateComponents.Components {
		byID[c.ID] = c
	}
	seen := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		c, ok := byID[id]
		if !ok || seen[id] {
			return
		}
		seen[id] = true
		if c.Component == "Text" {
			return
		}
		if c.Child != "" {
			walk(c.Child)
		}
		for _, child := range c.Children {
			walk(child)
		}
	}
	walk("root")
	return seen
}

func componentIDs(p a2uiPayload) []string {
	ids := make([]string, 0, len(p.UpdateComponents.Components))
	for _, c := range p.UpdateComponents.Components {
		ids = append(ids, c.ID)
	}
	return ids
}

func findComponent(p a2uiPayload, id string) *a2uiComponent {
	for i := range p.UpdateComponents.Components {
		if p.UpdateComponents.Components[i].ID == id {
			return &p.UpdateComponents.Components[i]
		}
	}
	return nil
}

func contains(slice []string, v string) bool {
	for _, s := range slice {
		if s == v {
			return true
		}
	}
	return false
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestA2UIHiddenWhenFlagOff pins the ADR-0023 default: with the capability flag left off, a session
// with full list_todos scope sees NO a2ui resource templates, cannot read the a2ui surfaces, and is
// not told about them in the instructions block — hidden, never half-open or falsely advertised.
// Governing: ADR-0023 REQ "Feature Flags Hide Advanced Surfaces".
func TestA2UIHiddenWhenFlagOff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newFakeStore()
	f.putTodo(store.Todo{EndpointID: defaultTestEndpointID, ID: "td_1", Queue: "reviews",
		Title: "Review PR #42", State: "pending"})
	cs := session(t, ctx, f, []string{"reviews"}, []string{"list_todos"}) // the flag is never flipped

	templates, err := cs.ListResourceTemplates(ctx, nil)
	if err != nil {
		t.Fatalf("list resource templates: %v", err)
	}
	for _, rt := range templates.ResourceTemplates {
		if strings.Contains(rt.URITemplate, "a2ui") {
			t.Fatalf("A2UI template advertised with the flag off: %s", rt.URITemplate)
		}
	}
	for _, uri := range []string{"switchboard://queue/reviews/a2ui", "switchboard://todo/td_1/a2ui"} {
		if _, err := cs.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri}); err == nil {
			t.Fatalf("%s readable with the flag off", uri)
		}
	}
	if init := cs.InitializeResult(); strings.Contains(init.Instructions, "a2ui") {
		t.Fatalf("instructions advertise A2UI with the flag off: %q", init.Instructions)
	}
}

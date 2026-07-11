// A2A friend-request intake proofs (SPEC-0010, ADR-0010/0011). These build the REAL router (the same
// newRouter Run uses) with fake store + provenance verifier, so assertions bind to the wired
// middleware stack — secureHeaders, the per-IP rate limiter, and the 64 KiB body cap — not a
// hand-rolled one. The store/OIDC internals are faked; the intake's own logic (verify-before-write,
// validation, quota, edge recording) is what is under test.
// Governing: SPEC-0010 REQ "Verifiable OIDC-Signed Provenance", REQ "Friend-Request Lifecycle",
// REQ "Anti-Spam — Bounded Discovery and Quotas", "Security Requirements".
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/auth"
	"github.com/joestump/switchboard/internal/config"
	"github.com/joestump/switchboard/internal/ingest"
	mcpsrv "github.com/joestump/switchboard/internal/mcp"
	"github.com/joestump/switchboard/internal/store"
	"github.com/joestump/switchboard/internal/web"
)

// --- fakes ---------------------------------------------------------------------------------------

type fakeFriendStore struct {
	persona    store.Persona
	personaErr error
	human      store.Human
	humanErr   error
	liveCount  int
	countErr   error
	createEdge store.FriendEdge
	createErr  error

	personaCalled bool
	created       *store.CreateFriendRequestParams // non-nil once CreateFriendRequest is called
	approvalTodo  *store.ApprovalTodoParams        // non-nil once CreateApprovalTodo is called
}

func (f *fakeFriendStore) PublishedPersonaByID(_ context.Context, _ string) (store.Persona, error) {
	f.personaCalled = true
	return f.persona, f.personaErr
}

func (f *fakeFriendStore) UpsertHuman(_ context.Context, _, _, _ string) (store.Human, error) {
	return f.human, f.humanErr
}

func (f *fakeFriendStore) CountLiveFriendRequestsFrom(_ context.Context, _ string) (int, error) {
	return f.liveCount, f.countErr
}

func (f *fakeFriendStore) CreateFriendRequest(_ context.Context, p store.CreateFriendRequestParams) (store.FriendEdge, error) {
	cp := p
	f.created = &cp
	return f.createEdge, f.createErr
}

func (f *fakeFriendStore) CreateApprovalTodo(_ context.Context, p store.ApprovalTodoParams) (store.Todo, bool, error) {
	cp := p
	f.approvalTodo = &cp
	return store.Todo{}, true, nil
}

type fakeVerifier struct {
	subject, name, email string
	err                  error
}

func (f fakeVerifier) VerifyProvenance(_ context.Context, _ string) (string, string, string, error) {
	return f.subject, f.name, f.email, f.err
}

// friendRouter builds the production route table with a faked friend-intake store + verifier. Every
// other surface uses a nil-pool store (the friend intake never touches it). A fresh router per test
// means a fresh rate limiter, so per-IP throttling never bleeds across cases.
func friendRouter(t *testing.T, fs friendIntakeStore, v provenanceVerifier) chi.Router {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{BaseURL: "https://sb.example.com"}
	st := store.New(nil)
	authr, err := auth.New(context.Background(), cfg, st, log)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	webh, err := web.New(st, cfg, log)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	hub := ingest.NewHub()
	mcph := mcpsrv.New(st, log)
	t.Cleanup(mcph.Close)
	return newRouter(routerDeps{
		st:      st,
		authr:   authr,
		webh:    webh,
		ing:     ingest.New(st, hub, log, ingest.Config{}),
		mcp:     mcph,
		friends: newFriendIntake(fs, v, log),
		ping:    func(context.Context) error { return nil },
		log:     log,
	})
}

func postIntake(t *testing.T, r chi.Router, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/a2a/friend-requests", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// validBody is a well-formed request; individual tests mutate it.
const validBody = `{"to_persona":"11111111-1111-1111-1111-111111111111","from_persona":"peer://a",` +
	`"requested_queues":["reviews"],"requested_verbs":["create_for"],"reason":"hand you PR reviews",` +
	`"provenance":"eyJraWQ.signed.token"}`

func okStore() *fakeFriendStore {
	return &fakeFriendStore{
		persona:    store.Persona{ID: "11111111-1111-1111-1111-111111111111", OwnerHumanID: "owner-1"},
		human:      store.Human{ID: "req-human-1", OIDCSubject: "pocket|alice"},
		createEdge: store.FriendEdge{ID: "edge-1", State: "pending", ProvenanceVerified: true},
	}
}

// --- tests ---------------------------------------------------------------------------------------

// Valid provenance → a PENDING incoming edge is recorded with the attested subject as from_human,
// provenance_verified true, and to_human resolved from the published target persona's owner.
func TestFriendIntakeValidProvenanceRecordsEdge(t *testing.T) {
	fs := okStore()
	r := friendRouter(t, fs, fakeVerifier{subject: "pocket|alice", name: "Alice"})

	rec := postIntake(t, r, validBody)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if fs.created == nil {
		t.Fatal("expected a pending edge to be recorded")
	}
	if !fs.created.ProvenanceVerified {
		t.Error("edge must be recorded with provenance_verified = true")
	}
	if fs.created.FromHuman != "req-human-1" {
		t.Errorf("from_human = %q, want the attested requester id", fs.created.FromHuman)
	}
	if fs.created.ToHuman != "owner-1" {
		t.Errorf("to_human = %q, want the target persona's owner", fs.created.ToHuman)
	}
	if fs.created.ToPersona != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("to_persona = %q, want the resolved persona id", fs.created.ToPersona)
	}
	// The pending edge also surfaces as a durable approval todo carrying the legible who/why for the
	// target human (SPEC-0010 "Approval Delivered as a Todo", #63).
	if fs.approvalTodo == nil {
		t.Fatal("expected an approval todo to be filed for the target human")
	}
	if fs.approvalTodo.EdgeID != "edge-1" || fs.approvalTodo.ToHuman != "owner-1" ||
		fs.approvalTodo.FromHuman != "req-human-1" || !fs.approvalTodo.ProvenanceVerified {
		t.Errorf("approval todo lost request context: %+v", fs.approvalTodo)
	}
	// Response envelope.
	var resp struct {
		RequestID          string `json:"request_id"`
		State              string `json:"state"`
		ProvenanceVerified bool   `json:"provenance_verified"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.RequestID != "edge-1" || resp.State != "pending" || !resp.ProvenanceVerified {
		t.Errorf("unexpected response: %+v", resp)
	}
	// Security headers on the intake response (secureHeaders is the outermost middleware).
	h := rec.Header()
	if h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("Referrer-Policy") != "strict-origin-when-cross-origin" || h.Get("Content-Security-Policy") == "" {
		t.Errorf("missing required security headers: %+v", h)
	}
}

// Missing provenance → unauthenticated, no edge, and the persona is never probed (verify-before-lookup).
func TestFriendIntakeMissingProvenanceRejected(t *testing.T) {
	fs := okStore()
	r := friendRouter(t, fs, fakeVerifier{subject: "pocket|alice"})

	body := `{"to_persona":"11111111-1111-1111-1111-111111111111","from_persona":"peer://a",` +
		`"requested_verbs":["create_for"],"reason":"x","provenance":""}`
	rec := postIntake(t, r, body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if fs.created != nil {
		t.Error("no pending edge must be created for a request lacking provenance")
	}
	if fs.personaCalled {
		t.Error("an unauthenticated request must not probe persona existence")
	}
}

// Invalid provenance (verifier error) → unauthenticated, no edge, no persona probe.
func TestFriendIntakeInvalidProvenanceRejected(t *testing.T) {
	fs := okStore()
	r := friendRouter(t, fs, fakeVerifier{err: errors.New("token signature invalid")})

	rec := postIntake(t, r, validBody)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if fs.created != nil || fs.personaCalled {
		t.Error("invalid provenance must create no edge and probe no persona")
	}
}

// OIDC not configured → 503 (a server-capability gap, distinct from a caller's bad token).
func TestFriendIntakeProvenanceUnavailable(t *testing.T) {
	fs := okStore()
	r := friendRouter(t, fs, fakeVerifier{err: auth.ErrProvenanceUnavailable})

	rec := postIntake(t, r, validBody)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if fs.created != nil {
		t.Error("no edge when provenance cannot be verified")
	}
}

// Malformed JSON → 400.
func TestFriendIntakeMalformedBody(t *testing.T) {
	fs := okStore()
	r := friendRouter(t, fs, fakeVerifier{subject: "pocket|alice"})

	rec := postIntake(t, r, `{"to_persona": "x", not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if fs.created != nil {
		t.Error("malformed body must create no edge")
	}
}

// Unknown field → 400 (DisallowUnknownFields rejects smuggled/typo'd keys).
func TestFriendIntakeUnknownField(t *testing.T) {
	fs := okStore()
	r := friendRouter(t, fs, fakeVerifier{subject: "pocket|alice"})

	body := `{"to_persona":"11111111-1111-1111-1111-111111111111","from_persona":"peer://a",` +
		`"requested_verbs":["create_for"],"provenance":"tok","granted_verbs":["*"]}`
	rec := postIntake(t, r, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// Empty requested_verbs → 400 (a request must ask for something to approve).
func TestFriendIntakeEmptyScopeRejected(t *testing.T) {
	fs := okStore()
	r := friendRouter(t, fs, fakeVerifier{subject: "pocket|alice"})

	body := `{"to_persona":"11111111-1111-1111-1111-111111111111","from_persona":"peer://a",` +
		`"requested_verbs":[],"reason":"x","provenance":"tok"}`
	rec := postIntake(t, r, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if fs.created != nil {
		t.Error("empty-scope request must create no edge")
	}
}

// Unpublished/unknown target persona → 404, no edge (discovery bounded to advertised personas).
func TestFriendIntakeUnpublishedPersona(t *testing.T) {
	fs := okStore()
	fs.personaErr = store.ErrNotFound
	r := friendRouter(t, fs, fakeVerifier{subject: "pocket|alice"})

	rec := postIntake(t, r, validBody)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if fs.created != nil {
		t.Error("an unresolved target persona must create no edge")
	}
}

// Over the per-requester quota → 429, no edge.
func TestFriendIntakeQuotaExceeded(t *testing.T) {
	fs := okStore()
	fs.liveCount = intakeLiveRequestQuota
	r := friendRouter(t, fs, fakeVerifier{subject: "pocket|alice"})

	rec := postIntake(t, r, validBody)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	if fs.created != nil {
		t.Error("an over-quota request must create no pending edge")
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("quota refusal should advertise Retry-After")
	}
}

// A duplicate live request for the same directional pair → 409 (anti-flood unique index).
func TestFriendIntakeDuplicateConflict(t *testing.T) {
	fs := okStore()
	fs.createErr = store.ErrConflict
	r := friendRouter(t, fs, fakeVerifier{subject: "pocket|alice"})

	rec := postIntake(t, r, validBody)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// A body over the 64 KiB cap → 413 (http.MaxBytesReader on the route).
func TestFriendIntakeBodyTooLarge(t *testing.T) {
	fs := okStore()
	r := friendRouter(t, fs, fakeVerifier{subject: "pocket|alice"})

	var buf bytes.Buffer
	buf.WriteString(`{"to_persona":"x","from_persona":"peer://a","requested_verbs":["create_for"],`)
	buf.WriteString(`"reason":"`)
	buf.Write(bytes.Repeat([]byte("A"), 70<<10)) // 70 KiB reason > 64 KiB cap
	buf.WriteString(`","provenance":"tok"}`)

	req := httptest.NewRequest(http.MethodPost, "/a2a/friend-requests", &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	if fs.created != nil {
		t.Error("an oversized body must create no edge")
	}
}

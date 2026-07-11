package ingest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/joestump/switchboard/internal/agentapi"
	"github.com/joestump/switchboard/internal/store"
)

// tokenEqual is the constant-time (crypto/subtle over SHA-256 digests) shared-secret compare.
// Governing: SPEC-0001 REQ "Shared-Secret Token Authentication for Unsigned Webhooks".
func TestTokenEqual(t *testing.T) {
	cases := []struct {
		name string
		want string
		got  string
		ok   bool
	}{
		{"matching token", "s3cret", "s3cret", true},
		{"wrong token", "s3cret", "wrong", false},
		{"empty presented token", "s3cret", "", false},
		{"different length (hash-normalized, still compared)", "s3cret", "s3cret-and-more", false},
		{"prefix is not enough", "s3cret", "s3c", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tokenEqual(tc.want, tc.got); got != tc.ok {
				t.Fatalf("tokenEqual(%q, %q) = %v, want %v", tc.want, tc.got, got, tc.ok)
			}
		})
	}
}

func TestParseGenericProviders(t *testing.T) {
	t.Run("empty input yields no providers", func(t *testing.T) {
		got, err := ParseGenericProviders("")
		if err != nil || len(got) != 0 {
			t.Fatalf("empty input: got %v, %v", got, err)
		}
	})
	t.Run("valid token and open providers", func(t *testing.T) {
		got, err := ParseGenericProviders(
			`{"dockerhub":{"mode":"token","token":"s3cret","queue":"builds"},"lan":{"mode":"open"}}`)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if p := got["dockerhub"]; p.Mode != "token" || p.Token != "s3cret" || p.Queue != "builds" {
			t.Fatalf("dockerhub provider wrong: %+v", p)
		}
		// Queue defaults to the provider name.
		if p := got["lan"]; p.Mode != "open" || p.Queue != "lan" {
			t.Fatalf("lan provider wrong: %+v", p)
		}
	})
	// A mode outside {token, open} MUST fail loudly — nothing may default to open
	// (SPEC-0001 REQ "Explicit Open Trust Mode").
	for _, bad := range []struct{ name, raw string }{
		{"invalid mode", `{"x":{"mode":"signed"}}`},
		{"missing mode", `{"x":{"token":"s3cret"}}`},
		{"malformed JSON", `{"x":`},
		{"empty provider name", `{"":{"mode":"open"}}`},
	} {
		t.Run(bad.name+" is rejected", func(t *testing.T) {
			if _, err := ParseGenericProviders(bad.raw); err == nil {
				t.Fatalf("config %q must be rejected", bad.raw)
			}
		})
	}
}

// genericRequest builds a POST /webhooks/generic/{name} request with the chi URL param populated,
// mirroring how the router invokes the handler.
func genericRequest(name, body string, hdr map[string]string, query string) *http.Request {
	target := "/webhooks/generic/" + name
	if query != "" {
		target += "?" + query
	}
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", name)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// Rejection paths must return the mapped status and persist NOTHING — the nil store proves it:
// any accidental persist attempt would panic the handler. Governing: SPEC-0001 REQ "Shared-Secret
// Token Authentication for Unsigned Webhooks" (403 no persist), REQ "Explicit Open Trust Mode"
// (unknown provider 404, never a fall-through to open).
func TestGenericRejections(t *testing.T) {
	ing := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{
		Generic: map[string]GenericProvider{
			"dockerhub": {Mode: "token", Token: "s3cret", Queue: "builds"},
			"tokenless": {Mode: "token"}, // configured but no token yet → disabled
		},
	})

	cases := []struct {
		name     string
		provider string
		hdr      map[string]string
		query    string
		want     int
	}{
		{"unconfigured provider is 404, not open", "unknown", nil, "", http.StatusNotFound},
		{"missing token", "dockerhub", nil, "", http.StatusForbidden},
		{"wrong bearer token", "dockerhub", map[string]string{"Authorization": "Bearer wrong"}, "", http.StatusForbidden},
		{"wrong dedicated header token", "dockerhub", map[string]string{"X-Webhook-Token": "wrong"}, "", http.StatusForbidden},
		{"wrong URL token", "dockerhub", nil, "token=wrong", http.StatusForbidden},
		{"malformed authorization scheme", "dockerhub", map[string]string{"Authorization": "Basic s3cret"}, "", http.StatusForbidden},
		{"wrong lowercase-bearer token", "dockerhub", map[string]string{"Authorization": "bearer wrong"}, "", http.StatusForbidden},
		{"provider disabled until token set", "tokenless", nil, "", http.StatusForbidden},
		// A disabled provider rejects even an empty presented token (no empty==empty match).
		{"disabled provider rejects empty token", "tokenless", map[string]string{"X-Webhook-Token": ""}, "", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			ing.Generic(rec, genericRequest(tc.provider, `{"push":"event"}`, tc.hdr, tc.query))
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// An oversized generic body is 413 before any token comparison (SPEC-0001 REQ body limits).
func TestGenericRejectsOversizedBody(t *testing.T) {
	ing := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{
		Generic: map[string]GenericProvider{"dockerhub": {Mode: "token", Token: "s3cret"}},
	})
	rec := httptest.NewRecorder()
	req := genericRequest("dockerhub", strings.Repeat("x", maxBody+1),
		map[string]string{"Authorization": "Bearer s3cret"}, "")
	ing.Generic(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized generic body: got %d, want 413", rec.Code)
	}
}

// The Authorization auth-scheme is case-insensitive per RFC 7235 §2.1 — `bearer x` and `BEARER x`
// must present the token exactly like `Bearer x`, while non-Bearer schemes present nothing.
func TestPresentedTokenSchemeCaseInsensitive(t *testing.T) {
	cases := []struct {
		name string
		auth string
		want string
	}{
		{"canonical Bearer", "Bearer s3cret", "s3cret"},
		{"lowercase bearer", "bearer s3cret", "s3cret"},
		{"uppercase BEARER", "BEARER s3cret", "s3cret"},
		{"mixed case BeArEr", "BeArEr s3cret", "s3cret"},
		{"Basic is not Bearer", "Basic s3cret", ""},
		{"bare scheme with no token", "Bearer ", ""},
		{"scheme fragment without space", "Bearers3cret", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := genericRequest("x", "", map[string]string{"Authorization": tc.auth}, "")
			if got := presentedToken(req); got != tc.want {
				t.Fatalf("presentedToken(Authorization: %q) = %q, want %q", tc.auth, got, tc.want)
			}
		})
	}
}

func TestPresentedTokenPrecedence(t *testing.T) {
	// Header (Bearer, then dedicated header) is preferred over the URL fallback.
	req := genericRequest("x", "", map[string]string{
		"Authorization": "Bearer from-bearer", "X-Webhook-Token": "from-header",
	}, "token=from-url")
	if got := presentedToken(req); got != "from-bearer" {
		t.Fatalf("bearer should win: %q", got)
	}
	req = genericRequest("x", "", map[string]string{"X-Webhook-Token": "from-header"}, "token=from-url")
	if got := presentedToken(req); got != "from-header" {
		t.Fatalf("dedicated header should beat URL: %q", got)
	}
	req = genericRequest("x", "", nil, "token=from-url")
	if got := presentedToken(req); got != "from-url" {
		t.Fatalf("URL fallback: %q", got)
	}
}

func decode(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
}

// testIngest builds a full Ingest for accept-path tests against the ingest-owned test database
// (see ingestTestPool in dedup_test.go). Skips cleanly without SWITCHBOARD_TEST_DATABASE_URL
// (Gitea CI is the gate).
func testIngest(t *testing.T, cfg Config) (*Ingest, *store.Store, context.Context, func(string) (trustMode string, verified bool, verifyDetail string)) {
	t.Helper()
	pool, ctx := ingestTestPool(t)
	st := store.New(pool)
	ing := New(st, agentapi.NewHub(), slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	eventRow := func(source string) (string, bool, string) {
		var mode, detail string
		var verified bool
		if err := pool.QueryRow(ctx,
			`SELECT trust_mode, verified, COALESCE(verify_detail,'') FROM events WHERE source=$1`,
			source).Scan(&mode, &verified, &detail); err != nil {
			t.Fatalf("query event for %s: %v", source, err)
		}
		return mode, verified, detail
	}
	return ing, st, ctx, eventRow
}

// Accepted token deliveries persist trust_mode='token', verified=false with a body-not-verified
// detail — never presented as 'signed'. Governing: SPEC-0001 scenario "Correct token is accepted
// as token trust".
func TestGenericTokenAcceptPersists(t *testing.T) {
	ing, _, _, eventRow := testIngest(t, Config{
		Generic: map[string]GenericProvider{"dockerhub": {Mode: "token", Token: "s3cret", Queue: "builds"}},
	})

	rec := httptest.NewRecorder()
	ing.Generic(rec, genericRequest("dockerhub",
		`{"push_data":{"tag":"latest"}}`, map[string]string{"Authorization": "Bearer s3cret"}, ""))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("valid token: got %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"verified":false`) || !strings.Contains(body, `"trust_mode":"token"`) {
		t.Fatalf("response must label token trust unverified: %s", body)
	}
	mode, verified, detail := eventRow("dockerhub")
	if mode != "token" || verified {
		t.Fatalf("persisted trust wrong: mode=%q verified=%v", mode, verified)
	}
	if !strings.Contains(detail, "body not verified") {
		t.Fatalf("verify_detail must state body is not verified: %q", detail)
	}

	// URL-fallback token is also accepted, and the redelivery dedups on the body hash into the
	// same todo (SPEC-0001 REQ "Idempotency Key Extraction and Dedup").
	rec2 := httptest.NewRecorder()
	ing.Generic(rec2, genericRequest("dockerhub", `{"push_data":{"tag":"latest"}}`, nil, "token=s3cret"))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("URL token fallback: got %d, want 202 (body %s)", rec2.Code, rec2.Body.String())
	}
	var first, second struct{ ID string }
	decode(t, rec.Body.Bytes(), &first)
	decode(t, rec2.Body.Bytes(), &second)
	if first.ID == "" || first.ID != second.ID {
		t.Fatalf("redelivery must dedup to the same todo: %q vs %q", first.ID, second.ID)
	}

	// A lowercase `bearer` scheme authenticates too (RFC 7235 case-insensitive schemes) and still
	// dedups into the same todo.
	rec3 := httptest.NewRecorder()
	ing.Generic(rec3, genericRequest("dockerhub",
		`{"push_data":{"tag":"latest"}}`, map[string]string{"Authorization": "bearer s3cret"}, ""))
	if rec3.Code != http.StatusAccepted {
		t.Fatalf("lowercase bearer scheme: got %d, want 202 (body %s)", rec3.Code, rec3.Body.String())
	}
	var third struct{ ID string }
	decode(t, rec3.Body.Bytes(), &third)
	if third.ID != first.ID {
		t.Fatalf("lowercase-bearer redelivery must dedup to the same todo: %q vs %q", third.ID, first.ID)
	}
}

// Open providers exist only by explicit opt-in and persist trust_mode='open', verified=false with a
// plain no-verification detail. Governing: SPEC-0001 scenario "Open delivery is labeled unverified".
func TestGenericOpenAcceptPersists(t *testing.T) {
	ing, _, _, eventRow := testIngest(t, Config{
		Generic: map[string]GenericProvider{"lan": {Mode: "open", Queue: "lan"}},
	})

	rec := httptest.NewRecorder()
	ing.Generic(rec, genericRequest("lan", `{"hello":"world"}`, nil, ""))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("open provider: got %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `"verified":false`) || !strings.Contains(body, `"trust_mode":"open"`) {
		t.Fatalf("response must label open trust unverified: %s", body)
	}
	mode, verified, detail := eventRow("lan")
	if mode != "open" || verified {
		t.Fatalf("persisted trust wrong: mode=%q verified=%v", mode, verified)
	}
	if !strings.Contains(detail, "no verification") {
		t.Fatalf("verify_detail must state no verification: %q", detail)
	}
}

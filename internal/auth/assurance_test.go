// Assurance posture & dev-login guard tests.
//
// Assurance posture: the MVP trusts the Pocket ID issuer and enforces NO amr/acr step-up on login —
// intentionally, because the issuer is passkey-only (ADR-0011). These tests pin that posture: a
// valid ID token logs in whether it carries no amr/acr at all or a non-phishing-resistant one,
// proving no hidden step-up filter exists. The guard for non-passkey issuers lives as a comment at
// the IdP-trust-set config point in New (also pinned here so it cannot be deleted silently).
//
// Dev-login guard: the OIDC-bypassing dev login answers 404 and establishes no session unless
// SWITCHBOARD_DEV_LOGIN is set — in every build, production included — and logs a prominent
// warning when used.
//
// Governing: ADR-0011 (identity assurance deferred), SPEC-0008 REQ "Assurance Posture — Trust the
// Issuer, Step-Up Deferred", REQ "Development Login Guard".
package auth

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/config"
)

// --- assurance posture ----------------------------------------------------------------------------

// SPEC-0008 scenario "No step-up enforced today": login succeeds with a valid token regardless of
// what the amr/acr claims say (or whether they exist), because the trusted issuer is passkey-only
// and step-up verification is deferred per ADR-0011.
func TestLoginSucceedsWithoutAmrAcrStepUp(t *testing.T) {
	cases := []struct {
		name   string
		claims map[string]any
	}{
		// Pocket ID today: passkey login, no assurance claims asserted.
		{"no amr/acr claims", nil},
		// A hypothetical non-phishing-resistant login: if switchboard enforced a step-up check,
		// this token would be rejected. It is not — intentionally, per ADR-0011.
		{"non-phishing-resistant amr", map[string]any{"amr": []string{"pwd"}, "acr": "0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idp := newFakeIdP(t)
			idp.setExtraClaims(tc.claims)
			fs := newFakeStore()
			a := newTestAuth(t, idp, "http://127.0.0.1:8080", fs)

			c, loc := doLogin(t, a)
			idp.setNonce(loc.Query().Get("nonce"))
			resp := doCallback(t, a, loc.Query().Get("state"), c)

			if resp.StatusCode != http.StatusFound {
				t.Fatalf("login must succeed without an amr/acr step-up check (ADR-0011): got %d", resp.StatusCode)
			}
			sc := cookieByName(t, resp, sessionCookie)
			if sc == nil || sc.Value == "" {
				t.Fatal("a session must be established")
			}
			if _, err := fs.SessionHuman(context.Background(), hashToken(sc.Value)); err != nil {
				t.Fatalf("session must resolve to a human: %v", err)
			}
		})
	}
}

// SPEC-0008 scenario "Non-passkey issuer triggers the deferred requirement": the IdP-trust-set
// configuration point (provider init in New) must carry a guard comment referencing ADR-0011 so the
// deferred phishing-resistant amr/acr requirement is confronted whenever a non-passkey issuer is
// added. Pinning the source text is deliberate — the guard IS the comment until step-up code exists.
func TestIdPTrustSetCarriesADR0011Guard(t *testing.T) {
	src, err := os.ReadFile("auth.go")
	if err != nil {
		t.Fatalf("read auth.go: %v", err)
	}
	text := string(src)
	for _, want := range []string{"ADR-0011", "amr/acr", "passkey-only"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the IdP-trust-set config point must carry a guard referencing %q (SPEC-0008)", want)
		}
	}
}

// --- dev-login guard --------------------------------------------------------------------------------

// newDevAuth builds an OIDC-less Authenticator with the given DevLogin flag and log sink.
func newDevAuth(fs *fakeStore, devLogin bool, logDst *bytes.Buffer) *Authenticator {
	log := discardLog()
	if logDst != nil {
		log = slog.New(slog.NewTextHandler(logDst, nil))
	}
	return &Authenticator{
		cfg:   config.Config{BaseURL: "http://127.0.0.1:8080", DevLogin: devLogin},
		store: fs,
		log:   log,
	}
}

func postDevLogin(a *Authenticator) *http.Response {
	rec := httptest.NewRecorder()
	a.DevLogin(rec, httptest.NewRequest(http.MethodPost, "/auth/dev-login", nil))
	return rec.Result()
}

// SPEC-0008 scenario "Dev login is off by default": when SWITCHBOARD_DEV_LOGIN is not set the route
// responds 404 and establishes no session — no cookie, no session record, no human upserted. The
// guard is a runtime config check, so it holds in production builds too.
func TestDevLoginDisabledReturns404AndNoSession(t *testing.T) {
	fs := newFakeStore()
	var buf bytes.Buffer
	a := newDevAuth(fs, false, &buf)

	resp := postDevLogin(a)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("dev-login without SWITCHBOARD_DEV_LOGIN must 404: got %d", resp.StatusCode)
	}
	requireNoSessionCookie(t, resp)
	if len(fs.sessions) != 0 || len(fs.humans) != 0 {
		t.Fatalf("no session may be established: sessions=%d humans=%d", len(fs.sessions), len(fs.humans))
	}
}

// SPEC-0008 REQ "Development Login Guard": when enabled, dev-login mints a real session for the
// fixed local human and logs a prominent warning that OIDC was bypassed.
func TestDevLoginEnabledMintsSessionAndWarnsLoudly(t *testing.T) {
	fs := newFakeStore()
	var buf bytes.Buffer
	a := newDevAuth(fs, true, &buf)

	resp := postDevLogin(a)

	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/" {
		t.Fatalf("enabled dev-login must redirect to /: got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	sc := cookieByName(t, resp, sessionCookie)
	if sc == nil || sc.Value == "" {
		t.Fatal("enabled dev-login must set a session cookie")
	}
	h, err := fs.SessionHuman(context.Background(), hashToken(sc.Value))
	if err != nil || h.OIDCSubject != "dev|local" {
		t.Fatalf("session must resolve to the fixed local dev human: %+v, %v", h, err)
	}

	logged := buf.String()
	if !strings.Contains(logged, "level=WARN") {
		t.Fatalf("dev-login use must log at WARN: %q", logged)
	}
	if !strings.Contains(logged, "OIDC bypassed") {
		t.Fatalf("the warning must state that OIDC was bypassed: %q", logged)
	}
}

// The disabled route must never log the bypass warning (nothing was bypassed) and must behave
// identically whether or not OIDC is configured — the guard is on the flag alone.
func TestDevLoginDisabledIsSilent(t *testing.T) {
	fs := newFakeStore()
	var buf bytes.Buffer
	a := newDevAuth(fs, false, &buf)

	_ = postDevLogin(a)

	if strings.Contains(buf.String(), "DEV LOGIN") {
		t.Fatalf("disabled dev-login must not log the bypass warning: %q", buf.String())
	}
}

// --- structured logging on auth failures ------------------------------------------------------------

// Callback rejections are auth failures and must leave a structured trace (slog key-value), not fail
// silently. State mismatch stands in for the family (missing/malformed state, nonce mismatch,
// verify failure all share the same logging path shape).
func TestCallbackRejectionIsLogged(t *testing.T) {
	idp := newFakeIdP(t)
	fs := newFakeStore()
	a := newTestAuth(t, idp, "http://127.0.0.1:8080", fs)
	var buf bytes.Buffer
	a.log = slog.New(slog.NewTextHandler(&buf, nil))

	c, _ := doLogin(t, a)
	resp := doCallback(t, a, "attacker-forged-state", c)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("forged state must be rejected: got %d", resp.StatusCode)
	}
	logged := buf.String()
	if !strings.Contains(logged, "level=WARN") || !strings.Contains(logged, "state mismatch") {
		t.Fatalf("callback rejection must log a structured WARN with the reason: %q", logged)
	}
}

package web

// The login page forwards a validated return path into the flow: the auth bounce (RequireHuman)
// lands here with ?return_to=, and every login entry — provider links and the dev form — carries
// it onward, so the session-establishing redirect can send the human back to the protected URL
// (a pending /oauth/authorize). An unsafe return_to renders as if absent.
// Governing: SPEC-0016 scenario "Human absent".

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/config"
)

func newOIDCTestHandler(t *testing.T) *Handler {
	t.Helper()
	h, err := New(nil, config.Config{
		BaseURL:            "https://sb.example.com",
		OIDCIssuer:         "https://idp.example.com",
		OIDCClientID:       "sb",
		OIDCClientSecret:   "secret",
		OIDCRedirectURL:    "https://sb.example.com/auth/callback",
		GitHubClientID:     "gh",
		GitHubClientSecret: "ghsecret",
		DevLogin:           true,
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func TestLoginForwardsValidatedReturnPath(t *testing.T) {
	h := newOIDCTestHandler(t)

	rec := httptest.NewRecorder()
	h.Login(rec, httptest.NewRequest(http.MethodGet, "/login?return_to=%2Foauth%2Fauthorize%3Fclient_id%3Dcid", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("login page: got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`href="/auth/login?return_to=%2Foauth%2Fauthorize%3Fclient_id%3Dcid"`,
		`href="/auth/login?provider=github&return_to=%2Foauth%2Fauthorize%3Fclient_id%3Dcid"`,
		`<input type="hidden" name="return_to" value="/oauth/authorize?client_id=cid">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("login page missing %q", want)
		}
	}
}

// An unsafe return_to is dropped before rendering: the links fall back to the bare flow.
func TestLoginDropsUnsafeReturnPath(t *testing.T) {
	h := newOIDCTestHandler(t)

	rec := httptest.NewRecorder()
	h.Login(rec, httptest.NewRequest(http.MethodGet, "/login?return_to="+url.QueryEscape("//evil.example/x"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("login page: got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `href="/auth/login"`) || !strings.Contains(body, `href="/auth/login?provider=github"`) {
		t.Error("unsafe return_to must render the bare login links")
	}
	if strings.Contains(body, "return_to=") {
		t.Error("unsafe return_to must not reach the login page")
	}
}

package server

// The device authorization grant (RFC 8628, issue #308) end to end through the real router +
// PostgreSQL: a headless client registers and starts a request, a signed-in human enters the short
// code and approves (or denies) on the consent screen, and the polling client receives the same
// operator-bound pair the code flow issues — usable on /api/v1, bound to the approving human, and
// bound to no endpoint. Covers the polling states (authorization_pending, slow_down), the single
// use spend, and the user-code entry rate limit. Skipped without SWITCHBOARD_TEST_DATABASE_URL.
// Governing: ADR-0019, ADR-0023, ADR-0057 (proposed).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/switchboard/internal/cred"
	"github.com/stump-wtf/switchboard/internal/oauthsrv"
	"github.com/stump-wtf/switchboard/internal/store"
)

// deviceFixture registers a client and starts a device request over the public surface, returning
// the plaintext codes exactly as the CLI sees them.
func deviceFixture(t *testing.T, r chi.Router, st *store.Store, ctx context.Context, clientID string) (deviceCode, userCode string) {
	t.Helper()
	if _, err := st.CreateOAuthClient(ctx, clientID, "harness installer", []string{"http://127.0.0.1:41234/callback"}); err != nil {
		t.Fatalf("register client: %v", err)
	}
	form := url.Values{"client_id": {clientID}, "resource": {"https://sb.example.com/api"}}
	req := httptest.NewRequest(http.MethodPost, oauthsrv.DeviceAuthorizationPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("device_authorization: %d %s", rec.Code, rec.Body.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("device_authorization document: %v", err)
	}
	for _, k := range []string{"device_code", "user_code", "verification_uri", "verification_uri_complete"} {
		if s, _ := doc[k].(string); s == "" {
			t.Fatalf("device document missing %s: %v", k, doc)
		}
	}
	if doc["verification_uri"] != "https://sb.example.com"+oauthsrv.DeviceConsentPath {
		t.Fatalf("verification_uri = %v", doc["verification_uri"])
	}
	if doc["expires_in"].(float64) != 600 || doc["interval"].(float64) != 5 {
		t.Fatalf("advertised clocks: %v", doc)
	}
	if got := doc["verification_uri_complete"].(string); !strings.HasPrefix(got, "https://sb.example.com"+oauthsrv.DeviceConsentPath+"?user_code=") {
		t.Fatalf("verification_uri_complete = %q", got)
	}
	return doc["device_code"].(string), doc["user_code"].(string)
}

// devicePoll is the CLI's polling call at the public token endpoint.
func devicePoll(t *testing.T, r chi.Router, deviceCode, clientID string) (int, map[string]any) {
	t.Helper()
	return tokenCall(t, r, url.Values{
		"grant_type":  {oauthsrv.DeviceGrantType},
		"device_code": {deviceCode},
		"client_id":   {clientID},
	})
}

func TestDeviceFlowApproveToOperatorAPI(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, session := mintSession(t, st, ctx, "test|device", "Ada Lovelace", "ada@example.com")

	// Scenario "Human absent": the consent page is behind the session like its code-flow sibling.
	anon := anonRequest(t, r, http.MethodGet, oauthsrv.DeviceConsentPath)
	if anon.Code != http.StatusFound || !strings.HasPrefix(anon.Header().Get("Location"), "/login?return_to=") {
		t.Fatalf("anonymous device consent: %d → %q, want a login bounce", anon.Code, anon.Header().Get("Location"))
	}

	deviceCode, userCode := deviceFixture(t, r, st, ctx, "cid-device")

	// Nothing was decided yet, and the device row carries no principal: the only answer is
	// authorization_pending.
	if code, doc := devicePoll(t, r, deviceCode, "cid-device"); code != http.StatusBadRequest || doc["error"] != "authorization_pending" {
		t.Fatalf("pre-approval poll: %d %v", code, doc)
	}
	// A poll straight back is slow_down (the interval is enforced), and slow_down mints nothing.
	if code, doc := devicePoll(t, r, deviceCode, "cid-device"); code != http.StatusBadRequest || doc["error"] != "slow_down" {
		t.Fatalf("fast poll: %d %v, want slow_down", code, doc)
	}

	c := newWizClient(t, r, session)
	page := c.get(oauthsrv.DeviceConsentPath + "?user_code=" + url.QueryEscape(userCode))
	if page.Code != http.StatusOK {
		t.Fatalf("entry GET with code: %d %.300s", page.Code, page.Body.String())
	}
	body := page.Body.String()
	for _, want := range []string{"harness installer", "act as you", "operator ·", "data-sb-device-consent",
		"register agents on this switchboard", oauthsrv.UserCodeDisplay(userCode)} {
		if !strings.Contains(body, want) {
			t.Errorf("device consent page missing %q", want)
		}
	}
	// The decision never redirects anywhere: the client polls, it cannot be bounced.
	csrf := scrapeCSRF(t, body)
	rec := c.do(http.MethodPost, oauthsrv.DeviceConsentPath, url.Values{
		"csrf_token": {csrf}, "user_code": {oauthsrv.UserCodeDisplay(userCode)}, "decision": {"approve"},
	})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "data-sb-device-done") {
		t.Fatalf("approve: %d, want the finished state on this page", rec.Code)
	}
	if rec.Header().Get("Location") != "" {
		t.Fatalf("the device consent must never redirect, got %q", rec.Header().Get("Location"))
	}

	// The next poll lands the pair. The row now names the approving human and is spent.
	code, doc := devicePoll(t, r, deviceCode, "cid-device")
	if code != http.StatusOK || doc["access_token"] == "" || doc["refresh_token"] == "" {
		t.Fatalf("post-approval poll: %d %v, want a token pair", code, doc)
	}
	access, _ := doc["access_token"].(string)
	if actor, err := st.HumanByOAuthToken(ctx, cred.Hash(access)); err != nil || actor.ID != human.ID {
		t.Fatalf("device grant is not bound to the approving human: %+v err=%v", actor, err)
	}
	if _, err := st.EndpointByOAuthToken(ctx, cred.Hash(access)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("device grant resolved to an endpoint credential: err=%v, want ErrNotFound", err)
	}
	api := apiCall(t, r, http.MethodGet, "/api/v1/endpoints", access, "")
	if api.Code != http.StatusOK {
		t.Fatalf("operator API with the device grant: %d %s", api.Code, api.Body.String())
	}

	// Single use: the grant cannot be redeemed twice; the refresh it handed out is the only
	// continuing path.
	if code, doc := devicePoll(t, r, deviceCode, "cid-device"); code != http.StatusBadRequest || doc["error"] != "invalid_grant" {
		t.Fatalf("second spend: %d %v, want invalid_grant", code, doc)
	}
	if row, err := st.OAuthDeviceCodeByDeviceHash(ctx, cred.Hash(deviceCode)); err != nil || row.UsedAt == nil || row.HumanID != human.ID {
		t.Fatalf("device row after spend: %+v err=%v", row, err)
	}
}

func TestDeviceFlowDeny(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	_, session := mintSession(t, st, ctx, "test|device-deny", "Denier", "d@example.com")
	deviceCode, userCode := deviceFixture(t, r, st, ctx, "cid-device-deny")
	c := newWizClient(t, r, session)
	csrf := scrapeCSRF(t, c.get(oauthsrv.DeviceConsentPath+"?user_code="+url.QueryEscape(userCode)).Body.String())
	if rec := c.do(http.MethodPost, oauthsrv.DeviceConsentPath, url.Values{
		"csrf_token": {csrf}, "user_code": {userCode}, "decision": {"deny"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("deny POST: %d", rec.Code)
	}
	if code, doc := devicePoll(t, r, deviceCode, "cid-device-deny"); code != http.StatusBadRequest || doc["error"] != "access_denied" {
		t.Fatalf("after deny: %d %v, want access_denied", code, doc)
	}
	// A denial is final: the row stays as the consent record, and no later approval can revive it.
	if row, err := st.OAuthDeviceCodeByDeviceHash(ctx, cred.Hash(deviceCode)); err != nil || row.DeniedAt == nil {
		t.Fatalf("denied device row: %+v err=%v", row, err)
	}
}

// The entry page cannot be used to grind the short code. Every lookup is charged to the signed-in
// principal, so the misses a per-request counter would never see still trip the limit, and an
// unknown code answers exactly like a wrong one right up to the moment the window closes.
func TestDeviceEntryRateLimited(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	human, session := mintSession(t, st, ctx, "test|device-rl", "Guesser", "g@example.com")
	_, userCode := deviceFixture(t, r, st, ctx, "cid-device-rl")

	if _, err := st.OAuthDeviceCodeByUserCode(ctx, cred.Hash(userCode), human.ID); err != nil {
		t.Fatalf("first entry lookup: %v", err)
	}
	var lastErr error
	for i := 1; i < store.DeviceEntryMaxGuesses; i++ {
		if _, err := st.OAuthDeviceCodeByUserCode(ctx, cred.Hash("VKTRJWPF"), human.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("wrong guess %d: %v, want the uniform not-found answer", i+1, err)
		}
	}
	_, lastErr = st.OAuthDeviceCodeByUserCode(ctx, cred.Hash("XXXXBBBB"), human.ID)
	if !errors.Is(lastErr, store.ErrConflict) {
		t.Fatalf("after %d lookups: %v, want the entry rate limit", store.DeviceEntryMaxGuesses+1, lastErr)
	}
	// The honest code is locked out along with the guesses — the limiter charges the actor, not the
	// guess — and the entry page says so instead of showing a consent screen.
	c := newWizClient(t, r, session)
	page := c.get(oauthsrv.DeviceConsentPath + "?user_code=" + url.QueryEscape(userCode))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "too many attempts") {
		t.Fatalf("rate-limited entry page: %d %.300s", page.Code, page.Body.String())
	}
}

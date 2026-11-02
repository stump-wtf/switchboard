package oauthsrv

// Handler-level conformance for the RFC 8628 device surface (issue #308): the device_authorization
// document, the resource/client gates, and the token endpoint's device-code grant state machine —
// authorization_pending, slow_down, expired_token, access_denied, single issuance bound to the
// approving human, and rejection of a second spend. In-memory fake with the store's semantics;
// the Postgres truth lives in internal/store and the end-to-end walk in internal/server.
// Governing: ADR-0019, ADR-0023, ADR-0057 (proposed).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/cred"
	"github.com/stump-wtf/switchboard/internal/store"
)

type fakeDevice struct {
	id          string
	clientID    string
	humanID     string
	expiresAt   time.Time
	authorized  *time.Time
	denied      *time.Time
	used        *time.Time
	lastPolled  *time.Time
	intervalSec int
}

type fakeDeviceStore struct {
	fakeClientStore
	devices  map[string]*fakeDevice // by device hash
	byUser   map[string]string      // user hash → device hash
	issued   []store.OAuthToken
	issueErr error
	// collideFirst answers the FIRST CreateOAuthDeviceCode with ErrConflict, as a hash collision
	// on a unique index does, so the re-mint path is exercised; createErr then replaces every later
	// attempt, which is how a broken database looks.
	collideFirst bool
	createErr    error
	createCalls  int
}

func (f *fakeDeviceStore) CreateOAuthDeviceCode(_ context.Context, deviceHash, userHash, clientID string, expiresAt time.Time, intervalSeconds int) error {
	f.createCalls++
	if f.createCalls == 1 && f.collideFirst {
		return store.ErrConflict
	}
	if f.createErr != nil {
		return f.createErr
	}
	if _, dup := f.devices[deviceHash]; dup {
		return store.ErrConflict
	}
	f.devices[deviceHash] = &fakeDevice{id: "row-" + deviceHash, clientID: clientID,
		expiresAt: expiresAt, intervalSec: intervalSeconds}
	f.byUser[userHash] = deviceHash
	return nil
}

func (f *fakeDeviceStore) OAuthDeviceCodeByDeviceHash(_ context.Context, deviceHash string) (store.OAuthDeviceCode, error) {
	d, ok := f.devices[deviceHash]
	if !ok {
		return store.OAuthDeviceCode{}, store.ErrNotFound
	}
	out := store.OAuthDeviceCode{ID: d.id, ClientID: d.clientID, HumanID: d.humanID,
		ExpiresAt: d.expiresAt, AuthorizedAt: d.authorized, DeniedAt: d.denied,
		UsedAt: d.used, LastPolledAt: d.lastPolled, IntervalSeconds: d.intervalSec}
	now := time.Now()
	d.lastPolled = &now
	return out, nil
}

func (f *fakeDeviceStore) SpendOAuthDeviceCode(_ context.Context, id string) error {
	for _, d := range f.devices {
		if d.id == id {
			if d.used != nil {
				return store.ErrConflict
			}
			now := time.Now()
			d.used = &now
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeDeviceStore) CreateOAuthToken(_ context.Context, accessHash, refreshHash, clientID, endpointID, humanID string, desiredExpiry time.Time) (store.OAuthToken, error) {
	if f.issueErr != nil {
		return store.OAuthToken{}, f.issueErr
	}
	tok := store.OAuthToken{ID: "tok-" + accessHash, ClientID: clientID, EndpointID: endpointID,
		HumanID: humanID, ExpiresAt: desiredExpiry}
	f.issued = append(f.issued, tok)
	return tok, nil
}

func newDeviceFixture(t *testing.T) (*Handler, *fakeDeviceStore, string, string) {
	t.Helper()
	fs := &fakeDeviceStore{
		fakeClientStore: fakeClientStore{clients: []store.OAuthClient{{
			ID: "c1", ClientID: "cid-dev", Name: "switchboard CLI",
			RedirectURIs: []string{"http://127.0.0.1:9/callback"},
		}}},
		devices: map[string]*fakeDevice{}, byUser: map[string]string{},
	}
	h := New(fs, testBase, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := postForm(t, h, DeviceAuthorizationPath, url.Values{
		"client_id": {"cid-dev"}, "resource": {testBase + OperatorResourcePath},
	})
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("device_authorization answered non-JSON (%d): %s", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("device_authorization: %d %s", rec.Code, rec.Body.String())
	}
	return h, fs, doc["device_code"].(string), doc["user_code"].(string)
}

func postForm(t *testing.T, h *Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	if path == TokenPath {
		h.Token(rec, req)
	} else {
		h.DeviceAuthorization(rec, req)
	}
	return rec
}

func pollForm(deviceCode string) url.Values {
	return url.Values{
		"grant_type":  {DeviceGrantType},
		"device_code": {deviceCode},
		"client_id":   {"cid-dev"},
	}
}

func pollDevice(t *testing.T, h *Handler, deviceCode string) (map[string]any, int) {
	t.Helper()
	rec := postForm(t, h, TokenPath, pollForm(deviceCode))
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("poll answered non-JSON (%d): %s", rec.Code, rec.Body.String())
	}
	return doc, rec.Code
}

func TestDeviceAuthorizationDocument(t *testing.T) {
	_, fs, deviceCode, userCode := newDeviceFixture(t)
	if len(fs.devices) != 1 || len(fs.byUser) != 1 {
		t.Fatalf("request not persisted: %+v", fs.devices)
	}
	d := fs.devices[cred.Hash(deviceCode)]
	if d == nil {
		t.Fatal("device row not stored under the code's hash")
	}
	if d.clientID != "cid-dev" || d.humanID != "" {
		t.Fatalf("pending request must name only the client: %+v", d)
	}
	if len(userCode) != userCodeLen {
		t.Fatalf("user code %q is not %d characters", userCode, userCodeLen)
	}
	for _, r := range userCode {
		if !strings.ContainsRune(string(userCodeAlphabet), r) {
			t.Fatalf("user code %q carries unsafe character %q", userCode, r)
		}
	}
	if _, ok := fs.byUser[cred.Hash(userCode)]; !ok {
		t.Fatal("user code not stored hashed")
	}
	if time.Until(d.expiresAt) <= 0 || time.Until(d.expiresAt) > DeviceCodeTTL+time.Minute {
		t.Fatalf("expiry %v off the advertised TTL", d.expiresAt)
	}
}

func TestDeviceAuthorizationCollision(t *testing.T) {
	// build returns a store whose first insert collides, plus the handler to aim at it.
	build := func(after error) (*Handler, *fakeDeviceStore) {
		fs := &fakeDeviceStore{
			fakeClientStore: fakeClientStore{clients: []store.OAuthClient{{
				ID: "c1", ClientID: "cid-dev", Name: "switchboard CLI",
				RedirectURIs: []string{"http://127.0.0.1:9/callback"},
			}}},
			devices: map[string]*fakeDevice{}, byUser: map[string]string{},
			collideFirst: true, createErr: after,
		}
		return New(fs, testBase, slog.New(slog.NewTextHandler(io.Discard, nil))), fs
	}
	form := url.Values{"client_id": {"cid-dev"}, "resource": {testBase + OperatorResourcePath}}

	t.Run("a collision is re-minted, never adopted", func(t *testing.T) {
		h, fs := build(nil)
		rec := postForm(t, h, DeviceAuthorizationPath, form)
		if rec.Code != http.StatusOK {
			t.Fatalf("collision response: %d %s", rec.Code, rec.Body.String())
		}
		if fs.createCalls != 2 || len(fs.devices) != 1 {
			t.Fatalf("collideFirst: %d create calls, %d rows; want one re-mint that lands once", fs.createCalls, len(fs.devices))
		}
	})

	// The re-mint's error must reach the response: handing a client codes that were never stored
	// is worse than an error, because its polling would then be waiting on nothing.
	t.Run("a request that never landed never answers 200", func(t *testing.T) {
		h, fs := build(errors.New("insert failed"))
		rec := postForm(t, h, DeviceAuthorizationPath, form)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("second insert failure: %d %s, want 500", rec.Code, rec.Body.String())
		}
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("error document: %v", err)
		}
		if doc["error"] != "server_error" || len(fs.devices) != 0 {
			t.Fatalf("error %v with %d rows stored", doc["error"], len(fs.devices))
		}
	})
}

func TestDeviceAuthorizationGates(t *testing.T) {
	h, _ := newTestHandler()
	for _, tc := range []struct {
		name    string
		form    url.Values
		wantErr string
	}{
		{"unknown client", url.Values{"client_id": {"nobody"}}, "invalid_client"},
		{"no client", url.Values{}, "invalid_request"},
		{"foreign resource", url.Values{"client_id": {"cid-dev"}, "resource": {"https://evil.example/api"}}, "invalid_target"},
		{"mcp resource", url.Values{"client_id": {"cid-dev"}, "resource": {testBase + "/mcp/slug"}}, "invalid_target"},
	} {
		rec := postForm(t, h, DeviceAuthorizationPath, tc.form)
		var doc map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("%s: non-JSON (%d): %s", tc.name, rec.Code, rec.Body.String())
		}
		if rec.Code != http.StatusBadRequest || doc["error"] != tc.wantErr {
			t.Fatalf("%s: got %d %v, want 400 %s", tc.name, rec.Code, doc, tc.wantErr)
		}
	}
}

func TestDevicePollStateMachine(t *testing.T) {
	h, fs, deviceCode, _ := newDeviceFixture(t)
	hash := cred.Hash(deviceCode)
	d := fs.devices[hash]

	if doc, code := pollDevice(t, h, deviceCode); code != http.StatusBadRequest || doc["error"] != "authorization_pending" {
		t.Fatalf("first poll: %d %v, want authorization_pending", code, doc)
	}
	// A poll inside the interval (minus the skew margin) is slow_down, whatever the state.
	if doc, code := pollDevice(t, h, deviceCode); code != http.StatusBadRequest || doc["error"] != "slow_down" {
		t.Fatalf("fast second poll: %d %v, want slow_down", code, doc)
	}
	d.lastPolled = nil

	// The human approves (used_at stays null until a poll mints).
	now := time.Now()
	d.authorized, d.humanID = &now, "human-7"
	doc, code := pollDevice(t, h, deviceCode)
	if code != http.StatusOK || doc["access_token"] == "" || doc["refresh_token"] == "" || doc["token_type"] != "Bearer" {
		t.Fatalf("approved poll: %d %v, want a token pair", code, doc)
	}
	if len(fs.issued) != 1 || fs.issued[0].HumanID != "human-7" || fs.issued[0].EndpointID != "" {
		t.Fatalf("grant not bound to the approving human alone: %+v", fs.issued)
	}

	// A second spend is refused: the flow is single-use like the code flow.
	d.lastPolled = nil
	if doc, code := pollDevice(t, h, deviceCode); code != http.StatusBadRequest || doc["error"] != "invalid_grant" {
		t.Fatalf("second spend: %d %v, want invalid_grant", code, doc)
	}
}

func TestDevicePollTerminalStates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(d *fakeDevice)
		wantErr string
	}{
		{"expired", func(d *fakeDevice) { d.expiresAt = time.Now().Add(-time.Minute) }, "expired_token"},
		{"denied", func(d *fakeDevice) { n := time.Now(); d.denied = &n }, "access_denied"},
	} {
		h, fs, deviceCode, _ := newDeviceFixture(t)
		tc.mutate(fs.devices[cred.Hash(deviceCode)])
		if doc, code := pollDevice(t, h, deviceCode); code != http.StatusBadRequest || doc["error"] != tc.wantErr {
			t.Fatalf("%s: %d %v, want %s", tc.name, code, doc, tc.wantErr)
		}
	}
}

func TestDevicePollUnknownCodeReadsExpired(t *testing.T) {
	h, _, _, _ := newDeviceFixture(t)
	if doc, code := pollDevice(t, h, "never-minted"); code != http.StatusBadRequest || doc["error"] != "expired_token" {
		t.Fatalf("unknown device code: %d %v, want expired_token", code, doc)
	}
}

func TestDevicePollClientMustMatch(t *testing.T) {
	h, fs, deviceCode, _ := newDeviceFixture(t)
	d := fs.devices[cred.Hash(deviceCode)]
	now := time.Now()
	d.authorized, d.humanID = &now, "human-7"
	form := pollForm(deviceCode)
	form.Set("client_id", "someone-else")
	rec := postForm(t, h, TokenPath, form)
	var doc map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	if rec.Code != http.StatusBadRequest || doc["error"] != "invalid_grant" {
		t.Fatalf("wrong client: %d %s, want invalid_grant", rec.Code, rec.Body.String())
	}
	if len(fs.issued) != 0 {
		t.Fatalf("a mismatched client minted tokens: %+v", fs.issued)
	}
}

func TestASMetadataAdvertisesDeviceFlow(t *testing.T) {
	h, _ := newTestHandler()
	w := httptest.NewRecorder()
	h.ASMetadata(w, httptest.NewRequest(http.MethodGet, ASMetadataPath, nil))
	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("metadata not JSON: %s", w.Body.String())
	}
	if got := doc["device_authorization_endpoint"]; got != testBase+DeviceAuthorizationPath {
		t.Fatalf("device_authorization_endpoint = %v", got)
	}
	var found bool
	for _, g := range doc["grant_types_supported"].([]any) {
		if g == DeviceGrantType {
			found = true
		}
	}
	if !found {
		t.Fatalf("grant_types_supported lacks the device grant: %v", doc["grant_types_supported"])
	}
}

func TestRegistrationAcceptsDeviceGrantType(t *testing.T) {
	fs := &fakeClientStore{}
	h := New(fs, testBase, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body, err := json.Marshal(map[string]any{
		"client_name":   "harness installer",
		"redirect_uris": []string{"http://127.0.0.1:4000/callback"},
		"grant_types":   []string{DeviceGrantType},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, RegisterPath, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Register(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register with the device grant: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDeviceCodeHelpers(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		_, userCode, _, _, err := DeviceCodes()
		if err != nil {
			t.Fatal(err)
		}
		if len(userCode) != userCodeLen {
			t.Fatalf("user code %q", userCode)
		}
		seen[userCode] = true
	}
	if len(seen) < 45 {
		t.Fatalf("user codes colliding too often: %d unique of 50", len(seen))
	}
	if got := NormalizeUserCode(" vktr-jwpf "); got != "VKTRJWPF" {
		t.Fatalf("normalize: %q", got)
	}
	if got := UserCodeDisplay("VKTRJWPF"); got != "VKTR-JWPF" {
		t.Fatalf("display: %q", got)
	}
}

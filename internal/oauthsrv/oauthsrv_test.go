// Conformance tests for the OAuth AS discovery + registration surface (ADR-0019, SPEC-0016):
// RFC 8414 AS metadata, RFC 9728 protected-resource metadata, RFC 7591 dynamic client
// registration with exact redirect-URI validation, and the exact-match rule the authorize
// endpoint will enforce (redirect mismatch cases are part of the definition of done — design.md
// "Security notes").
package oauthsrv

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/store"
)

const testBase = "https://sb.example.com"

// fakeClientStore records registrations in memory; failErr forces the persist-failure path. The
// embedded unusedTokenStore satisfies the Store interface for tests that never touch the token
// endpoint (token-endpoint conformance uses its own functional fake in token_test.go).
type fakeClientStore struct {
	unusedTokenStore
	clients []store.OAuthClient
	failErr error
}

func (f *fakeClientStore) CreateOAuthClient(_ context.Context, clientID, name string, redirectURIs []string) (store.OAuthClient, error) {
	if f.failErr != nil {
		return store.OAuthClient{}, f.failErr
	}
	c := store.OAuthClient{ID: "row-" + clientID, ClientID: clientID, Name: name, RedirectURIs: redirectURIs}
	f.clients = append(f.clients, c)
	return c, nil
}

// unusedTokenStore panics on any use: registration/metadata tests must never reach token storage.
type unusedTokenStore struct{}

func (unusedTokenStore) RedeemOAuthCode(context.Context, string) (store.OAuthCode, error) {
	panic("token store used in a registration test")
}

func (unusedTokenStore) CreateOAuthToken(context.Context, string, string, string, string, time.Time) (store.OAuthToken, error) {
	panic("token store used in a registration test")
}

func (unusedTokenStore) RotateOAuthToken(context.Context, string, string, string, string, time.Time) (store.OAuthToken, error) {
	panic("token store used in a registration test")
}

func newTestHandler() (*Handler, *fakeClientStore) {
	fs := &fakeClientStore{}
	return New(fs, testBase, slog.New(slog.NewTextHandler(io.Discard, nil))), fs
}

// router mounts the handler exactly as internal/server does, so URL params resolve the same way.
func router(h *Handler) chi.Router {
	r := chi.NewRouter()
	r.Get(ASMetadataPath, h.ASMetadata)
	r.Get(ProtectedResourcePrefix+"/mcp/{endpoint}", h.ProtectedResourceMetadata)
	r.Post(RegisterPath, h.Register)
	return r
}

func getJSON(t *testing.T, r chi.Router, path string, wantStatus int) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != wantStatus {
		t.Fatalf("GET %s: got %d, want %d (body: %.300s)", path, rec.Code, wantStatus, rec.Body.String())
	}
	if wantStatus != http.StatusOK {
		return nil
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("GET %s: Content-Type = %q, want application/json", path, ct)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("GET %s: invalid JSON: %v", path, err)
	}
	return doc
}

// TestASMetadata: the RFC 8414 document advertises the full OAuth 2.1 profile ADR-0019 commits
// to, with every URL derived from the deployed base (SPEC-0016 REQ "Authorization Server
// Metadata" scenario "Metadata round-trip").
func TestASMetadata(t *testing.T) {
	h, _ := newTestHandler()
	doc := getJSON(t, router(h), ASMetadataPath, http.StatusOK)

	wantStrings := map[string]string{
		"issuer":                 testBase,
		"authorization_endpoint": testBase + "/oauth/authorize",
		"token_endpoint":         testBase + "/oauth/token",
		"registration_endpoint":  testBase + "/oauth/register",
	}
	for k, want := range wantStrings {
		if got, _ := doc[k].(string); got != want {
			t.Errorf("%s = %v, want %q", k, doc[k], want)
		}
	}
	wantLists := map[string][]string{
		"response_types_supported":              {"code"},
		"grant_types_supported":                 {"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      {"S256"},
		"token_endpoint_auth_methods_supported": {"none"},
	}
	for k, want := range wantLists {
		got, ok := doc[k].([]any)
		if !ok || len(got) != len(want) {
			t.Errorf("%s = %v, want %v", k, doc[k], want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s[%d] = %v, want %q", k, i, got[i], want[i])
			}
		}
	}
	// Every advertised URL must live under the issuer — the deployment-consistency half of the
	// requirement (a URL on another origin could never resolve on this deployment).
	for _, k := range []string{"authorization_endpoint", "token_endpoint", "registration_endpoint"} {
		if u, _ := doc[k].(string); !strings.HasPrefix(u, testBase+"/") {
			t.Errorf("%s = %q, want a URL under %s", k, u, testBase)
		}
	}
}

// TestProtectedResourceMetadata: the RFC 9728 document for a mount identifies the resource and its
// authorization server (SPEC-0016 REQ "Protected Resource Metadata"), and junk slugs 404 rather
// than being reflected.
func TestProtectedResourceMetadata(t *testing.T) {
	h, _ := newTestHandler()
	r := router(h)

	doc := getJSON(t, r, ProtectedResourcePrefix+"/mcp/agent-a-11111111", http.StatusOK)
	if got, _ := doc["resource"].(string); got != testBase+"/mcp/agent-a-11111111" {
		t.Errorf("resource = %v, want %q", doc["resource"], testBase+"/mcp/agent-a-11111111")
	}
	as, ok := doc["authorization_servers"].([]any)
	if !ok || len(as) != 1 || as[0] != testBase {
		t.Errorf("authorization_servers = %v, want [%q]", doc["authorization_servers"], testBase)
	}

	// The URL the 401 challenge advertises must be exactly the URL this handler serves — the two
	// derivations share ResourceMetadataURL, asserted here so they can never drift.
	if got := ResourceMetadataURL(testBase, "agent-a-11111111"); got != testBase+ProtectedResourcePrefix+"/mcp/agent-a-11111111" {
		t.Errorf("ResourceMetadataURL = %q", got)
	}

	// Non-slug-shaped path input is never reflected into a document: 404.
	for _, bad := range []string{"UPPER", "sp%20ace", "semi;colon", "under_score"} {
		getJSON(t, r, ProtectedResourcePrefix+"/mcp/"+bad, http.StatusNotFound)
	}
}

// TestRegister covers the RFC 7591 registration matrix: the ADR-0019 public-client profile is
// accepted, and every malformed or open-redirect-shaped registration is rejected with the RFC
// error document (SPEC-0016 REQ "Dynamic Client Registration").
func TestRegister(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantErr    string // "error" member of the RFC 7591 error document (400s only)
	}{
		{"https redirect", `{"redirect_uris":["https://client.example.com/callback"],"client_name":"Claude Desktop"}`,
			http.StatusCreated, ""},
		{"loopback http redirect", `{"redirect_uris":["http://127.0.0.1:33418/callback"]}`,
			http.StatusCreated, ""},
		{"localhost http redirect", `{"redirect_uris":["http://localhost:8976/oauth/cb"]}`,
			http.StatusCreated, ""},
		{"private-use scheme", `{"redirect_uris":["com.example.app:/oauth2redirect"]}`,
			http.StatusCreated, ""},
		{"full profile echo", `{"redirect_uris":["https://c.example.com/cb"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"],"token_endpoint_auth_method":"none"}`,
			http.StatusCreated, ""},

		{"not json", `redirect_uris=https://c.example.com/cb`, http.StatusBadRequest, "invalid_client_metadata"},
		{"missing redirect_uris", `{"client_name":"nope"}`, http.StatusBadRequest, "invalid_redirect_uri"},
		{"empty redirect_uris", `{"redirect_uris":[]}`, http.StatusBadRequest, "invalid_redirect_uri"},
		{"relative redirect", `{"redirect_uris":["/callback"]}`, http.StatusBadRequest, "invalid_redirect_uri"},
		{"fragment redirect", `{"redirect_uris":["https://c.example.com/cb#frag"]}`,
			http.StatusBadRequest, "invalid_redirect_uri"},
		{"non-loopback http", `{"redirect_uris":["http://evil.example.com/cb"]}`,
			http.StatusBadRequest, "invalid_redirect_uri"},
		{"wildcard host", `{"redirect_uris":["https://*.example.com/cb"]}`,
			http.StatusBadRequest, "invalid_redirect_uri"},
		{"one bad among good", `{"redirect_uris":["https://c.example.com/cb","http://evil.example.com/x"]}`,
			http.StatusBadRequest, "invalid_redirect_uri"},
		{"too many redirects", `{"redirect_uris":[` + strings.Repeat(`"https://c.example.com/cb",`, 10) + `"https://c.example.com/z"]}`,
			http.StatusBadRequest, "invalid_redirect_uri"},
		{"confidential client", `{"redirect_uris":["https://c.example.com/cb"],"token_endpoint_auth_method":"client_secret_basic"}`,
			http.StatusBadRequest, "invalid_client_metadata"},
		{"foreign grant type", `{"redirect_uris":["https://c.example.com/cb"],"grant_types":["client_credentials"]}`,
			http.StatusBadRequest, "invalid_client_metadata"},
		{"foreign response type", `{"redirect_uris":["https://c.example.com/cb"],"response_types":["token"]}`,
			http.StatusBadRequest, "invalid_client_metadata"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, fs := newTestHandler()
			rec := httptest.NewRecorder()
			router(h).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RegisterPath, strings.NewReader(tt.body)))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %.300s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			var doc map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
				t.Fatalf("response is not JSON: %v", err)
			}
			if tt.wantStatus == http.StatusBadRequest {
				if got, _ := doc["error"].(string); got != tt.wantErr {
					t.Errorf("error = %v, want %q", doc["error"], tt.wantErr)
				}
				if len(fs.clients) != 0 {
					t.Errorf("rejected registration must persist nothing; stored %d clients", len(fs.clients))
				}
				return
			}
			// Accepted: a usable client_id came back and matches what was persisted.
			cid, _ := doc["client_id"].(string)
			if cid == "" {
				t.Fatal("registration succeeded without a client_id")
			}
			if len(fs.clients) != 1 || fs.clients[0].ClientID != cid {
				t.Fatalf("persisted clients = %+v, want exactly one with client_id %q", fs.clients, cid)
			}
			if m, _ := doc["token_endpoint_auth_method"].(string); m != "none" {
				t.Errorf("token_endpoint_auth_method = %v, want none (public clients only)", doc["token_endpoint_auth_method"])
			}
			if _, hasSecret := doc["client_secret"]; hasSecret {
				t.Error("public clients must never be issued a client_secret")
			}
		})
	}
}

// TestRegisterPersistFailure: a store failure answers 500 without leaking internals.
func TestRegisterPersistFailure(t *testing.T) {
	fs := &fakeClientStore{failErr: errors.New("pg down")}
	h := New(fs, testBase, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	router(h).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RegisterPath,
		strings.NewReader(`{"redirect_uris":["https://c.example.com/cb"]}`)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "pg down") {
		t.Fatal("internal error detail leaked to the client")
	}
}

// TestClientIDsUnique: minted client ids are high-entropy — two registrations never collide.
func TestClientIDsUnique(t *testing.T) {
	h, fs := newTestHandler()
	r := router(h)
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RegisterPath,
			strings.NewReader(`{"redirect_uris":["https://c.example.com/cb"]}`)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("registration %d: status %d", i, rec.Code)
		}
	}
	if fs.clients[0].ClientID == fs.clients[1].ClientID {
		t.Fatalf("two registrations minted the same client_id %q", fs.clients[0].ClientID)
	}
}

// TestRedirectAllowed is the authorize-time exact-match contract (SPEC-0016 scenario "any
// authorize request with a non-matching redirect URI is rejected"): byte-for-byte equality against
// the registered list, mismatches of every common near-miss shape refused.
func TestRedirectAllowed(t *testing.T) {
	client := store.OAuthClient{
		ClientID: "abc123",
		RedirectURIs: []string{
			"https://client.example.com/callback",
			"http://127.0.0.1:33418/cb",
		},
	}
	tests := []struct {
		name string
		uri  string
		want bool
	}{
		{"exact match first", "https://client.example.com/callback", true},
		{"exact match second", "http://127.0.0.1:33418/cb", true},

		{"empty", "", false},
		{"trailing slash", "https://client.example.com/callback/", false},
		{"prefix only", "https://client.example.com/call", false},
		{"suffix extended", "https://client.example.com/callback/extra", false},
		{"scheme swapped", "http://client.example.com/callback", false},
		{"host case", "https://CLIENT.example.com/callback", false},
		{"path case", "https://client.example.com/Callback", false},
		{"different port", "http://127.0.0.1:33419/cb", false},
		{"added query", "https://client.example.com/callback?next=x", false},
		{"added fragment", "https://client.example.com/callback#x", false},
		{"subdomain attack", "https://client.example.com.evil.example/callback", false},
		{"userinfo smuggling", "https://client.example.com@evil.example/callback", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedirectAllowed(client, tt.uri); got != tt.want {
				t.Errorf("RedirectAllowed(%q) = %v, want %v", tt.uri, got, tt.want)
			}
		})
	}
}

// TestValidateRedirectURI pins the registration-time shape rules independently of the HTTP
// surface (they also gate the authorize story's re-validation of stored rows).
func TestValidateRedirectURI(t *testing.T) {
	tests := []struct {
		name   string
		uri    string
		wantOK bool
	}{
		{"https", "https://client.example.com/cb", true},
		{"https with port", "https://client.example.com:8443/cb", true},
		{"loopback ipv4", "http://127.0.0.1:9999/cb", true},
		{"loopback name", "http://localhost/cb", true},
		{"loopback ipv6", "http://[::1]:7777/cb", true},
		{"private-use scheme", "com.example.app:/oauth2redirect", true},

		{"empty", "", false},
		{"relative", "/cb", false},
		{"fragment", "https://client.example.com/cb#top", false},
		{"http non-loopback", "http://client.example.com/cb", false},
		{"http lan address", "http://192.168.1.10/cb", false},
		{"wildcard host", "https://*.example.com/cb", false},
		{"https without host", "https:///cb", false},
		{"overlong", "https://client.example.com/" + strings.Repeat("a", maxRedirectLen), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRedirectURI(tt.uri)
			if (err == nil) != tt.wantOK {
				t.Errorf("ValidateRedirectURI(%q) = %v, want ok=%v", tt.uri, err, tt.wantOK)
			}
		})
	}
}

// TestSlugOK pins the reflected-input gate shared by the metadata handler and the 401 challenge.
func TestSlugOK(t *testing.T) {
	for slug, want := range map[string]bool{
		"agent-a-11111111": true,
		"a":                true,
		"queue2-bot":       true,
		"":                 false,
		"UPPER":            false,
		"has space":        false,
		"semi;colon":       false,
		`quote"mark`:       false,
		"new\nline":        false,
		"under_score":      false,
	} {
		if got := SlugOK(slug); got != want {
			t.Errorf("SlugOK(%q) = %v, want %v", slug, got, want)
		}
	}
}

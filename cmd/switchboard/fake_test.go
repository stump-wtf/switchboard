package main

// A fake switchboard deployment for the CLI tests: the RFC 9728 / 8414 discovery documents, RFC
// 7591 registration, an authorize endpoint that redirects straight back with a code (the human
// said yes), a token endpoint that verifies PKCE and rotates refresh tokens, and the three
// /api/v1 verbs behind a bearer check. It records what the CLI sent so tests can assert the
// wire shape (resource = <base>/api, S256, exact redirect URI) rather than only the outcome.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type fakeDeployment struct {
	t   *testing.T
	srv *httptest.Server

	mu           sync.Mutex
	deny         bool                // authorize answers access_denied
	registered   []map[string]any    // registration bodies seen
	authorizes   []url.Values        // authorize queries seen
	codes        map[string]string   // code → PKCE challenge
	tokenCalls   []url.Values        // token-endpoint forms seen
	access       string              // the currently valid access token
	refresh      string              // the currently valid refresh token
	expiresIn    int64               // expires_in advertised on issuance
	generation   int                 // bumps on every issuance
	revoked      map[string]bool     // access tokens the API rejects with 401
	vends        []map[string]string // POST /api/v1/endpoints bodies seen
	pushes       []map[string]any    // POST /api/v1/endpoints/{ref}/todos bodies seen, "ref" added
	pushExists   bool                // the push answers created:false, as a matched key does
	revokes      []string            // endpoint refs seen on POST /api/v1/endpoints/{ref}/revoke
	revokeStatus int                 // when non-zero, the status the revoke route answers with
	endpointRows []map[string]any    // GET /api/v1/endpoints answer
	agentRows    []map[string]any    // GET /api/v1/agents answer
}

func newFakeDeployment(t *testing.T) *fakeDeployment {
	t.Helper()
	f := &fakeDeployment{t: t, codes: map[string]string{}, revoked: map[string]bool{}, expiresIn: 3600}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDeployment) base() string { return f.srv.URL }

// issue mints the next access/refresh pair.
func (f *fakeDeployment) issue() map[string]any {
	f.generation++
	f.access = fmt.Sprintf("at-%d", f.generation)
	f.refresh = fmt.Sprintf("rt-%d", f.generation)
	return map[string]any{
		"access_token": f.access, "refresh_token": f.refresh,
		"expires_in": f.expiresIn, "token_type": "Bearer",
	}
}

func (f *fakeDeployment) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	base := f.srv.URL
	switch {
	case r.URL.Path == "/.well-known/oauth-protected-resource/api":
		writeJSONResponse(w, 200, map[string]any{"resource": base + "/api", "authorization_servers": []string{base}})
	case r.URL.Path == "/.well-known/oauth-authorization-server":
		writeJSONResponse(w, 200, map[string]any{
			"issuer":                 base,
			"authorization_endpoint": base + "/oauth/authorize",
			"token_endpoint":         base + "/oauth/token",
			"registration_endpoint":  base + "/oauth/register",
		})
	case r.URL.Path == "/oauth/register" && r.Method == http.MethodPost:
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONResponse(w, 400, map[string]any{"error": "invalid_client_metadata"})
			return
		}
		f.registered = append(f.registered, body)
		writeJSONResponse(w, 201, map[string]any{"client_id": "cid-test", "redirect_uris": body["redirect_uris"]})
	case r.URL.Path == "/oauth/authorize":
		q := r.URL.Query()
		f.authorizes = append(f.authorizes, q)
		redirect, err := url.Parse(q.Get("redirect_uri"))
		if err != nil || q.Get("client_id") != "cid-test" || q.Get("code_challenge_method") != "S256" ||
			q.Get("resource") != base+"/api" || q.Get("code_challenge") == "" {
			http.Error(w, "bad authorize request: "+q.Encode(), 400)
			return
		}
		out := url.Values{"state": {q.Get("state")}}
		if f.deny {
			out.Set("error", "access_denied")
			out.Set("error_description", "the operator denied the request")
		} else {
			code := fmt.Sprintf("code-%d", len(f.codes)+1)
			f.codes[code] = q.Get("code_challenge")
			out.Set("code", code)
		}
		redirect.RawQuery = out.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusFound)
	case r.URL.Path == "/oauth/token" && r.Method == http.MethodPost:
		if err := r.ParseForm(); err != nil {
			writeJSONResponse(w, 400, map[string]any{"error": "invalid_request"})
			return
		}
		f.tokenCalls = append(f.tokenCalls, r.PostForm)
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			challenge, ok := f.codes[r.PostForm.Get("code")]
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if !ok || r.PostForm.Get("client_id") != "cid-test" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				writeJSONResponse(w, 400, map[string]any{"error": "invalid_grant", "error_description": "code or verifier rejected"})
				return
			}
			delete(f.codes, r.PostForm.Get("code"))
			writeJSONResponse(w, 200, f.issue())
		case "refresh_token":
			if r.PostForm.Get("refresh_token") != f.refresh || r.PostForm.Get("client_id") != "cid-test" {
				writeJSONResponse(w, 400, map[string]any{"error": "invalid_grant", "error_description": "the grant is no longer valid"})
				return
			}
			writeJSONResponse(w, 200, f.issue())
		default:
			writeJSONResponse(w, 400, map[string]any{"error": "unsupported_grant_type"})
		}
	case strings.HasPrefix(r.URL.Path, "/api/v1/"):
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if bearer == "" || bearer != f.access || f.revoked[bearer] {
			w.Header().Set("WWW-Authenticate", `Bearer realm="switchboard-api"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/endpoints":
			var in map[string]string
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, "invalid JSON body", 400)
				return
			}
			if in["name"] == "" {
				http.Error(w, "name is required", 400)
				return
			}
			f.vends = append(f.vends, in)
			slug := in["name"] + "-ab12cd34"
			writeJSONResponse(w, 201, map[string]any{
				"agent_name": in["name"], "slug": slug,
				"mcp_url": base + "/mcp/" + slug, "token": "sbk_" + strings.Repeat("x", 40),
				"queue": in["queue"], "verbs": []string{"list_todos", "claim", "complete"},
				"webhook": map[string]any{"webhook_id": "wh-1", "ingest_url": base + "/webhooks/w/deadbeef", "trust_mode": "token"},
				"mcp_json": map[string]any{"mcpServers": map[string]any{"switchboard": map[string]any{
					"type": "http", "url": base + "/mcp/" + slug,
					"headers": map[string]string{"Authorization": "Bearer sbk_" + strings.Repeat("x", 40)},
				}}},
				"expires_at": nil,
			})
		case "GET /api/v1/endpoints":
			rows := f.endpointRows
			if rows == nil {
				rows = []map[string]any{}
			}
			writeJSONResponse(w, 200, rows)
		case "GET /api/v1/agents":
			rows := f.agentRows
			if rows == nil {
				rows = []map[string]any{}
			}
			writeJSONResponse(w, 200, rows)
		default:
			// POST /api/v1/endpoints/{ref}/todos
			if rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/endpoints/"); ok && r.Method == http.MethodPost {
				if ref, ok := strings.CutSuffix(rest, "/todos"); ok && ref != "" {
					var in map[string]any
					if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
						http.Error(w, "invalid JSON body", 400)
						return
					}
					if in["title"] == "" {
						http.Error(w, "title is required", 400)
						return
					}
					in["ref"] = ref
					f.pushes = append(f.pushes, in)
					queue, _ := in["queue"].(string)
					if queue == "" {
						queue = "inbox"
					}
					writeJSONResponse(w, 201, map[string]any{
						"id": "td_1", "endpoint_id": "ep-1", "slug": ref, "queue": queue,
						"state": "pending", "created": !f.pushExists, "event_id": 7})
					return
				}
			}
			// POST /api/v1/endpoints/{ref}/revoke
			ref, isRevoke := strings.CutPrefix(r.URL.Path, "/api/v1/endpoints/")
			ref, alsoRevoke := strings.CutSuffix(ref, "/revoke")
			if r.Method == http.MethodPost && isRevoke && alsoRevoke && ref != "" {
				f.revokes = append(f.revokes, ref)
				if f.revokeStatus != 0 {
					writeJSONResponse(w, f.revokeStatus, map[string]any{"error": "endpoint is already revoked"})
					return
				}
				writeJSONResponse(w, 200, map[string]any{
					"id": "ep-1", "slug": ref, "agent_name": "some-agent", "state": "revoked"})
				return
			}
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

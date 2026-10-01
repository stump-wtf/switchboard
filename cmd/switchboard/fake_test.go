package main

// A fake switchboard deployment for the CLI tests: the RFC 9728 / 8414 discovery documents, RFC
// 7591 registration, an authorize endpoint that redirects straight back with a code (the human
// said yes), a token endpoint that verifies PKCE and rotates refresh tokens, and the /api/v1
// routes the CLI drives, behind a bearer check. It records what the CLI sent so tests can assert
// the wire shape (resource = <base>/api, S256, exact redirect URI) rather than only the outcome.

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
	deny         bool              // authorize answers access_denied
	registered   []map[string]any  // registration bodies seen
	authorizes   []url.Values      // authorize queries seen
	codes        map[string]string // code → PKCE challenge
	tokenCalls   []url.Values      // token-endpoint forms seen
	access       string            // the currently valid access token
	refresh      string            // the currently valid refresh token
	expiresIn    int64             // expires_in advertised on issuance
	generation   int               // bumps on every issuance
	revoked      map[string]bool   // access tokens the API rejects with 401
	vends        []map[string]any  // POST /api/v1/endpoints bodies seen
	edits        []map[string]any  // PATCH /api/v1/endpoints/{ref} bodies seen, "ref" added
	editStatus   int               // when non-zero, the status (and APIError body) the edit route answers with
	pushes       []map[string]any  // POST /api/v1/endpoints/{ref}/todos bodies seen, "ref" added
	pushExists   bool              // the push answers created:false, as a matched key does
	revokes      []string          // endpoint refs seen on POST /api/v1/endpoints/{ref}/revoke
	revokeStatus int               // when non-zero, the status the revoke route answers with
	endpointRows []map[string]any  // GET /api/v1/endpoints answer
	agentRows    []map[string]any  // GET /api/v1/agents answer
	webhookRows  []map[string]any  // GET /api/v1/webhooks answer
	rulesDocs    map[string]any    // GET /api/v1/webhooks/{id}/rules answers, by webhook id
	ruleSets     []map[string]any  // PUT /api/v1/webhooks/{id}/rules bodies seen, "webhook" added
	ruleTests    []map[string]any  // POST /api/v1/webhooks/{id}/rules/test bodies seen, "webhook" added
	testAnswer   map[string]any    // the rules/test answer
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
		if f.serveWebhooks(w, r) {
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/endpoints":
			var in map[string]any
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, "invalid JSON body", 400)
				return
			}
			name, _ := in["name"].(string)
			if name == "" {
				http.Error(w, "name is required", 400)
				return
			}
			f.vends = append(f.vends, in)
			slug := name + "-ab12cd34"
			doc := map[string]any{
				"agent_name": in["name"], "slug": slug,
				"mcp_url": base + "/mcp/" + slug, "token": "sbk_" + strings.Repeat("x", 40),
				"queue": in["queue"], "verbs": []string{"list_todos", "claim", "complete"},
				"webhook": map[string]any{"webhook_id": "wh-1", "ingest_url": base + "/webhooks/w/deadbeef", "trust_mode": "token"},
				"mcp_json": map[string]any{"mcpServers": map[string]any{"switchboard": map[string]any{
					"type": "http", "url": base + "/mcp/" + slug,
					"headers": map[string]string{"Authorization": "Bearer sbk_" + strings.Repeat("x", 40)},
				}}},
				"expires_at": nil,
			}
			// The lease pair is echoed only when the vend set one, so the older reveal tests keep
			// the response shape they were written against.
			if d, ok := in["default_lease_ttl_seconds"].(float64); ok {
				doc["default_lease_ttl_seconds"], doc["effective_lease_ttl_seconds"] = int(d), int(d)
			}
			writeJSONResponse(w, 201, doc)
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
			// PATCH /api/v1/endpoints/{ref}
			if ref, ok := strings.CutPrefix(r.URL.Path, "/api/v1/endpoints/"); ok && r.Method == http.MethodPatch && ref != "" && !strings.Contains(ref, "/") {
				var in map[string]any
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
					http.Error(w, "invalid JSON body", 400)
					return
				}
				in["ref"] = ref
				f.edits = append(f.edits, in)
				if f.editStatus != 0 {
					writeJSONResponse(w, f.editStatus, map[string]any{
						"error": "default_lease_ttl_seconds must be a whole number of seconds from 60 to 86400, or null for the server default (300)",
						"code":  "invalid_argument"})
					return
				}
				out := map[string]any{"id": "ep-1", "slug": ref, "agent_name": "some-agent", "state": "active",
					"default_lease_ttl_seconds": nil, "effective_lease_ttl_seconds": 300}
				if d, ok := in["default_lease_ttl_seconds"].(float64); ok {
					out["default_lease_ttl_seconds"], out["effective_lease_ttl_seconds"] = int(d), int(d)
				}
				writeJSONResponse(w, 200, out)
				return
			}
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

// serveWebhooks answers the human API's webhook routes the way the real handlers do: the rules
// document shape, SPEC-0035's {"error", "code"} errors, and a PUT that keeps params when the body has
// no params key. It reports whether it handled the request.
func (f *fakeDeployment) serveWebhooks(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet && r.URL.Path == "/api/v1/webhooks" {
		rows := f.webhookRows
		if rows == nil {
			rows = []map[string]any{}
		}
		writeJSONResponse(w, 200, map[string]any{"webhooks": rows})
		return true
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/webhooks/")
	if !ok {
		return false
	}
	id, verb, _ := strings.Cut(rest, "/")
	notFound := func() { writeJSONResponse(w, 404, map[string]any{"error": "webhook not found", "code": "not_found"}) }
	decode := func() (map[string]any, bool) {
		var in map[string]any
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSONResponse(w, 400, map[string]any{"error": "invalid JSON body", "code": "invalid_argument"})
			return nil, false
		}
		return in, true
	}
	switch r.Method + " " + verb {
	case "GET rules":
		doc, ok := f.rulesDocs[id]
		if !ok {
			notFound()
			return true
		}
		writeJSONResponse(w, 200, doc)
	case "PUT rules":
		prev, ok := f.rulesDocs[id].(map[string]any)
		if !ok {
			notFound()
			return true
		}
		in, ok := decode()
		if !ok {
			return true
		}
		saved := map[string]any{"webhook_id": id, "source_type": prev["source_type"], "target_queue": prev["target_queue"],
			"rules": in["rules"], "grant": prev["grant"]}
		if d, has := in["default_action"]; has && d != nil {
			saved["default_action"] = d
		}
		params, has := in["params"]
		if !has {
			params = prev["params"]
		}
		if params != nil {
			saved["params"] = params
		}
		f.rulesDocs[id] = saved
		in["webhook"] = id
		f.ruleSets = append(f.ruleSets, in)
		writeJSONResponse(w, 200, saved)
	case "POST rules/test":
		if _, ok := f.rulesDocs[id]; !ok {
			notFound()
			return true
		}
		in, ok := decode()
		if !ok {
			return true
		}
		in["webhook"] = id
		f.ruleTests = append(f.ruleTests, in)
		writeJSONResponse(w, 200, f.testAnswer)
	default:
		http.NotFound(w, r)
	}
	return true
}

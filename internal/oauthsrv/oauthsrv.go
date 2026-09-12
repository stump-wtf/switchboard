// Package oauthsrv is switchboard's OAuth 2.1 authorization-server surface for its own vended MCP
// endpoints (ADR-0019, SPEC-0016): RFC 9728 protected-resource metadata per /mcp/{slug} mount,
// the RFC 8414 authorization-server metadata document, and RFC 7591 dynamic client registration.
// Every OAuth credential this subsystem ever issues is a credential ONTO an existing vended
// endpoint — never a parallel grant universe (ADR-0008 doctrine survives byte-for-byte).
//
// This is a deliberate sibling of internal/auth, not an extension of it: internal/auth is the OIDC
// relying party for human login (trust flowing OUT to Pocket ID), while this package is the
// authorization server MCP clients flow INTO. Different trust directions, different failure modes
// (design.md "New sibling package, not an extension of internal/auth").
//
// The full flow lives here now: authorize-request plumbing (authorize.go — the consent screen
// itself renders in internal/web behind the human session) and the token endpoint (token.go —
// code + PKCE exchange and rotating refresh, SPEC-0016 REQ "Token Issuance And Refresh").
package oauthsrv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/switchboard/internal/store"
)

// Well-known and endpoint paths of the AS surface. Exported as constants so the router
// (internal/server), the resource server's challenge (internal/mcp), and the metadata documents
// can never drift apart. Governing: SPEC-0016 REQ "Authorization Server Metadata" (every URL in
// the document resolves on this deployment).
const (
	// ASMetadataPath serves the RFC 8414 authorization-server metadata document.
	ASMetadataPath = "/.well-known/oauth-authorization-server"
	// ProtectedResourcePrefix is the RFC 9728 well-known prefix: metadata for the protected
	// resource /mcp/{slug} lives at ProtectedResourcePrefix + "/mcp/{slug}" (path-insertion form).
	ProtectedResourcePrefix = "/.well-known/oauth-protected-resource"
	// RegisterPath accepts RFC 7591 dynamic client registrations.
	RegisterPath = "/oauth/register"
	// AuthorizePath is served by internal/web (the consent screen sits behind the human session);
	// TokenPath is served by Handler.Token (token.go). Both are advertised in the AS metadata.
	AuthorizePath = "/oauth/authorize"
	TokenPath     = "/oauth/token"
)

// Registration input bounds (SPEC-0016 "Security Requirements → Input Validation"): an MCP client
// registers a handful of redirect URIs and a display name, so anything past these bounds is abuse,
// not a bigger client.
const (
	maxRedirectURIs  = 10
	maxRedirectLen   = 2000
	maxClientNameLen = 200
)

// ClientStore is the slice of the store the registration surface needs. *store.Store satisfies it;
// tests substitute a fake.
type ClientStore interface {
	CreateOAuthClient(ctx context.Context, clientID, name string, redirectURIs []string) (store.OAuthClient, error)
}

// Store is everything the AS surface needs from the store: client registration (this file) plus
// code redemption and token issuance/rotation (token.go). *store.Store satisfies it.
type Store interface {
	ClientStore
	TokenStore
}

// Handler serves the discovery metadata documents, dynamic client registration, and the token
// endpoint.
type Handler struct {
	store  ClientStore
	tokens TokenStore
	base   string // externally-reachable origin (cfg.BaseURL), no trailing slash; the RFC 8414 issuer
	log    *slog.Logger
}

// New builds the AS surface over the deployed base URL. base is cfg.BaseURL — the issuer identity
// every metadata URL derives from, so the documents are consistent with the deployment by
// construction (SPEC-0016 REQ "Authorization Server Metadata").
func New(st Store, base string, log *slog.Logger) *Handler {
	return &Handler{store: st, tokens: st, base: strings.TrimRight(base, "/"), log: log}
}

// ResourceMetadataURL is the absolute URL of the RFC 9728 protected-resource metadata for the MCP
// mount at /mcp/{slug}. The resource server's WWW-Authenticate challenge points here, so the
// derivation is shared rather than duplicated. Governing: SPEC-0016 REQ "Protected Resource
// Metadata".
func ResourceMetadataURL(base, slug string) string {
	return strings.TrimRight(base, "/") + ProtectedResourcePrefix + "/mcp/" + slug
}

// SlugOK reports whether slug is shaped like a minted endpoint slug (store.MintSlug emits only
// lowercase alphanumerics and dashes). Both the metadata handler and the challenge builder gate on
// it, so arbitrary path input is never reflected into a JSON document or a response header.
// Governing: SPEC-0016 "Security Requirements → Output encoding for user-supplied data".
func SlugOK(slug string) bool {
	if slug == "" {
		return false
	}
	for _, r := range slug {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// ASMetadata serves the RFC 8414 authorization-server metadata document. Grant surface: code +
// PKCE (S256 only) and refresh, public clients only (token_endpoint_auth_method "none") — exactly
// the OAuth 2.1 profile ADR-0019 commits to, no more. Governing: SPEC-0016 REQ "Authorization
// Server Metadata".
func (h *Handler) ASMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                h.base,
		"authorization_endpoint":                h.base + AuthorizePath,
		"token_endpoint":                        h.base + TokenPath,
		"registration_endpoint":                 h.base + RegisterPath,
		"response_types_supported":              []string{"code"},
		"response_modes_supported":              []string{"query"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	})
}

// ProtectedResourceMetadata serves the RFC 9728 document for one MCP mount. It is deliberately
// static — derived entirely from the base URL and the path slug, with NO store lookup — so it can
// neither leak whether an endpoint exists (the slug is not a secret, but endpoint existence is
// nobody's business pre-auth) nor add a database read to an unauthenticated public route. A slug
// that isn't even shaped like a minted slug 404s. Governing: SPEC-0016 REQ "Protected Resource
// Metadata".
func (h *Handler) ProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "endpoint")
	if !SlugOK(slug) {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 h.base + "/mcp/" + slug,
		"authorization_servers":    []string{h.base},
		"bearer_methods_supported": []string{"header"}, // internal/mcp consults ONLY the Authorization header
		"resource_name":            "switchboard MCP endpoint",
	})
}

// OperatorResourceMetadata serves the RFC 9728 document for the operator API resource
// (<base>/api): the discovery pointer the CLI (cmd/switchboard) reads to find the authorization
// server for its gh-style login (ADR-0023). Static like its MCP-mount sibling — no store lookup.
func (h *Handler) OperatorResourceMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 h.base + "/api",
		"authorization_servers":    []string{h.base},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "switchboard operator API",
		"scopes_supported":         []string{"operator"},
	})
}

// registerRequest is the RFC 7591 client-metadata subset switchboard honors. Unknown members are
// ignored per the RFC; members that would contradict the ADR-0019 profile (confidential clients,
// foreign grant types) are rejected rather than silently rewritten.
type registerRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
}

// Register accepts an RFC 7591 dynamic client registration: validate the redirect URIs exactly
// (no wildcards, no fragments, https or loopback-http or private-use scheme per RFC 8252), mint a
// high-entropy public client_id, persist, and answer 201 with the registered metadata. Clients are
// public — PKCE carries the proof — so no client_secret is ever minted. Governing: ADR-0019,
// SPEC-0016 REQ "Dynamic Client Registration".
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		writeRegisterError(w, "invalid_client_metadata", "request body must be a JSON client-metadata document")
		return
	}
	if req.TokenEndpointAuthMethod != "" && req.TokenEndpointAuthMethod != "none" {
		writeRegisterError(w, "invalid_client_metadata",
			"only public clients are supported: token_endpoint_auth_method must be \"none\"")
		return
	}
	for _, g := range req.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			writeRegisterError(w, "invalid_client_metadata", "unsupported grant_type "+quote(g))
			return
		}
	}
	for _, rt := range req.ResponseTypes {
		if rt != "code" {
			writeRegisterError(w, "invalid_client_metadata", "unsupported response_type "+quote(rt))
			return
		}
	}
	if len(req.RedirectURIs) == 0 {
		writeRegisterError(w, "invalid_redirect_uri", "redirect_uris is required and must be non-empty")
		return
	}
	if len(req.RedirectURIs) > maxRedirectURIs {
		writeRegisterError(w, "invalid_redirect_uri",
			fmt.Sprintf("at most %d redirect_uris are accepted", maxRedirectURIs))
		return
	}
	for _, u := range req.RedirectURIs {
		if err := ValidateRedirectURI(u); err != nil {
			writeRegisterError(w, "invalid_redirect_uri", err.Error())
			return
		}
	}
	name := req.ClientName
	if len(name) > maxClientNameLen {
		name = name[:maxClientNameLen]
	}

	clientID, err := newClientID()
	if err != nil {
		h.log.Error("oauth register: mint client_id", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	c, err := h.store.CreateOAuthClient(r.Context(), clientID, name, req.RedirectURIs)
	if err != nil {
		h.log.Error("oauth register: persist client", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  c.ClientID,
		"client_id_issued_at":        c.CreatedAt.Unix(),
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}

// ValidateRedirectURI enforces the registration-time shape of a single redirect URI: an absolute
// URI with no fragment, no wildcard host, and a scheme that cannot be abused as an open redirect —
// https anywhere, http only on a loopback interface (RFC 8252 native-app pattern), or a private-use
// scheme (e.g. app://callback) for installed MCP clients. Matching at authorize time is then exact
// string equality against what was registered (RedirectAllowed) — validation here bounds WHAT may
// be registered, never loosens HOW it is matched. Governing: SPEC-0016 REQ "Dynamic Client
// Registration" ("validated exactly — no wildcards, no open redirects").
func ValidateRedirectURI(raw string) error {
	if raw == "" {
		return errors.New("redirect_uri must not be empty")
	}
	if len(raw) > maxRedirectLen {
		return fmt.Errorf("redirect_uri exceeds %d characters", maxRedirectLen)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("redirect_uri %s does not parse", quote(raw))
	}
	if !u.IsAbs() {
		return fmt.Errorf("redirect_uri %s must be absolute", quote(raw))
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return fmt.Errorf("redirect_uri %s must not carry a fragment", quote(raw))
	}
	if strings.Contains(u.Host, "*") {
		return fmt.Errorf("redirect_uri %s must not use a wildcard host", quote(raw))
	}
	switch u.Scheme {
	case "https":
		if u.Host == "" {
			return fmt.Errorf("redirect_uri %s must carry a host", quote(raw))
		}
	case "http":
		// http is acceptable ONLY on a loopback interface: the RFC 8252 pattern for native apps
		// binding an ephemeral local listener. Anything else is a downgrade waiting to be phished.
		if !isLoopbackHost(u.Hostname()) {
			return fmt.Errorf("redirect_uri %s: http is only allowed on a loopback host", quote(raw))
		}
	default:
		// Private-use scheme (reverse-DNS or app-specific): allowed for installed clients. The
		// exact-match rule at authorize time still applies byte-for-byte.
	}
	return nil
}

// RedirectAllowed reports whether uri exactly matches one of the client's registered redirect
// URIs. Byte-for-byte equality — no wildcarding, no prefix matching, no normalization — is the
// whole defense against open redirects, so the authorize endpoint MUST route every presented
// redirect_uri through here before touching it. Governing: SPEC-0016 REQ "Dynamic Client
// Registration" scenario "any authorize request with a non-matching redirect URI is rejected".
func RedirectAllowed(c store.OAuthClient, uri string) bool {
	if uri == "" {
		return false
	}
	for _, registered := range c.RedirectURIs {
		if registered == uri {
			return true
		}
	}
	return false
}

// isLoopbackHost reports whether host names a loopback interface (localhost, 127.0.0.0/8, ::1).
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if strings.HasPrefix(host, "127.") {
		return true
	}
	return host == "::1"
}

// newClientID mints the public client identifier: 16 random bytes, hex-encoded. It is an
// identifier, not a credential (public clients prove themselves with PKCE, never a secret), so it
// is stored plaintext — but it is still minted with crypto/rand and a propagated error, matching
// internal/cred's refusal to ever hand out low-entropy material.
func newClientID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oauthsrv: read random: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// writeJSON writes a JSON document with the standard headers. These responses are machine-read
// metadata/registration documents, never HTML.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeRegisterError answers a rejected registration with the RFC 7591 error document (HTTP 400,
// error + error_description).
func writeRegisterError(w http.ResponseWriter, code, desc string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error":             code,
		"error_description": desc,
	})
}

// quote bounds and quotes a client-supplied string for an error message, bounding its length so a hostile
// registration cannot bloat responses or logs.
func quote(s string) string {
	if len(s) > 64 {
		s = s[:64] + "…"
	}
	return fmt.Sprintf("%q", s)
}

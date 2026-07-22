// Package auth is the OIDC relying-party login (ADR-0011) and session layer.
//
// Switchboard is an RP against Pocket ID (a passkey-only IdP that holds HUMANS ONLY). It trusts the
// issuer and deliberately does NOT enforce an amr/acr assurance claim — passkey step-up is deferred
// (ADR-0011). Before adding any non-passkey issuer, that guard MUST be added; this is called out at
// the provider-init site below so a reviewer confronts it.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/joestump/switchboard/internal/config"
	"github.com/joestump/switchboard/internal/store"
)

const (
	sessionCookie = "sb_session"
	stateCookie   = "sb_oidc_state"
	sessionTTL    = 12 * time.Hour
)

type ctxKey int

const (
	humanKey ctxKey = iota
	csrfKey
)

// sessionStore is the slice of the data layer the authenticator needs (human upsert + sessions).
// *store.Store satisfies it; the narrow interface keeps the OIDC flow testable without PostgreSQL.
// Governing: SPEC-0008 REQ "Human Upsert Keyed on OIDC Subject", REQ "Server-Side Session Establishment".
type sessionStore interface {
	UpsertHuman(ctx context.Context, subject, displayName, email string) (store.Human, error)
	CreateSession(ctx context.Context, tokenHash, humanID string, ttl time.Duration) error
	SessionHuman(ctx context.Context, tokenHash string) (store.Human, error)
	DeleteSession(ctx context.Context, tokenHash string) error
}

// Authenticator holds the OIDC provider + session store.
type Authenticator struct {
	cfg      config.Config
	store    sessionStore
	log      *slog.Logger
	provider *oidc.Provider
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	secure   bool
}

// New builds an Authenticator. OIDC is initialized only when configured; dev-login still works without it.
func New(ctx context.Context, cfg config.Config, st *store.Store, log *slog.Logger) (*Authenticator, error) {
	a := &Authenticator{cfg: cfg, store: st, log: log, secure: strings.HasPrefix(cfg.BaseURL, "https://")}
	if cfg.OIDCConfigured() {
		// This is the IdP-trust-set configuration point. ADR-0011: we trust this issuer wholesale and
		// enforce NO amr/acr assurance claim. That is acceptable ONLY because Pocket ID is passkey-only.
		// Adding a non-passkey issuer here MUST be paired with a phishing-resistant amr/acr step-up
		// check on consent actions (friend approvals). Do not cross silently.
		// Governing: ADR-0011 (assurance step-up deferred), SPEC-0008 REQ "Assurance Posture — Trust
		// the Issuer, Step-Up Deferred".
		provider, err := oidc.NewProvider(ctx, cfg.OIDCIssuer)
		if err != nil {
			return nil, err
		}
		a.provider = provider
		a.oauth = oauth2.Config{
			ClientID:     cfg.OIDCClientID,
			ClientSecret: cfg.OIDCClientSecret,
			RedirectURL:  cfg.OIDCRedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		}
		a.verifier = provider.Verifier(&oidc.Config{ClientID: cfg.OIDCClientID})
	}
	return a, nil
}

// Human is the authenticated principal carried in request context.
type Human = store.Human

// FromContext returns the authenticated human, if any.
func FromContext(ctx context.Context) (Human, bool) {
	h, ok := ctx.Value(humanKey).(Human)
	return h, ok
}

// CSRFFromContext returns the per-session CSRF token for the current request, if authenticated.
// Handlers embed it as a hidden field in state-changing forms; RequireCSRF validates it. (SPEC-0008.)
func CSRFFromContext(ctx context.Context) string {
	t, _ := ctx.Value(csrfKey).(string)
	return t
}

// csrfToken derives a per-session CSRF synchronizer token from the (secret, HttpOnly) session token.
// It is bound to the session, safe to embed in HTML (preimage-resistant; leaks nothing about the
// session token), and needs no server-side storage. An attacker cannot forge it without reading the
// victim's session cookie, which SameSite=Lax + HttpOnly + the same-origin policy prevent.
func csrfToken(sessionToken string) string {
	sum := sha256.Sum256([]byte("switchboard-csrf:" + sessionToken))
	return hex.EncodeToString(sum[:])
}

// oidcState is the short-lived per-login state stashed in a cookie across the redirect.
type oidcState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
}

// Login begins the OIDC authorization-code flow (with PKCE + nonce).
// Governing: SPEC-0008 REQ "OIDC Relying-Party Login".
func (a *Authenticator) Login(w http.ResponseWriter, r *http.Request) {
	if a.provider == nil {
		http.Error(w, "OIDC not configured (set SWITCHBOARD_OIDC_* or SWITCHBOARD_DEV_LOGIN=1)", http.StatusServiceUnavailable)
		return
	}
	state, err := randToken()
	if err != nil {
		a.log.Error("login: generate state", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	nonce, err := randToken()
	if err != nil {
		a.log.Error("login: generate nonce", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	st := oidcState{State: state, Nonce: nonce, Verifier: oauth2.GenerateVerifier()}
	raw, _ := json.Marshal(st)
	http.SetCookie(w, a.cookie(stateCookie, base64.RawURLEncoding.EncodeToString(raw), 10*time.Minute))
	url := a.oauth.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier))
	http.Redirect(w, r, url, http.StatusFound)
}

// Callback completes the flow: exchange code, verify the ID token, upsert the human, mint a session.
// Governing: SPEC-0008 REQ "Callback Verification", REQ "Human Upsert Keyed on OIDC Subject".
func (a *Authenticator) Callback(w http.ResponseWriter, r *http.Request) {
	if a.provider == nil {
		http.Error(w, "OIDC not configured", http.StatusServiceUnavailable)
		return
	}
	c, err := r.Cookie(stateCookie)
	if err != nil {
		a.log.Warn("oidc callback rejected", "reason", "missing state cookie", "remote", r.RemoteAddr)
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		a.log.Warn("oidc callback rejected", "reason", "malformed state cookie", "remote", r.RemoteAddr)
		http.Error(w, "bad state", http.StatusBadRequest)
		return
	}
	var st oidcState
	if err := json.Unmarshal(raw, &st); err != nil || st.State == "" || st.State != r.URL.Query().Get("state") {
		a.log.Warn("oidc callback rejected", "reason", "state mismatch", "remote", r.RemoteAddr)
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, a.cookie(stateCookie, "", -time.Hour)) // clear

	token, err := a.oauth.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		a.log.Warn("oidc exchange failed", "err", err)
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok {
		a.log.Warn("oidc callback rejected", "reason", "no id_token in token response", "remote", r.RemoteAddr)
		http.Error(w, "no id_token", http.StatusBadGateway)
		return
	}
	// The verifier checks issuer, audience, expiry, and signature only. Deliberately NO amr/acr
	// assurance check here — the trusted issuer is passkey-only (ADR-0011); see New for the guard
	// that must accompany any non-passkey issuer.
	// Governing: ADR-0011, SPEC-0008 REQ "Assurance Posture — Trust the Issuer, Step-Up Deferred".
	idToken, err := a.verifier.Verify(r.Context(), rawID)
	if err != nil {
		a.log.Warn("oidc callback rejected", "reason", "id_token verification failed", "err", err, "remote", r.RemoteAddr)
		http.Error(w, "id_token verify failed", http.StatusBadGateway)
		return
	}
	if idToken.Nonce != st.Nonce {
		a.log.Warn("oidc callback rejected", "reason", "nonce mismatch", "remote", r.RemoteAddr)
		http.Error(w, "nonce mismatch", http.StatusBadRequest)
		return
	}
	var claims struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	_ = idToken.Claims(&claims)

	if err := a.establishSession(r.Context(), w, idToken.Subject, claims.Name, claims.Email); err != nil {
		a.log.Error("establish session", "err", err)
		http.Error(w, "login failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// ErrProvenanceUnavailable is returned by VerifyProvenance when OIDC is not configured, so the
// server has no trusted issuer to verify a friend request's provenance against. Callers distinguish
// this (a 503 "the feature needs OIDC") from a token that simply failed to verify (a 401).
var ErrProvenanceUnavailable = errors.New("auth: provenance verification unavailable (OIDC not configured)")

// VerifyProvenance verifies an OIDC-signed provenance assertion — the requesting human's ID token —
// against the SAME trusted issuer switchboard authenticates its own humans with, and returns the
// attested subject (OIDC `sub`) plus display name/email for the legible pending-request row. This is the
// A2A friend-request credential: the request carries the requesting human's signed identity in-band,
// NOT the agent's self-assertion of who owns it (ADR-0010). The verifier checks issuer, audience,
// expiry, and signature; it deliberately does NOT enforce an amr/acr assurance claim because the one
// trusted issuer (Pocket ID) is passkey-only — passkey step-up stays deferred until a non-passkey
// issuer is added (ADR-0011; the guard lives at New's provider-init site).
//
// A verification failure is returned as an error and MUST be surfaced by the caller as
// `unauthenticated` with no pending edge created (SPEC-0010 "Missing or invalid provenance is
// rejected"). No network callback is made here: the issuer's JWKS was fetched once at New and the
// verifier validates the JWT signature locally, so there is no per-request SSRF surface.
// Governing: ADR-0010 (verifiable OIDC-signed provenance), ADR-0011 (trust the passkey-only issuer,
// step-up deferred), SPEC-0010 REQ "Verifiable OIDC-Signed Provenance".
func (a *Authenticator) VerifyProvenance(ctx context.Context, rawIDToken string) (subject, name, email string, err error) {
	if a.verifier == nil {
		return "", "", "", ErrProvenanceUnavailable
	}
	idToken, err := a.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return "", "", "", fmt.Errorf("auth: provenance verify: %w", err)
	}
	if idToken.Subject == "" {
		return "", "", "", errors.New("auth: provenance token has empty subject")
	}
	var claims struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	_ = idToken.Claims(&claims)
	return idToken.Subject, claims.Name, claims.Email, nil
}

// DevLogin mints a session for a fixed local human, bypassing OIDC. Guarded by SWITCHBOARD_DEV_LOGIN:
// when the flag is unset the route answers 404 and establishes no session — in every build, including
// production. Governing: SPEC-0008 REQ "Development Login Guard".
func (a *Authenticator) DevLogin(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.DevLogin {
		http.NotFound(w, r)
		return
	}
	a.log.Warn("DEV LOGIN used — OIDC bypassed; never enable SWITCHBOARD_DEV_LOGIN in production")
	if err := a.establishSession(r.Context(), w, "dev|local", "Local Dev", "dev@localhost"); err != nil {
		http.Error(w, "dev login failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// Logout revokes the session.
func (a *Authenticator) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = a.store.DeleteSession(r.Context(), hashToken(c.Value))
	}
	http.SetCookie(w, a.cookie(sessionCookie, "", -time.Hour))
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *Authenticator) establishSession(ctx context.Context, w http.ResponseWriter, subject, name, email string) error {
	h, err := a.store.UpsertHuman(ctx, subject, name, email)
	if err != nil {
		return err
	}
	tok, err := randToken()
	if err != nil {
		return err
	}
	if err := a.store.CreateSession(ctx, hashToken(tok), h.ID, sessionTTL); err != nil {
		return err
	}
	http.SetCookie(w, a.cookie(sessionCookie, tok, sessionTTL))
	return nil
}

// RequireHuman is middleware that admits only authenticated humans; others are redirected to login.
// It also stashes the per-session CSRF token so downstream handlers can embed it in forms.
// Governing: SPEC-0008 REQ "Session-Gated Human Surface", REQ "Server-Side Session Establishment"
// (only live sessions admit access; expired sessions are treated as unauthenticated).
func (a *Authenticator) RequireHuman(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, err := a.sessionHuman(r)
		if err != nil {
			// Clear ONLY a cookie we can prove is dead — one that resolved to no live session
			// (garbage, revoked, or expired → store.ErrNotFound). A transient store failure (a DB
			// blip) leaves the cookie's validity UNKNOWN, so we must NOT delete a possibly-valid
			// session cookie — doing so would silently log the human out on every hiccup. We still
			// deny THIS request (fail closed) and redirect to login regardless.
			// Governing: SPEC-0008 REQ "Session-Gated Human Surface".
			switch {
			case errors.Is(err, store.ErrNotFound):
				http.SetCookie(w, a.cookie(sessionCookie, "", -time.Hour))
			case errors.Is(err, http.ErrNoCookie):
				// No cookie was presented — nothing to clear.
			default:
				a.log.Warn("session lookup", "err", err)
			}
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		ctx := context.WithValue(r.Context(), humanKey, h)
		if c, err := r.Cookie(sessionCookie); err == nil {
			ctx = context.WithValue(ctx, csrfKey, csrfToken(c.Value))
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireCSRF rejects unsafe (state-changing) requests whose CSRF token does not match the one
// derived from the caller's session. Safe methods pass through untouched. The token is read from the
// csrf_token form field or the X-CSRF-Token header. Governing: SPEC-0008 REQ CSRF synchronizer tokens.
func (a *Authenticator) RequireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
			next.ServeHTTP(w, r)
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		expected := csrfToken(c.Value)
		got := r.FormValue("csrf_token")
		if got == "" {
			got = r.Header.Get("X-CSRF-Token")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
			a.log.Warn("csrf token mismatch", "path", r.URL.Path, "remote", r.RemoteAddr)
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// LoadHuman is middleware that injects the human when present but does not require it — the seam for
// dual-mode routes like GET / that render one surface for an authenticated operator and another for
// an anonymous visitor. When a live session is present it also stashes the per-session CSRF token
// (exactly as RequireHuman does), so a human-facing page mounted here can embed it in forms and the
// HTMX header without a second lookup; an anonymous request carries neither value and flows through
// untouched. Governing: SPEC-0008 REQ "Session-Gated Human Surface" (a dead/absent cookie is simply
// unauthenticated here, never an error).
func (a *Authenticator) LoadHuman(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := a.human(r); ok {
			ctx := context.WithValue(r.Context(), humanKey, h)
			if c, err := r.Cookie(sessionCookie); err == nil {
				ctx = context.WithValue(ctx, csrfKey, csrfToken(c.Value))
			}
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Authenticator) human(r *http.Request) (Human, bool) {
	h, err := a.sessionHuman(r)
	if err != nil {
		// ErrNotFound (dead cookie) and http.ErrNoCookie (no cookie) are ordinary "not logged in"
		// outcomes; only a transient store failure is worth a log line.
		if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, http.ErrNoCookie) {
			a.log.Warn("session lookup", "err", err)
		}
		return Human{}, false
	}
	return h, true
}

// sessionHuman resolves the caller's session cookie to a live human, preserving WHY resolution
// failed so callers can react correctly. The returned error is one of:
//   - nil                → authenticated
//   - http.ErrNoCookie   → no session cookie was presented
//   - store.ErrNotFound  → a cookie was presented but resolves to NO live session (garbage,
//     revoked, or expired): the cookie is provably dead
//   - any other error    → a transient store failure; the cookie's validity is UNKNOWN and it MUST
//     NOT be treated as dead (see RequireHuman's cookie-clearing decision)
//
// Governing: SPEC-0008 REQ "Session-Gated Human Surface", REQ "Server-Side Session Establishment".
func (a *Authenticator) sessionHuman(r *http.Request) (Human, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return Human{}, err // http.ErrNoCookie
	}
	return a.store.SessionHuman(r.Context(), hashToken(c.Value))
}

func (a *Authenticator) cookie(name, value string, ttl time.Duration) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(ttl),
		MaxAge:   int(ttl.Seconds()),
	}
}

// randToken returns a 256-bit URL-safe random string. A crypto/rand failure is surfaced, never
// swallowed — a silent low-entropy state/nonce/session token would defeat CSRF/replay protection
// and session unguessability. Governing: SPEC-0008.
func randToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Package auth is the OIDC relying-party login (ADR-011) and session layer.
//
// Switchboard is an RP against Pocket ID (a passkey-only IdP that holds HUMANS ONLY). It trusts the
// issuer and deliberately does NOT enforce an amr/acr assurance claim — passkey step-up is deferred
// (ADR-011). Before adding any non-passkey issuer, that guard MUST be added; this is called out at
// the provider-init site below so a reviewer confronts it.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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

const humanKey ctxKey = 0

// Authenticator holds the OIDC provider + session store.
type Authenticator struct {
	cfg      config.Config
	store    *store.Store
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
		// ADR-011: we trust this issuer wholesale and enforce NO amr/acr assurance claim. That is
		// acceptable ONLY because Pocket ID is passkey-only. Adding a non-passkey issuer here MUST be
		// paired with an amr/acr step-up check on consent actions (friend approvals). Do not cross silently.
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

// oidcState is the short-lived per-login state stashed in a cookie across the redirect.
type oidcState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
}

// Login begins the OIDC authorization-code flow (with PKCE + nonce).
func (a *Authenticator) Login(w http.ResponseWriter, r *http.Request) {
	if a.provider == nil {
		http.Error(w, "OIDC not configured (set SWITCHBOARD_OIDC_* or SWITCHBOARD_DEV_LOGIN=1)", http.StatusServiceUnavailable)
		return
	}
	st := oidcState{State: randToken(), Nonce: randToken(), Verifier: oauth2.GenerateVerifier()}
	raw, _ := json.Marshal(st)
	http.SetCookie(w, a.cookie(stateCookie, base64.RawURLEncoding.EncodeToString(raw), 10*time.Minute))
	url := a.oauth.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier))
	http.Redirect(w, r, url, http.StatusFound)
}

// Callback completes the flow: exchange code, verify the ID token, upsert the human, mint a session.
func (a *Authenticator) Callback(w http.ResponseWriter, r *http.Request) {
	if a.provider == nil {
		http.Error(w, "OIDC not configured", http.StatusServiceUnavailable)
		return
	}
	c, err := r.Cookie(stateCookie)
	if err != nil {
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		http.Error(w, "bad state", http.StatusBadRequest)
		return
	}
	var st oidcState
	if err := json.Unmarshal(raw, &st); err != nil || st.State == "" || st.State != r.URL.Query().Get("state") {
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
		http.Error(w, "no id_token", http.StatusBadGateway)
		return
	}
	idToken, err := a.verifier.Verify(r.Context(), rawID)
	if err != nil {
		http.Error(w, "id_token verify failed", http.StatusBadGateway)
		return
	}
	if idToken.Nonce != st.Nonce {
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

// DevLogin mints a session for a fixed local human, bypassing OIDC. Guarded by SWITCHBOARD_DEV_LOGIN.
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
	tok := randToken()
	if err := a.store.CreateSession(ctx, hashToken(tok), h.ID, sessionTTL); err != nil {
		return err
	}
	http.SetCookie(w, a.cookie(sessionCookie, tok, sessionTTL))
	return nil
}

// RequireHuman is middleware that admits only authenticated humans; others are redirected to login.
func (a *Authenticator) RequireHuman(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, ok := a.human(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), humanKey, h)))
	})
}

// LoadHuman is middleware that injects the human when present but does not require it.
func (a *Authenticator) LoadHuman(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := a.human(r); ok {
			r = r.WithContext(context.WithValue(r.Context(), humanKey, h))
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Authenticator) human(r *http.Request) (Human, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return Human{}, false
	}
	h, err := a.store.SessionHuman(r.Context(), hashToken(c.Value))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			a.log.Warn("session lookup", "err", err)
		}
		return Human{}, false
	}
	return h, true
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

func randToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

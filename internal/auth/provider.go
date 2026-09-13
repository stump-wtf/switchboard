package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// identity is what a provider hands back after a completed login: the verified
// principal plus the provenance the session must record. Issuer is the trusted
// issuer string (the OIDC issuer URL, or "https://github.com/" for the GitHub
// provider); Subject is the provider-side subject; HumanSubject is the key the
// human row is upserted on (OIDC uses its raw sub; GitHub namespaces its
// numeric id so a Pocket ID sub can never collide with a GitHub account).
// Governing: SPEC-0021 REQ "Session Parity and Provenance".
type identity struct {
	Issuer       string
	Subject      string // provider-side subject, recorded as session provenance
	HumanSubject string // the humans.oidc_subject key this identity maps to
	Name         string
	Email        string
}

// AuthProvider is one human-login method behind the shared /auth/login and
// /auth/callback routes (ADR-0026: "a third provider is config + one file" —
// one route tree, one state cookie, one session-establishment path; a parallel
// per-provider route tree would duplicate security-critical code that drifts).
// Governing: SPEC-0021, cairn ADR-0019 (same provider-interface shape).
type AuthProvider interface {
	// ID is the provider query-parameter value ("pocket-id", "github") and the
	// registry key. It is recorded in the state cookie at login start so the
	// callback finishes with the provider that started the flow — never with
	// whatever the callback query string claims on its own.
	ID() string
	// DisplayName is the human-visible name (login page, error surfaces).
	DisplayName() string
	// Configured reports whether this provider can start a login. An
	// unconfigured provider 404s on /auth/login?provider=<id> and is not
	// offered on the login page (SPEC-0021 REQ "Provider Selection on the
	// Login Page").
	Configured() bool
	// Begin builds the provider's authorize URL and the per-login state
	// payload to carry across the redirect. The state cookie itself — its
	// name, TTL, single-use clearing — stays the Authenticator's, so every
	// provider inherits the same CSRF/replay protection (SPEC-0008 state
	// cookie semantics reused verbatim).
	Begin(r *http.Request) (authorizeURL string, st oidcState, err error)
	// Finish completes the flow: exchange the code, verify what the provider
	// can verify (ID token for OIDC; profile fetch for GitHub), and return
	// the identity. It runs AFTER the Authenticator has validated and cleared
	// the state cookie, and before establishSession. The GitHub access token
	// lives only inside Finish and is never returned, persisted, or logged
	// (SPEC-0021 REQ "Token Containment").
	Finish(ctx context.Context, st oidcState, code string) (identity, error)
}

// ProviderName is the provider id recorded at login start; an empty value is a
// pre-provider state cookie and means Pocket ID, whose URL carried no provider
// parameter.
func (s oidcState) ProviderName() string {
	if s.Provider == "" {
		return PocketIDProviderID
	}
	return s.Provider
}

// pocketIDProvider adapts the existing OIDC machinery (the Authenticator's
// provider/oauth/verifier fields) to AuthProvider. It is the pre-existing
// Pocket ID flow, unchanged in behavior — merely reshaped behind the provider
// interface (SPEC-0021: "Pocket ID default behavior unchanged").
type pocketIDProvider struct {
	a *Authenticator
}

func (p *pocketIDProvider) ID() string          { return PocketIDProviderID }
func (p *pocketIDProvider) DisplayName() string { return "Pocket ID" }
func (p *pocketIDProvider) Configured() bool    { return p.a.provider != nil }

// Begin builds the OIDC authorize URL with PKCE + nonce, as Login always did.
func (p *pocketIDProvider) Begin(r *http.Request) (string, oidcState, error) {
	state, err := randToken()
	if err != nil {
		return "", oidcState{}, fmt.Errorf("pocket-id: generate state: %w", err)
	}
	nonce, err := randToken()
	if err != nil {
		return "", oidcState{}, fmt.Errorf("pocket-id: generate nonce: %w", err)
	}
	st := oidcState{State: state, Nonce: nonce, Verifier: oauth2.GenerateVerifier(), Provider: PocketIDProviderID}
	url := p.a.oauth.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier))
	return url, st, nil
}

// Finish exchanges the code, verifies the ID token, and returns the identity.
// The amr/acr posture is unchanged: deliberately NO assurance check here — the
// trusted issuer is passkey-only (ADR-0011); see New for the guard that must
// accompany any further non-passkey issuer.
// Governing: ADR-0011, SPEC-0008 REQ "Assurance Posture — Trust the Issuer,
// Step-Up Deferred".
func (p *pocketIDProvider) Finish(ctx context.Context, st oidcState, code string) (identity, error) {
	token, err := p.a.oauth.Exchange(ctx, code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		return identity{}, fmt.Errorf("%w: %v", errExchangeFailed, err)
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok {
		return identity{}, errNoIDToken
	}
	idToken, err := p.a.verifier.Verify(ctx, rawID)
	if err != nil {
		return identity{}, fmt.Errorf("%w: %v", errVerifyFailed, err)
	}
	if idToken.Nonce != st.Nonce {
		return identity{}, errNonceMismatch
	}
	var claims struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	_ = idToken.Claims(&claims)
	return identity{
		Issuer: idToken.Issuer, Subject: idToken.Subject, HumanSubject: idToken.Subject,
		Name: claims.Name, Email: claims.Email,
	}, nil
}

// Provider-failure sentinels. Finish wraps its failures in these so the
// shared callback maps each to the same user-visible status and message the
// single-provider flow always returned — and logs the detail server-side
// without echoing provider protocol detail to the browser.
var (
	errExchangeFailed = errors.New("token exchange failed")
	errNoIDToken      = errors.New("no id_token")
	errVerifyFailed   = errors.New("id_token verify failed")
	errNonceMismatch  = errors.New("nonce mismatch")
	// errUnverifiedEmail is the GitHub-only rejection: a login without a
	// verified primary email (SPEC-0021 REQ "GitHub OAuth Callback Exchange",
	// scenario "Unverified or missing primary email").
	errUnverifiedEmail = errors.New("github: no primary verified email")
)

const (
	// PocketIDProviderID is the default provider: the pre-existing OIDC flow.
	// A callback or login without an explicit provider keeps behaving exactly
	// as it did before providers existed (SPEC-0021: "Pocket ID default
	// behavior unchanged").
	PocketIDProviderID = "pocket-id"
	// GitHubProviderID is the GitHub OAuth provider's registry key and query
	// parameter value.
	GitHubProviderID = "github"
)

// providerFor resolves the registry entry for a provider id. An unknown id is
// indistinguishable from a mistyped link and 404s at the caller, per SPEC-0021.
func (a *Authenticator) providerFor(id string) AuthProvider {
	if id == "" {
		id = PocketIDProviderID
	}
	return a.providers[id]
}

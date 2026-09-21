package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"golang.org/x/oauth2"

	"github.com/stump-wtf/switchboard/internal/config"
)

// githubAPI is the API base the callback calls exactly twice (profile +
// emails) and then never again: the access token dies at Finish's return
// (SPEC-0021 REQ "Token Containment" — zero GitHub API traffic in steady
// state). A var so tests can point it at an httptest server.
var githubAPI = "https://api.github.com"

// githubProvider implements AuthProvider for GitHub OAuth 2.0 (ADR-0026).
// GitHub issues no OIDC ID token for user login, so identity is the code
// exchange plus GET /user and GET /user/emails, anchored on the primary
// verified email (SPEC-0021 REQ "GitHub OAuth Callback Exchange").
type githubProvider struct {
	cfg   config.Config
	oauth oauth2.Config
	log   *slog.Logger
	http  *http.Client
}

// newGitHubProvider builds the provider; nil when credentials are absent, in
// which case the provider stays unconfigured (login route 404s, login page
// hides the button) rather than half-alive.
func newGitHubProvider(cfg config.Config, log *slog.Logger) *githubProvider {
	if !cfg.GitHubConfigured() {
		return nil
	}
	return &githubProvider{
		cfg:  cfg,
		log:  log,
		http: http.DefaultClient,
		oauth: oauth2.Config{
			ClientID:     cfg.GitHubClientID,
			ClientSecret: cfg.GitHubClientSecret,
			RedirectURL:  cfg.GitHubRedirectURL,
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://github.com/login/oauth/authorize",
				TokenURL: "https://github.com/login/oauth/access_token",
			},
			// read:user for GET /user, user:email for GET /user/emails. No
			// repo scopes — ever: this provider grants board access only.
			Scopes: []string{"read:user", "user:email"},
		},
	}
}

func (g *githubProvider) ID() string          { return GitHubProviderID }
func (g *githubProvider) DisplayName() string { return "GitHub" }
func (g *githubProvider) Configured() bool    { return g != nil }

// Begin builds the GitHub authorize URL. The Authenticator-owned state cookie
// carries the anti-CSRF state; GitHub gets no nonce/PKCE (GitHub's OAuth app
// flow supports neither), so the single-use short-TTL state cookie is the
// replay control (SPEC-0021 Security Requirements).
func (g *githubProvider) Begin(r *http.Request) (string, oidcState, error) {
	state, err := randToken()
	if err != nil {
		return "", oidcState{}, fmt.Errorf("github: generate state: %w", err)
	}
	st := oidcState{State: state, Provider: GitHubProviderID}
	return g.oauth.AuthCodeURL(st.State), st, nil
}

// Finish exchanges the code and resolves the identity. State validation has
// already happened by the time this runs — a bad or replayed state never
// reaches a token exchange (SPEC-0021 scenario "Invalid or replayed state").
func (g *githubProvider) Finish(ctx context.Context, st oidcState, code string) (identity, error) {
	tok, err := g.oauth.Exchange(ctx, code)
	if err != nil {
		return identity{}, fmt.Errorf("github: token exchange: %w", err)
	}
	// The token is used here and dropped when Finish returns: it never reaches
	// a session, cookie, log line, or database row. establishSession receives
	// only the resolved identity.
	api := g.apiClient(ctx, tok.AccessToken)

	var profile struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
		Name  string `json:"name"`
	}
	if err := g.getJSON(ctx, api, "/user", &profile); err != nil {
		return identity{}, err
	}
	if profile.ID == 0 {
		return identity{}, fmt.Errorf("github: /user returned no id")
	}

	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	// GET /user/emails returns a JSON array.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPI+"/user/emails", nil)
	if err != nil {
		return identity{}, err
	}
	resp, err := api.Do(req)
	if err != nil {
		return identity{}, fmt.Errorf("github: fetch emails: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return identity{}, fmt.Errorf("github: fetch emails: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&emails); err != nil {
		return identity{}, fmt.Errorf("github: decode emails: %w", err)
	}

	email := ""
	for _, e := range emails {
		if e.Primary && e.Verified {
			email = e.Email
			break
		}
	}
	if email == "" {
		// SPEC-0021 scenario "Unverified or missing primary email": reject with
		// a user-visible error, no session; the log names the reason and
		// carries no token.
		g.log.Warn("github login rejected", "reason", "no primary verified email", "login", profile.Login)
		return identity{}, errUnverifiedEmail
	}

	name := profile.Name
	if name == "" {
		name = profile.Login
	}
	return identity{
		Issuer:       "https://github.com/",
		Subject:      fmt.Sprintf("%d", profile.ID),
		HumanSubject: fmt.Sprintf("github.com|%d", profile.ID),
		Name:         name,
		Email:        email,
	}, nil
}

func (g *githubProvider) apiClient(ctx context.Context, token string) *http.Client {
	return &http.Client{Transport: &bearerTransport{ctx: ctx, token: token, base: g.http.Transport}}
}

type bearerTransport struct {
	ctx   context.Context
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(t.ctx)
	r.Header.Set("Authorization", "Bearer "+t.token)
	r.Header.Set("Accept", "application/vnd.github+json")
	if t.base == nil {
		return http.DefaultTransport.RoundTrip(r)
	}
	return t.base.RoundTrip(r)
}

func (g *githubProvider) getJSON(ctx context.Context, client *http.Client, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPI+path, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("github: fetch %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("github: fetch %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(out)
}

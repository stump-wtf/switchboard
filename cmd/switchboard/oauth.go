package main

// The OAuth Login
//
// Dynamic client registration (RFC 7591), authorization code + PKCE (S256) with a loopback
// redirect, and a rotating refresh token — the gh pattern, against switchboard's own
// authorization server (ADR-0019). The authorize request carries resource = <base>/api, so the
// consent screen and the minted token are bound to the OPERATOR (the signed-in human), not to any
// vended endpoint (ADR-0023). Only token plaintexts ever exist in memory and in the credentials
// file; switchboard stores hashes.
//
// @joestump-agent 09/03/2026 - The loopback listener answers only the callback carrying our state
// (a stray hit no longer aborts the login), the browser opener is per-OS and injectable, and a
// token-endpoint error surfaces the server's RFC 6749 error document instead of an empty body.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// asMetadata is the RFC 8414 subset the CLI needs.
type asMetadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// callback is what the loopback redirect delivers.
type callback struct {
	code, err, desc string
}

// login runs the whole flow against base and returns the credentials to persist. open, when
// non-nil, is asked to open the authorization URL in a browser; the URL is printed either way.
func (c *cli) login(ctx context.Context, base string, open func(string) error) (*credentials, error) {
	as, err := c.discover(ctx, base)
	if err != nil {
		return nil, err
	}

	state, err := randomString(32)
	if err != nil {
		return nil, err
	}
	// Loopback listener first: the port is known before registration so the redirect URI is
	// exact-matchable (the AS enforces an exact allowlist).
	ln, redirectURI, codeCh, err := listenLoopback(state)
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	defer func() { _ = ln.Close() }()

	clientID, err := c.registerClient(ctx, as.RegistrationEndpoint, redirectURI)
	if err != nil {
		return nil, err
	}
	verifier, err := pkceVerifier()
	if err != nil {
		return nil, err
	}

	authz := as.AuthorizationEndpoint + "?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
		"resource":              {base + "/api"},
	}.Encode()

	opened := false
	if open != nil {
		opened = open(authz) == nil
	}
	if opened {
		fmt.Fprintln(c.stdout, "Opening your browser to complete the login. If it does not open, visit:")
	} else {
		fmt.Fprintln(c.stdout, "Open this URL in your browser to complete the login:")
	}
	fmt.Fprintf(c.stdout, "\n  %s\n\n", authz)

	// The callback must arrive within the login timeout (the browser is a human), carrying our
	// state — the listener has already dropped anything else.
	timeout := c.loginTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	select {
	case cb := <-codeCh:
		if cb.err != "" {
			if cb.desc != "" {
				return nil, fmt.Errorf("login denied: %s (%s)", cb.err, cb.desc)
			}
			return nil, fmt.Errorf("login denied: %s", cb.err)
		}
		tok, err := c.postToken(ctx, as.TokenEndpoint, url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {cb.code},
			"code_verifier": {verifier},
			"client_id":     {clientID},
			"redirect_uri":  {redirectURI},
		})
		if err != nil {
			return nil, err
		}
		creds := &credentials{BaseURL: base, ClientID: clientID}
		creds.apply(tok, c.now())
		return creds, nil
	case <-time.After(timeout):
		return nil, errors.New("login: timed out waiting for the browser to come back")
	case <-ctx.Done():
		return nil, fmt.Errorf("login: %w", ctx.Err())
	}
}

// listenLoopback binds 127.0.0.1 on an ephemeral port and serves the redirect. Only a callback
// carrying wantState is forwarded (exactly once); anything else is answered and dropped, so a
// stray or forged hit cannot abort or hijack the login.
func listenLoopback(wantState string) (net.Listener, string, <-chan callback, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", nil, err
	}
	codeCh := make(chan callback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if q.Get("state") != wantState {
			w.WriteHeader(http.StatusBadRequest)
			writeCallbackPage(w, "Login failed", "This callback does not belong to the login in progress. Return to the terminal and try again.")
			return
		}
		cb := callback{err: q.Get("error"), desc: q.Get("error_description")}
		if cb.err == "" {
			cb.code = q.Get("code")
		}
		if cb.err != "" {
			writeCallbackPage(w, "Login failed", "switchboard reported: "+cb.err+" "+cb.desc)
		} else {
			writeCallbackPage(w, "Login complete", "You can close this window and return to the terminal.")
		}
		select {
		case codeCh <- cb:
		default: // a second matching callback after the first: already delivered
		}
	})
	go func() { _ = http.Serve(ln, mux) }()
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)
	return ln, redirectURI, codeCh, nil
}

func writeCallbackPage(w io.Writer, title, msg string) {
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>switchboard · %s</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem;color:#222}h1{font-size:1.25rem}</style>
<h1>%s</h1><p>%s</p>
`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(msg))
}

// discover resolves the authorization server from the operator API's RFC 9728 protected-resource
// metadata, so the CLI never hardcodes endpoint paths.
func (c *cli) discover(ctx context.Context, base string) (*asMetadata, error) {
	var pr struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := c.getJSON(ctx, base+"/.well-known/oauth-protected-resource/api", &pr); err != nil {
		return nil, fmt.Errorf("discovering %s: %w (is this a switchboard deployment?)", base, err)
	}
	if len(pr.AuthorizationServers) == 0 {
		return nil, fmt.Errorf("discovering %s: no authorization server advertised", base)
	}
	var as asMetadata
	if err := c.getJSON(ctx, strings.TrimRight(pr.AuthorizationServers[0], "/")+"/.well-known/oauth-authorization-server", &as); err != nil {
		return nil, fmt.Errorf("discovering the authorization server: %w", err)
	}
	if as.AuthorizationEndpoint == "" || as.TokenEndpoint == "" || as.RegistrationEndpoint == "" {
		return nil, errors.New("discovering the authorization server: metadata is missing an endpoint")
	}
	return &as, nil
}

// registerClient performs RFC 7591 dynamic registration as a public PKCE client bound to the
// loopback redirect URI.
func (c *cli) registerClient(ctx context.Context, registrationEndpoint, redirectURI string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"client_name":                "switchboard CLI",
		"redirect_uris":              []string{redirectURI},
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registrationEndpoint, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("client registration: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("client registration: %s: %s", statusText(resp.StatusCode), strings.TrimSpace(string(b)))
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.ClientID == "" {
		return "", errors.New("client registration: no client_id in the response")
	}
	return out.ClientID, nil
}

// postToken calls the token endpoint. A rejected request surfaces the server's RFC 6749 error
// document (error + error_description) rather than a bare status.
func (c *cli) postToken(ctx context.Context, endpoint string, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			if e.Description != "" {
				return nil, fmt.Errorf("token endpoint: %s (%s)", e.Error, e.Description)
			}
			return nil, fmt.Errorf("token endpoint: %s", e.Error)
		}
		return nil, fmt.Errorf("token endpoint: %s: %s", statusText(resp.StatusCode), strings.TrimSpace(string(b)))
	}
	var out tokenResponse
	if err := json.Unmarshal(b, &out); err != nil || out.AccessToken == "" {
		return nil, errors.New("token endpoint: malformed response (no access_token)")
	}
	return &out, nil
}

func (c *cli) getJSON(ctx context.Context, rawurl string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", rawurl, statusText(resp.StatusCode))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(out)
}

// randomString is n base64url characters of CSPRNG material (PKCE verifiers and OAuth state).
func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b)[:n], nil
}

// pkceVerifier is 96 characters: comfortably inside RFC 7636's 43–128 verifier window.
func pkceVerifier() (string, error) { return randomString(96) }

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

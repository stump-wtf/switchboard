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
	Issuer                      string `json:"issuer"`
	AuthorizationEndpoint       string `json:"authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
	RegistrationEndpoint        string `json:"registration_endpoint"`
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
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

// oauthError is the RFC 6749 §5.2 error document of a rejected token request. The device poll
// loop reads the error code out of it (authorization_pending, slow_down, ...); every other caller
// turns it into an error.
type oauthError struct {
	Code        string
	Description string
}

func (e *oauthError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("token endpoint: %s (%s)", e.Code, e.Description)
	}
	return "token endpoint: " + e.Code
}

// postToken calls the token endpoint. A rejected request surfaces the server's RFC 6749 error
// document as an *oauthError rather than a bare status, so callers that must branch on the error
// code can, and everyone else gets a readable message.
func (c *cli) postTokenRaw(ctx context.Context, endpoint string, form url.Values) (*tokenResponse, *oauthError, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("token endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		var doc struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		if json.Unmarshal(b, &doc) == nil && doc.Error != "" {
			return nil, &oauthError{Code: doc.Error, Description: doc.Description}, nil
		}
		return nil, nil, fmt.Errorf("token endpoint: %s: %s", statusText(resp.StatusCode), strings.TrimSpace(string(b)))
	}
	var out tokenResponse
	if err := json.Unmarshal(b, &out); err != nil || out.AccessToken == "" {
		return nil, nil, errors.New("token endpoint: malformed response (no access_token)")
	}
	return &out, nil, nil
}

func (c *cli) postToken(ctx context.Context, endpoint string, form url.Values) (*tokenResponse, error) {
	tok, oerr, err := c.postTokenRaw(ctx, endpoint, form)
	if err != nil {
		return nil, err
	}
	if oerr != nil {
		return nil, oerr
	}
	return tok, nil
}

// sleepCtx waits d unless ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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

// loginDevice runs the RFC 8628 device flow: discover, register (the redirect URI stays part of
// the registration even though the device flow never uses it — DCR requires one, and the same
// client may later do a loopback login), start the request, print what a human needs on any other
// device, and poll the token endpoint until the decision lands. The poll interval is the server's
// (interval), and slow_down grows it by a second per hit, exactly as the RFC directs. The token
// pair this returns is identical in kind to the loopback flow's — the same operator-bound grant
// (ADR-0023), because the consent happens to the same human on the same consent screen.
func (c *cli) loginDevice(ctx context.Context, base string) (*credentials, error) {
	as, err := c.discover(ctx, base)
	if err != nil {
		return nil, err
	}
	deviceEndpoint := as.DeviceAuthorizationEndpoint
	if deviceEndpoint == "" {
		return nil, errors.New("login --device: the authorization server advertises no device_authorization_endpoint")
	}
	redirectURI, err := loopbackRedirectURI()
	if err != nil {
		return nil, fmt.Errorf("login --device: %w", err)
	}
	clientID, err := c.registerClient(ctx, as.RegistrationEndpoint, redirectURI)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceEndpoint,
		strings.NewReader(url.Values{"client_id": {clientID}, "resource": {base + "/api"}}.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device authorization: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var dev struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int64  `json:"expires_in"`
		Interval                int64  `json:"interval"`
		Error                   string `json:"error"`
		Description             string `json:"error_description"`
	}
	if err := json.Unmarshal(b, &dev); err != nil || dev.DeviceCode == "" {
		if dev.Error != "" {
			return nil, fmt.Errorf("device authorization: %s (%s)", dev.Error, dev.Description)
		}
		return nil, fmt.Errorf("device authorization: %s: %s", statusText(resp.StatusCode), strings.TrimSpace(string(b)))
	}
	interval := time.Duration(dev.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	expiry := time.Now().Add(time.Duration(dev.ExpiresIn) * time.Second)
	if dev.ExpiresIn <= 0 {
		expiry = time.Now().Add(10 * time.Minute)
	}

	fmt.Fprintln(c.stdout, "Sign in from any device:")
	fmt.Fprintf(c.stdout, "\n  %s\n\n  code: %s\n\n", dev.VerificationURI, displayUserCode(dev.UserCode))
	if dev.VerificationURIComplete != "" {
		fmt.Fprintf(c.stdout, "(or open %s and approve)\n\n", dev.VerificationURIComplete)
	}

	poll := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {dev.DeviceCode},
		"client_id":   {clientID},
	}
	for {
		if time.Now().After(expiry) {
			return nil, errors.New("login --device: the code expired before anyone approved it")
		}
		if err := c.sleep(ctx, interval); err != nil {
			return nil, fmt.Errorf("login --device: %w", err)
		}
		tok, oerr, err := c.postTokenRaw(ctx, as.TokenEndpoint, poll)
		if err != nil {
			return nil, err
		}
		if oerr == nil {
			creds := &credentials{BaseURL: base, ClientID: clientID}
			creds.apply(tok, c.now())
			return creds, nil
		}
		switch oerr.Code {
		case "authorization_pending":
			// The human has not decided yet; keep polling at the advertised interval.
		case "slow_down":
			interval += time.Second
		case "access_denied":
			return nil, errors.New("login denied: the operator denied the request")
		case "expired_token":
			return nil, errors.New("login --device: the code expired before anyone approved it")
		default:
			return nil, oerr
		}
	}
}

// loopbackRedirectURI mints an ephemeral loopback redirect URI for registration without keeping a
// listener open — the device flow never receives a callback, but DCR requires a registered URI and
// the same client may later do a loopback login.
func loopbackRedirectURI() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		return "", err
	}
	return fmt.Sprintf("http://127.0.0.1:%d/callback", port), nil
}

// displayUserCode renders the user code the way the consent screen shows it: XXXX-XXXX.
func displayUserCode(code string) string {
	if len(code) <= 4 {
		return code
	}
	return code[:4] + "-" + code[4:]
}

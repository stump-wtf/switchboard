package main

// Credentials And The API Client
//
// The credentials file is what login persists and every verb consumes: the deployment's base URL,
// the registered client id, and the access/refresh pair (plaintext here, hashed on the server).
// It lives under the user's config directory, mode 0600, and $SWITCHBOARD_CREDENTIALS relocates
// it (several identities on one machine, or a test).
//
// The API client attaches the bearer to every /api/v1 call and keeps the pair alive on its own:
// a token about to expire is refreshed before the call, and a 401 refreshes once and retries.
// A refresh the server refuses means the grant is gone, and the operator logs in again.
//
// @joestump-agent 09/03/2026 - Expiry is a time.Time, the file path is injectable, logout removes
// the file, and every HTTP call goes through the cli's timeout-bounded client.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// credentials is the persisted login state.
type credentials struct {
	BaseURL      string    `json:"base_url"`
	ClientID     string    `json:"client_id"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// apply records a freshly minted pair.
func (c *credentials) apply(tok *tokenResponse, now time.Time) {
	c.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		c.RefreshToken = tok.RefreshToken
	}
	c.ExpiresAt = time.Time{}
	if tok.ExpiresIn > 0 {
		c.ExpiresAt = now.Add(time.Duration(tok.ExpiresIn) * time.Second).UTC()
	}
}

// credentialsPath is $SWITCHBOARD_CREDENTIALS, else <user config dir>/switchboard/credentials.json
// (~/.config on Linux, ~/Library/Application Support on macOS, %AppData% on Windows).
func (c *cli) credentialsPath() (string, error) {
	if p := c.getenv("SWITCHBOARD_CREDENTIALS"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locating the config directory: %w", err)
	}
	return filepath.Join(dir, "switchboard", "credentials.json"), nil
}

func (c *cli) loadCredentials() (*credentials, error) {
	p, err := c.credentialsPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if isNotExist(err) {
		return nil, errNotLoggedIn
	}
	if err != nil {
		return nil, fmt.Errorf("reading credentials: %w", err)
	}
	var creds credentials
	if err := json.Unmarshal(b, &creds); err != nil {
		return nil, fmt.Errorf("credentials file %s is corrupt: %w (run `switchboard logout` then log in again)", p, err)
	}
	if creds.BaseURL == "" || creds.AccessToken == "" {
		return nil, errNotLoggedIn
	}
	return &creds, nil
}

func (c *cli) saveCredentials(creds *credentials) error {
	p, err := c.credentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("creating the credentials directory: %w", err)
	}
	b, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename so a crash mid-write can never leave a half-written (or world-readable)
	// credentials file behind.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing credentials: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("writing credentials: %w", err)
	}
	return nil
}

func (c *cli) removeCredentials() error {
	p, err := c.credentialsPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !isNotExist(err) {
		return fmt.Errorf("removing credentials: %w", err)
	}
	return nil
}

// --- API client ---

// apiClient calls /api/v1 with the stored bearer, refreshing the pair before an imminent expiry
// and once more on a 401 before giving up.
type apiClient struct {
	c     *cli
	creds *credentials
	ctx   context.Context
}

func (c *cli) apiClient(ctx context.Context) (*apiClient, error) {
	creds, err := c.loadCredentials()
	if err != nil {
		return nil, err
	}
	return &apiClient{c: c, creds: creds, ctx: ctx}, nil
}

// refreshLeeway is how close to expiry the client refreshes proactively rather than spending a
// round trip on a 401.
const refreshLeeway = 30 * time.Second

func (a *apiClient) do(method, path string, body any) (*http.Response, error) {
	if !a.creds.ExpiresAt.IsZero() && !a.c.now().Add(refreshLeeway).Before(a.creds.ExpiresAt) {
		if err := a.refresh(); err != nil {
			return nil, err
		}
	}
	resp, err := a.call(method, path, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if err := a.refresh(); err != nil {
			return nil, err
		}
		return a.call(method, path, body)
	}
	return resp, nil
}

func (a *apiClient) call(method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(a.ctx, method, a.creds.BaseURL+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.creds.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "switchboard-cli/"+version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	return resp, nil
}

// refresh rotates the stored pair against the deployment's token endpoint and persists it.
func (a *apiClient) refresh() error {
	if a.creds.RefreshToken == "" {
		return fmt.Errorf("the access token has expired and no refresh token is stored — run `switchboard login` again")
	}
	as, err := a.c.discover(a.ctx, a.creds.BaseURL)
	if err != nil {
		return err
	}
	tok, err := a.c.postToken(a.ctx, as.TokenEndpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {a.creds.RefreshToken},
		"client_id":     {a.creds.ClientID},
	})
	if err != nil {
		return fmt.Errorf("refreshing credentials: %w — run `switchboard login` again", err)
	}
	a.creds.apply(tok, a.c.now())
	return a.c.saveCredentials(a.creds)
}

// get performs a GET and returns the response body, or an error carrying the status and the
// server's own message for anything but 200.
func (a *apiClient) get(path string) ([]byte, error) {
	resp, err := a.do(http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	return readAPIResponse(resp, http.MethodGet, path, http.StatusOK)
}

// post performs a JSON POST and returns the response body, accepting 200 or 201.
func (a *apiClient) post(path string, body any) ([]byte, error) {
	resp, err := a.do(http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	return readAPIResponse(resp, http.MethodPost, path, http.StatusOK, http.StatusCreated)
}

func readAPIResponse(resp *http.Response, method, path string, want ...int) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s %s: reading response: %w", method, path, err)
	}
	for _, code := range want {
		if resp.StatusCode == code {
			return b, nil
		}
	}
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		return nil, fmt.Errorf("%s %s: %s", method, path, statusText(resp.StatusCode))
	}
	return nil, fmt.Errorf("%s %s: %s: %s", method, path, statusText(resp.StatusCode), msg)
}

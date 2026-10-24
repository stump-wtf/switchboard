// Package provider holds read-only clients that list recently-updated issues
// and pull requests from the providers switchboard fronts, for an
// endpoint-scoped caller that wants to diff what the provider holds against
// what this server recorded.
//
// These are plain read clients, not ingestion. Migration 0021 removed the
// provider registry and its pull adapters on purpose (ADR-0022: todo
// ownership is endpoint-scoped), and nothing here revives instance-wide pull
// ingestion: the package imports neither store nor ingest, wires to nothing,
// and owns no credentials — a caller that authenticates does so with an
// http.Client whose Transport injects its own token, which this package never
// reads, stores, or logs. Whether a delivery synthesized from a list result
// is trusted is a connection-config question (#614), not one this package
// answers.
//
// Every implementation is testable against an httptest server: the base URL
// is a constructor argument, and no default is dialed by a test.
//
// Governing: stump.wtf/switchboard#613 (part B of the epic #587 split).
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Pagination and response-size defaults shared by the implementations. The
// page-size cap matches the smallest per_page/limit ceiling the supported
// provider APIs accept.
const (
	defaultPageSize = 50
	maxPageSize     = 100

	maxBodyBytes    = 8 << 20
	maxMessageBytes = 4 << 10
	maxMessageRunes = 200
)

// Kind classifies a listed item as an issue or a pull request.
type Kind string

const (
	KindIssue       Kind = "issue"
	KindPullRequest Kind = "pull_request"
)

// Repo names a repository on a provider. Owner and Name must be bare path
// segments: they are escaped into API paths, and anything that could escape
// one is refused before a request is built.
type Repo struct {
	Owner string
	Name  string
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

func (r Repo) validate() error {
	if r.Owner == "" || r.Name == "" {
		return fmt.Errorf("provider: repo %q: owner and name are required", r)
	}
	for _, part := range []string{r.Owner, r.Name} {
		if strings.ContainsAny(part, `/\?#`) || part == "." || part == ".." {
			return fmt.Errorf("provider: repo %q: owner and name must be bare path segments", r)
		}
	}
	return nil
}

// WorkItem is one issue or pull request as the provider listed it. State is
// the provider's own value verbatim ("open", "closed", …).
type WorkItem struct {
	Kind      Kind
	Number    int
	Title     string
	State     string
	Draft     bool
	HTMLURL   string
	UpdatedAt time.Time
}

// Page is one page of recently-updated items, newest first. NextCursor is
// "" once the walk is done; otherwise it is an opaque value the caller hands
// back to the same implementation on the same repository and with the same
// since. Callers construct cursors by no other means.
type Page struct {
	Items      []WorkItem
	NextCursor string
}

// Reader reads recently-updated issues and pull requests from one provider.
// since time.Time zero lists without a date floor; a non-zero since keeps
// only items updated at or after it, and a page whose oldest item is older
// than since ends the walk (pages are ordered newest first). cursor is ""
// for the first page.
type Reader interface {
	ListRecentlyUpdated(ctx context.Context, repo Repo, since time.Time, cursor string) (*Page, error)
}

// The three error paths a reconcile caller branches on come back as a
// *StatusError that errors.Is-matches these sentinels; other non-2xx answers
// match none of them but still carry a *StatusError.
var (
	ErrForbidden   = errors.New("provider: forbidden")
	ErrNotFound    = errors.New("provider: not found")
	ErrRateLimited = errors.New("provider: rate limited")

	// ErrInvalidCursor reports a cursor that did not come from this client
	// (it decoded badly, or points at another origin than the configured
	// base, or carries credentials).
	ErrInvalidCursor = errors.New("provider: invalid cursor")
)

// StatusError is a non-2xx answer from the provider. RetryAfter is set from a
// 429's Retry-After header when the provider sent one.
type StatusError struct {
	Provider   string
	Op         string
	StatusCode int
	RetryAfter time.Duration
	Message    string
}

func (e *StatusError) Error() string {
	s := fmt.Sprintf("provider/%s: %s: http %d", e.Provider, e.Op, e.StatusCode)
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Is lets errors.Is(err, ErrForbidden|ErrNotFound|ErrRateLimited) branch on
// the status without the caller inspecting the code.
func (e *StatusError) Is(target error) bool {
	switch target {
	case ErrForbidden:
		return e.StatusCode == http.StatusForbidden
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound
	case ErrRateLimited:
		return e.StatusCode == http.StatusTooManyRequests
	}
	return false
}

// Option configures a provider client.
type Option func(*settings)

type settings struct {
	httpClient *http.Client
	pageSize   int
}

// WithHTTPClient supplies the client used for the calls — the hook for a
// caller whose transport adds its own authorization; nil means
// http.DefaultClient.
func WithHTTPClient(hc *http.Client) Option {
	return func(s *settings) { s.httpClient = hc }
}

// WithPageSize sets the per-page size (clamped to [1, maxPageSize]);
// default defaultPageSize.
func WithPageSize(n int) Option {
	return func(s *settings) { s.pageSize = n }
}

func applyOptions(opts []Option) settings {
	s := settings{httpClient: http.DefaultClient, pageSize: defaultPageSize}
	for _, opt := range opts {
		opt(&s)
	}
	if s.httpClient == nil {
		s.httpClient = http.DefaultClient
	}
	switch {
	case s.pageSize <= 0:
		s.pageSize = defaultPageSize
	case s.pageSize > maxPageSize:
		s.pageSize = maxPageSize
	}
	return s
}

// httpBase is the shared HTTP half of an implementation: a validated base
// URL, a caller-supplied http.Client, and the paging size.
type httpBase struct {
	providerName string
	base         *url.URL
	httpClient   *http.Client
	pageSize     int
}

func newHTTPBase(providerName, baseURL string, s settings) (*httpBase, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("provider/%s: base url: %w", providerName, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("provider/%s: base url scheme %q: want http or https", providerName, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("provider/%s: base url %q: host is required", providerName, baseURL)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("provider/%s: base url %q: must not carry credentials, query, or fragment", providerName, baseURL)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return &httpBase{providerName: providerName, base: u, httpClient: s.httpClient, pageSize: s.pageSize}, nil
}

// repoURL builds base + /repos/{owner}/{name}/ + resource with query.
func (b *httpBase) repoURL(op string, repo Repo, resource string, query url.Values) (string, error) {
	if err := repo.validate(); err != nil {
		return "", fmt.Errorf("provider/%s: %s: %w", b.providerName, op, err)
	}
	u := *b.base
	u.Path = b.base.Path + "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/" + resource
	u.RawQuery = query.Encode()
	return u.String(), nil
}

// getJSON GETs a URL this client built or a Link target it resolved, and
// returns the body plus the Link rel="next" target (absolute, same-origin;
// "" when absent).
func (b *httpBase) getJSON(ctx context.Context, op, fullURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("provider/%s: %s: %w", b.providerName, op, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("provider/%s: %s: %w", b.providerName, op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", b.statusError(op, resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("provider/%s: %s: read body: %w", b.providerName, op, err)
	}
	if len(data) > maxBodyBytes {
		return nil, "", fmt.Errorf("provider/%s: %s: response body exceeds %d bytes", b.providerName, op, maxBodyBytes)
	}
	return data, parseLinkNext(resp.Header.Get("Link"), b.base), nil
}

func (b *httpBase) statusError(op string, resp *http.Response) error {
	e := &StatusError{Provider: b.providerName, Op: op, StatusCode: resp.StatusCode}
	if resp.StatusCode == http.StatusTooManyRequests {
		if v := resp.Header.Get("Retry-After"); v != "" {
			if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
				e.RetryAfter = time.Duration(secs) * time.Second
			}
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMessageBytes))
	if err == nil {
		var body struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &body) == nil {
			e.Message = firstLine(truncate(body.Message, maxMessageRunes))
		}
	}
	return e
}

// resolveCursor turns a GitHub-style cursor (a next-page URL this client
// once handed out) back into an absolute URL, refusing anything that points
// outside the configured base.
func (b *httpBase) resolveCursor(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("provider/%s: %w: %v", b.providerName, ErrInvalidCursor, err)
	}
	if !u.IsAbs() {
		u = b.base.ResolveReference(u)
	}
	if u.Scheme != b.base.Scheme || u.Host != b.base.Host || u.User != nil || u.Fragment != "" {
		return "", fmt.Errorf("provider/%s: %w: cursor points outside the configured provider", b.providerName, ErrInvalidCursor)
	}
	return u.String(), nil
}

// parseLinkNext extracts the rel="next" target of an RFC 5988 Link header,
// resolved against base and kept only when same-origin; "" when absent or
// unresolvable.
func parseLinkNext(header string, base *url.URL) string {
	for _, part := range strings.Split(header, ",") {
		segments := strings.Split(part, ";")
		target := strings.TrimSpace(segments[0])
		if len(target) < 2 || target[0] != '<' || target[len(target)-1] != '>' {
			continue
		}
		isNext := false
		for _, param := range segments[1:] {
			name, value, ok := strings.Cut(strings.TrimSpace(param), "=")
			if !ok {
				continue
			}
			name = strings.ToLower(strings.TrimSpace(name))
			value = strings.Trim(strings.TrimSpace(value), `"`)
			if name == "rel" && strings.EqualFold(value, "next") {
				isNext = true
			}
		}
		if !isNext {
			continue
		}
		u, err := url.Parse(target[1 : len(target)-1])
		if err != nil {
			continue
		}
		u = base.ResolveReference(u)
		if u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil {
			continue
		}
		return u.String()
	}
	return ""
}

func truncate(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// pickTime falls back to created when a provider omitted updated_at.
func pickTime(updated, created time.Time) time.Time {
	if updated.IsZero() {
		return created
	}
	return updated
}

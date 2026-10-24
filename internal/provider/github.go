package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

// DefaultGitHubBase is GitHub's public API root; a GitHub Enterprise Server
// caller passes its own https://{host} (or {host}/api/v3) base instead.
const DefaultGitHubBase = "https://api.github.com"

// GitHub is a read-only GitHub client. It authenticates only through the
// http.Client it was given (see WithHTTPClient); it never reads a token of
// its own.
type GitHub struct {
	b *httpBase
}

// NewGitHub builds a GitHub read client against baseURL (httptest servers
// pass their own URL).
func NewGitHub(baseURL string, opts ...Option) (*GitHub, error) {
	b, err := newHTTPBase("github", baseURL, applyOptions(opts))
	if err != nil {
		return nil, err
	}
	return &GitHub{b: b}, nil
}

var _ Reader = (*GitHub)(nil)

type githubIssue struct {
	Number      int             `json:"number"`
	Title       string          `json:"title"`
	State       string          `json:"state"`
	Draft       bool            `json:"draft"`
	HTMLURL     string          `json:"html_url"`
	UpdatedAt   time.Time       `json:"updated_at"`
	CreatedAt   time.Time       `json:"created_at"`
	PullRequest json.RawMessage `json:"pull_request"`
}

// ListRecentlyUpdated walks GitHub's issue listing for repo — which carries
// pull requests too, distinguished by their pull_request field — sorted
// newest-updated first. cursor is "" for the first page, then Page.NextCursor.
func (g *GitHub) ListRecentlyUpdated(ctx context.Context, repo Repo, since time.Time, cursor string) (*Page, error) {
	op := "list recently-updated for " + repo.String()
	var endpoint string
	if cursor != "" {
		resolved, err := g.b.resolveCursor(cursor)
		if err != nil {
			return nil, fmt.Errorf("provider/github: %s: %w", op, err)
		}
		endpoint = resolved
	} else {
		query := url.Values{
			"state":     {"all"},
			"sort":      {"updated"},
			"direction": {"desc"},
			"per_page":  {fmt.Sprint(g.b.pageSize)},
		}
		if !since.IsZero() {
			query.Set("since", since.UTC().Format(time.RFC3339))
		}
		built, err := g.b.repoURL(op, repo, "issues", query)
		if err != nil {
			return nil, err
		}
		endpoint = built
	}
	data, next, err := g.b.getJSON(ctx, op, endpoint)
	if err != nil {
		return nil, err
	}
	var raw []githubIssue
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("provider/github: %s: decode issue page: %w", op, err)
	}
	page := &Page{NextCursor: next}
	for _, item := range raw {
		updated := pickTime(item.UpdatedAt, item.CreatedAt)
		if !since.IsZero() && updated.Before(since) {
			// Newest-first ordering: this page already reaches past
			// since, so no later page can hold a kept item.
			page.NextCursor = ""
			break
		}
		kind := KindIssue
		if len(item.PullRequest) > 0 && string(item.PullRequest) != "null" {
			kind = KindPullRequest
		}
		page.Items = append(page.Items, WorkItem{
			Kind:      kind,
			Number:    item.Number,
			Title:     item.Title,
			State:     item.State,
			Draft:     kind == KindPullRequest && item.Draft,
			HTMLURL:   item.HTMLURL,
			UpdatedAt: updated,
		})
	}
	return page, nil
}

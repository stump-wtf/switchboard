package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// Gitea is a read-only Gitea client. Gitea is self-hosted, so there is no
// default base: the caller passes its own API root, https://{host}/api/v1
// for a stock install (an httptest server's URL + "/api/v1" in tests). Like
// GitHub, it authenticates only through the http.Client it was given.
type Gitea struct {
	b *httpBase
}

// NewGitea builds a Gitea read client against baseURL.
func NewGitea(baseURL string, opts ...Option) (*Gitea, error) {
	b, err := newHTTPBase("gitea", baseURL, applyOptions(opts))
	if err != nil {
		return nil, err
	}
	return &Gitea{b: b}, nil
}

var _ Reader = (*Gitea)(nil)

type giteaIssue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	HTMLURL   string    `json:"html_url"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedAt time.Time `json:"created_at"`
	IsPull    bool      `json:"is_pull"`
}

type giteaPull struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	Draft     bool      `json:"draft"`
	HTMLURL   string    `json:"html_url"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedAt time.Time `json:"created_at"`
}

// giteaCursor is the walk state of the two streams Gitea must page in
// parallel: Gitea lists issues and pulls from separate endpoints, so one
// cursor tracks the next page of each, 0 meaning that stream is spent.
type giteaCursor struct {
	Issues int `json:"i,omitempty"`
	Pulls  int `json:"p,omitempty"`
}

func encodeGiteaCursor(c giteaCursor) string {
	raw, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeGiteaCursor(s string) (giteaCursor, error) {
	var c giteaCursor
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	if c.Issues < 0 || c.Pulls < 0 {
		return c, fmt.Errorf("%w: negative page", ErrInvalidCursor)
	}
	return c, nil
}

// ListRecentlyUpdated walks Gitea's issue and pull listings for repo,
// newest-updated first, merging the two streams into one page. cursor is ""
// for the first page, then Page.NextCursor.
func (g *Gitea) ListRecentlyUpdated(ctx context.Context, repo Repo, since time.Time, cursor string) (*Page, error) {
	op := "list recently-updated for " + repo.String()
	nextIssues, nextPulls := 1, 1
	if cursor != "" {
		c, err := decodeGiteaCursor(cursor)
		if err != nil {
			return nil, fmt.Errorf("provider/gitea: %s: %w", op, err)
		}
		nextIssues, nextPulls = c.Issues, c.Pulls
		if nextIssues == 0 && nextPulls == 0 {
			return nil, fmt.Errorf("provider/gitea: %s: %w: cursor has no stream left to walk", op, ErrInvalidCursor)
		}
	}
	page := &Page{}
	if nextIssues > 0 {
		items, more, err := g.fetchIssues(ctx, op, repo, since, nextIssues)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, items...)
		if !more {
			nextIssues = 0
		} else {
			nextIssues++
		}
	}
	if nextPulls > 0 {
		items, more, err := g.fetchPulls(ctx, op, repo, since, nextPulls)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, items...)
		if !more {
			nextPulls = 0
		} else {
			nextPulls++
		}
	}
	sort.SliceStable(page.Items, func(i, j int) bool {
		return page.Items[i].UpdatedAt.After(page.Items[j].UpdatedAt)
	})
	if nextIssues > 0 || nextPulls > 0 {
		page.NextCursor = encodeGiteaCursor(giteaCursor{Issues: nextIssues, Pulls: nextPulls})
	}
	return page, nil
}

func (g *Gitea) fetchIssues(ctx context.Context, op string, repo Repo, since time.Time, page int) ([]WorkItem, bool, error) {
	query := giteaPageQuery(g.b.pageSize, page)
	if !since.IsZero() {
		query.Set("since", since.UTC().Format(time.RFC3339))
	}
	endpoint, err := g.b.repoURL(op, repo, "issues", query)
	if err != nil {
		return nil, false, err
	}
	data, _, err := g.b.getJSON(ctx, op, endpoint)
	if err != nil {
		return nil, false, err
	}
	var raw []giteaIssue
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, false, fmt.Errorf("provider/gitea: %s: decode issue page: %w", op, err)
	}
	items := make([]WorkItem, 0, len(raw))
	reachedPastSince := false
	for _, issue := range raw {
		if issue.IsPull {
			// Gitea rows are issues under the hood; the pull
			// stream is the one source for PRs, so a PR-shaped
			// row here is the duplicate and is skipped.
			continue
		}
		updated := pickTime(issue.UpdatedAt, issue.CreatedAt)
		if !since.IsZero() && updated.Before(since) {
			reachedPastSince = true
			continue
		}
		items = append(items, WorkItem{
			Kind:      KindIssue,
			Number:    issue.Number,
			Title:     issue.Title,
			State:     issue.State,
			HTMLURL:   issue.HTMLURL,
			UpdatedAt: updated,
		})
	}
	return items, len(raw) >= g.b.pageSize && !reachedPastSince, nil
}

func (g *Gitea) fetchPulls(ctx context.Context, op string, repo Repo, since time.Time, page int) ([]WorkItem, bool, error) {
	endpoint, err := g.b.repoURL(op, repo, "pulls", giteaPageQuery(g.b.pageSize, page))
	if err != nil {
		return nil, false, err
	}
	data, _, err := g.b.getJSON(ctx, op, endpoint)
	if err != nil {
		return nil, false, err
	}
	var raw []giteaPull
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, false, fmt.Errorf("provider/gitea: %s: decode pull page: %w", op, err)
	}
	items := make([]WorkItem, 0, len(raw))
	reachedPastSince := false
	for _, pull := range raw {
		updated := pickTime(pull.UpdatedAt, pull.CreatedAt)
		if !since.IsZero() && updated.Before(since) {
			reachedPastSince = true
			continue
		}
		items = append(items, WorkItem{
			Kind:      KindPullRequest,
			Number:    pull.Number,
			Title:     pull.Title,
			State:     pull.State,
			Draft:     pull.Draft,
			HTMLURL:   pull.HTMLURL,
			UpdatedAt: updated,
		})
	}
	return items, len(raw) >= g.b.pageSize && !reachedPastSince, nil
}

func giteaPageQuery(pageSize, page int) url.Values {
	return url.Values{
		"state": {"all"},
		"sort":  {"recentupdate"},
		"limit": {strconv.Itoa(pageSize)},
		"page":  {strconv.Itoa(page)},
	}
}

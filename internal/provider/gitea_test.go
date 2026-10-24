package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mustGitea(t *testing.T, srv *httptest.Server, opts ...Option) *Gitea {
	t.Helper()
	g, err := NewGitea(srv.URL+"/api/v1", opts...)
	if err != nil {
		t.Fatalf("NewGitea(%s/api/v1): %v", srv.URL, err)
	}
	return g
}

func giteaItem(number int, title, state, updated string, extra ...string) string {
	fields := append([]string{
		fmt.Sprintf("%q: %d", "number", number),
		fmt.Sprintf("%q: %q", "title", title),
		fmt.Sprintf("%q: %q", "state", state),
		fmt.Sprintf("%q: %q", "html_url", fmt.Sprintf("https://git.test/octo/repo/issues/%d", number)),
		fmt.Sprintf("%q: %q", "updated_at", updated),
	}, extra...)
	return "{" + strings.Join(fields, ",") + "}"
}

func TestGiteaMergesStreamsAndPages(t *testing.T) {
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, strings.TrimPrefix(r.URL.Path, "/api/v1/repos/octo/repo/")+":"+r.URL.Query().Get("page"))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues") && r.URL.Query().Get("page") == "1":
			// A PR-shaped row must be skipped: the pulls stream owns PRs.
			_, _ = w.Write([]byte("[" + strings.Join([]string{
				giteaItem(10, "issue ten", "open", "2026-10-05T00:00:00Z"),
				giteaItem(9, "pr nine", "open", "2026-10-04T00:00:00Z", `"is_pull": true`),
				giteaItem(11, "issue eleven", "closed", "2026-10-03T00:00:00Z"),
			}, ",") + "]"))
		case strings.HasSuffix(r.URL.Path, "/issues"): // page 2: short page ends the stream
			_, _ = w.Write([]byte("[" + giteaItem(12, "issue twelve", "open", "2026-10-01T00:00:00Z") + "]"))
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			_, _ = w.Write([]byte("[" + giteaItem(9, "pr nine", "open", "2026-10-06T00:00:00Z", `"draft": true`) + "]"))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	g := mustGitea(t, srv, WithPageSize(2))

	first, err := g.ListRecentlyUpdated(context.Background(), Repo{Owner: "octo", Name: "repo"}, time.Time{}, "")
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	wantFirst := []WorkItem{
		{Kind: KindPullRequest, Number: 9, Title: "pr nine", State: "open", Draft: true, HTMLURL: "https://git.test/octo/repo/issues/9", UpdatedAt: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)},
		{Kind: KindIssue, Number: 10, Title: "issue ten", State: "open", HTMLURL: "https://git.test/octo/repo/issues/10", UpdatedAt: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)},
		{Kind: KindIssue, Number: 11, Title: "issue eleven", State: "closed", HTMLURL: "https://git.test/octo/repo/issues/11", UpdatedAt: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)},
	}
	if len(first.Items) != len(wantFirst) {
		t.Fatalf("first items = %+v, want %d items", first.Items, len(wantFirst))
	}
	for i := range wantFirst {
		if first.Items[i] != wantFirst[i] {
			t.Errorf("first item %d = %+v, want %+v", i, first.Items[i], wantFirst[i])
		}
	}
	if first.NextCursor == "" {
		t.Fatalf("NextCursor empty, want the issues page-2 cursor")
	}
	decoded, err := decodeGiteaCursor(first.NextCursor)
	if err != nil {
		t.Fatalf("decode cursor %q: %v", first.NextCursor, err)
	}
	if decoded.Issues != 2 || decoded.Pulls != 0 {
		t.Errorf("cursor = %+v, want issues page 2 and pulls spent", decoded)
	}

	second, err := g.ListRecentlyUpdated(context.Background(), Repo{Owner: "octo", Name: "repo"}, time.Time{}, first.NextCursor)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if second.NextCursor != "" {
		t.Errorf("second NextCursor = %q, want empty", second.NextCursor)
	}
	if len(second.Items) != 1 || second.Items[0].Number != 12 {
		t.Errorf("second items = %+v, want just #12", second.Items)
	}
	wantRequests := []string{"issues:1", "pulls:1", "issues:2"}
	if len(requests) != len(wantRequests) {
		t.Fatalf("requests = %v, want %v", requests, wantRequests)
	}
	for i := range wantRequests {
		if requests[i] != wantRequests[i] {
			t.Errorf("request %d = %q, want %q", i, requests[i], wantRequests[i])
		}
	}
}

func TestGiteaErrorPaths(t *testing.T) {
	for _, tc := range []struct {
		name     string
		path     string // which stream answers the status
		status   int
		sentinel error
		retry    time.Duration
	}{
		{name: "403 on issues", path: "issues", status: 403, sentinel: ErrForbidden},
		{name: "404 on pulls", path: "pulls", status: 404, sentinel: ErrNotFound},
		{name: "429 on issues", path: "issues", status: 429, sentinel: ErrRateLimited, retry: 7 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/"+tc.path) {
					if tc.retry > 0 {
						w.Header().Set("Retry-After", "7")
					}
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(`{"message":"provider said no"}`))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
			}))
			t.Cleanup(srv.Close)
			_, err := mustGitea(t, srv).ListRecentlyUpdated(context.Background(), Repo{Owner: "octo", Name: "repo"}, time.Time{}, "")
			if err == nil {
				t.Fatalf("want error, got nil")
			}
			if !errors.Is(err, tc.sentinel) {
				t.Errorf("errors.Is(err, %v) = false for %v", tc.sentinel, err)
			}
			var statusErr *StatusError
			if !errors.As(err, &statusErr) {
				t.Fatalf("err = %v (%T), want *StatusError", err, err)
			}
			if statusErr.Provider != "gitea" || statusErr.StatusCode != tc.status {
				t.Errorf("provider = %q status = %d, want gitea %d", statusErr.Provider, statusErr.StatusCode, tc.status)
			}
			if statusErr.RetryAfter != tc.retry {
				t.Errorf("RetryAfter = %v, want %v", statusErr.RetryAfter, tc.retry)
			}
		})
	}
}

func TestGiteaSinceEndsPastStreams(t *testing.T) {
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, strings.TrimPrefix(r.URL.Path, "/api/v1/repos/octo/repo/"))
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/issues") {
			_, _ = w.Write([]byte("[" + strings.Join([]string{
				giteaItem(10, "kept", "open", "2026-10-05T00:00:00Z"),
				giteaItem(11, "stale", "open", "2026-10-01T00:00:00Z"),
				giteaItem(12, "stale two", "open", "2026-10-01T00:00:00Z"),
			}, ",") + "]"))
			return
		}
		_, _ = w.Write([]byte("[" + strings.Join([]string{
			giteaItem(20, "pr kept", "open", "2026-10-06T00:00:00Z"),
			giteaItem(21, "pr stale", "open", "2026-10-01T00:00:00Z"),
			giteaItem(22, "pr stale two", "open", "2026-10-01T00:00:00Z"),
		}, ",") + "]"))
	}))
	t.Cleanup(srv.Close)

	since := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	page, err := mustGitea(t, srv, WithPageSize(3)).ListRecentlyUpdated(context.Background(), Repo{Owner: "octo", Name: "repo"}, since, "")
	if err != nil {
		t.Fatalf("ListRecentlyUpdated: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items = %+v, want the two kept items", page.Items)
	}
	if page.Items[0].Number != 20 || page.Items[1].Number != 10 {
		t.Errorf("items order = %+v, want #20 then #10", page.Items)
	}
	if page.NextCursor != "" {
		t.Errorf("NextCursor = %q, want empty: both streams reached past since", page.NextCursor)
	}
	if len(requests) != 2 {
		t.Errorf("requests = %v, want one per stream", requests)
	}
}

func TestGiteaCursorValidation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request expected, got %s", r.URL)
	}))
	t.Cleanup(srv.Close)
	g := mustGitea(t, srv)
	if _, err := g.ListRecentlyUpdated(context.Background(), Repo{Owner: "octo", Name: "repo"}, time.Time{}, "not base64!"); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("garbage cursor err = %v, want ErrInvalidCursor", err)
	}
	empty := base64.RawURLEncoding.EncodeToString(func() []byte {
		raw, _ := json.Marshal(giteaCursor{})
		return raw
	}())
	if _, err := g.ListRecentlyUpdated(context.Background(), Repo{Owner: "octo", Name: "repo"}, time.Time{}, empty); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("spent cursor err = %v, want ErrInvalidCursor", err)
	}
}

func TestGiteaPaginationQueryShape(t *testing.T) {
	var got map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/issues") {
			got = r.URL.Query()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	since := time.Date(2026, 10, 4, 12, 0, 0, 0, time.FixedZone("UTC-4", -4*3600))
	if _, err := mustGitea(t, srv).ListRecentlyUpdated(context.Background(), Repo{Owner: "octo", Name: "repo"}, since, ""); err != nil {
		t.Fatalf("ListRecentlyUpdated: %v", err)
	}
	for key, want := range map[string]string{"state": "all", "sort": "recentupdate", "limit": "50", "page": "1", "since": "2026-10-04T16:00:00Z"} {
		if got := got[key]; len(got) != 1 || got[0] != want {
			t.Errorf("query %s = %v, want [%s]", key, got, want)
		}
	}
}

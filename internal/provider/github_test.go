package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func mustGitHub(t *testing.T, srv *httptest.Server, opts ...Option) *GitHub {
	t.Helper()
	g, err := NewGitHub(srv.URL, opts...)
	if err != nil {
		t.Fatalf("NewGitHub(%s): %v", srv.URL, err)
	}
	return g
}

func jsonHandler(status int, body string, headers map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func TestGitHubListDecodesFieldsAndQuery(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/octo/repo/issues" {
			t.Errorf("path = %q, want /repos/octo/repo/issues", r.URL.Path)
		}
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"number":7,"title":"Bug A","state":"open","html_url":"https://example.test/octo/repo/issues/7","updated_at":"2026-10-03T10:00:00Z"},
			{"number":9,"title":"Draft PR","state":"open","draft":true,"html_url":"https://example.test/octo/repo/pull/9","updated_at":"2026-10-02T10:00:00Z","pull_request":{"url":"https://example.test/repos/octo/repo/pulls/9"}},
			{"number":8,"title":"Old issue","state":"closed","html_url":"https://example.test/octo/repo/issues/8","created_at":"2026-10-01T10:00:00Z"}
		]`))
	}))
	t.Cleanup(srv.Close)

	since := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	page, err := mustGitHub(t, srv).ListRecentlyUpdated(context.Background(), Repo{Owner: "octo", Name: "repo"}, since, "")
	if err != nil {
		t.Fatalf("ListRecentlyUpdated: %v", err)
	}
	want := []WorkItem{
		{Kind: KindIssue, Number: 7, Title: "Bug A", State: "open", HTMLURL: "https://example.test/octo/repo/issues/7", UpdatedAt: time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)},
		{Kind: KindPullRequest, Number: 9, Title: "Draft PR", State: "open", Draft: true, HTMLURL: "https://example.test/octo/repo/pull/9", UpdatedAt: time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)},
		{Kind: KindIssue, Number: 8, Title: "Old issue", State: "closed", HTMLURL: "https://example.test/octo/repo/issues/8", UpdatedAt: time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)},
	}
	if len(page.Items) != len(want) {
		t.Fatalf("items = %d, want %d: %+v", len(page.Items), len(want), page.Items)
	}
	for i := range want {
		if page.Items[i] != want[i] {
			t.Errorf("item %d = %+v, want %+v", i, page.Items[i], want[i])
		}
	}
	if page.NextCursor != "" {
		t.Errorf("NextCursor = %q, want empty without a Link header", page.NextCursor)
	}
	for key, want := range map[string]string{
		"state": "all", "sort": "updated", "direction": "desc", "per_page": "50",
		"since": "2026-09-30T00:00:00Z",
	} {
		if got := gotQuery.Get(key); got != want {
			t.Errorf("query %s = %q, want %q", key, got, want)
		}
	}
}

func TestGitHubPaginationWalk(t *testing.T) {
	var pages []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		w.Header().Set("Content-Type", "application/json")
		if page == "2" {
			_, _ = w.Write([]byte(`[{"number":2,"title":"second","state":"closed","updated_at":"2026-10-01T00:00:00Z"}]`))
			return
		}
		w.Header().Set("Link", `<`+srv.URL+`/repos/octo/repo/issues?page=2>; rel="next", <`+srv.URL+`/repos/octo/repo/issues?page=9>; rel="last"`)
		_, _ = w.Write([]byte(`[{"number":1,"title":"first","state":"open","updated_at":"2026-10-02T00:00:00Z"}]`))
	}))
	t.Cleanup(srv.Close)
	g := mustGitHub(t, srv)

	page1, err := g.ListRecentlyUpdated(context.Background(), Repo{Owner: "octo", Name: "repo"}, time.Time{}, "")
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if page1.NextCursor == "" || !strings.Contains(page1.NextCursor, "page=2") {
		t.Fatalf("NextCursor = %q, want the page=2 link", page1.NextCursor)
	}
	page2, err := g.ListRecentlyUpdated(context.Background(), Repo{Owner: "octo", Name: "repo"}, time.Time{}, page1.NextCursor)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if page2.NextCursor != "" {
		t.Errorf("second NextCursor = %q, want empty", page2.NextCursor)
	}
	if len(page1.Items) != 1 || len(page2.Items) != 1 || page2.Items[0].Number != 2 {
		t.Errorf("unexpected walk: %+v %+v", page1.Items, page2.Items)
	}
	if len(pages) != 2 || pages[0] != "" || pages[1] != "2" {
		t.Errorf("requested pages = %v, want [\"\", \"2\"]", pages)
	}
}

func TestGitHubStopsWalkingAtSince(t *testing.T) {
	requests := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Link", `<`+srv.URL+`/repos/octo/repo/issues?page=2>; rel="next"`)
		_, _ = w.Write([]byte(`[
			{"number":1,"title":"kept","state":"open","updated_at":"2026-10-05T00:00:00Z"},
			{"number":2,"title":"older","state":"open","updated_at":"2026-10-01T00:00:00Z"}
		]`))
	}))
	t.Cleanup(srv.Close)

	since := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	page, err := mustGitHub(t, srv).ListRecentlyUpdated(context.Background(), Repo{Owner: "octo", Name: "repo"}, since, "")
	if err != nil {
		t.Fatalf("ListRecentlyUpdated: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Number != 1 {
		t.Errorf("items = %+v, want only the kept item", page.Items)
	}
	if page.NextCursor != "" {
		t.Errorf("NextCursor = %q, want empty: the walk reached past since", page.NextCursor)
	}
	if requests != 1 {
		t.Errorf("requests = %d, want 1: page 2 must not be fetched", requests)
	}
}

func TestGitHubErrorPaths(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		sentinel error
		retry    time.Duration
		message  string
	}{
		{name: "403", status: 403, sentinel: ErrForbidden, message: "Forbidden: Resource protected"},
		{name: "404", status: 404, sentinel: ErrNotFound, message: "Not Found"},
		{name: "429", status: 429, sentinel: ErrRateLimited, retry: 7 * time.Second, message: "API rate limit exceeded"},
		{name: "500", status: 500, sentinel: nil, message: "Server Error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{}
			if tc.retry > 0 {
				headers["Retry-After"] = "7"
			}
			srv := httptest.NewServer(jsonHandler(tc.status, `{"message":"`+tc.message+`"}`, headers))
			t.Cleanup(srv.Close)
			_, err := mustGitHub(t, srv).ListRecentlyUpdated(context.Background(), Repo{Owner: "octo", Name: "repo"}, time.Time{}, "")
			if err == nil {
				t.Fatalf("want error, got nil")
			}
			var statusErr *StatusError
			if !errors.As(err, &statusErr) {
				t.Fatalf("err = %v (%T), want *StatusError", err, err)
			}
			if statusErr.StatusCode != tc.status || statusErr.Provider != "github" {
				t.Errorf("status = %d provider = %q, want %d github", statusErr.StatusCode, statusErr.Provider, tc.status)
			}
			if statusErr.Message != tc.message {
				t.Errorf("Message = %q, want %q", statusErr.Message, tc.message)
			}
			if tc.sentinel == nil {
				if errors.Is(err, ErrForbidden) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrRateLimited) {
					t.Errorf("a %d must match no branch sentinel", tc.status)
				}
			} else if !errors.Is(err, tc.sentinel) {
				t.Errorf("errors.Is(err, %v) = false for %v", tc.sentinel, err)
			}
			if statusErr.RetryAfter != tc.retry {
				t.Errorf("RetryAfter = %v, want %v", statusErr.RetryAfter, tc.retry)
			}
		})
	}
}

func TestGitHubRejectsForeignCursor(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	t.Cleanup(srv.Close)
	_, err := mustGitHub(t, srv).ListRecentlyUpdated(context.Background(), Repo{Owner: "octo", Name: "repo"}, time.Time{}, "https://elsewhere.test/repos/octo/repo/issues?page=2")
	if !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("err = %v, want ErrInvalidCursor", err)
	}
	if requests != 0 {
		t.Errorf("requests = %d, want 0", requests)
	}
}

func TestGitHubRepoValidation(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	t.Cleanup(srv.Close)
	g := mustGitHub(t, srv)
	for _, repo := range []Repo{{Owner: "a/b", Name: "repo"}, {Owner: "octo", Name: "../evil"}, {Owner: "", Name: "repo"}} {
		if _, err := g.ListRecentlyUpdated(context.Background(), repo, time.Time{}, ""); err == nil {
			t.Errorf("repo %v: want error, got nil", repo)
		}
	}
	if requests != 0 {
		t.Errorf("requests = %d, want 0", requests)
	}
}

func TestGitHubBaseURLValidation(t *testing.T) {
	for _, bad := range []string{"ftp://api.test", "https://", "https://tok:***@api.test", "https://api.test/x?a=1", "://nope"} {
		if _, err := NewGitHub(bad); err == nil {
			t.Errorf("NewGitHub(%q): want error, got nil", bad)
		}
	}
}

func TestPageSizeClamped(t *testing.T) {
	g, err := NewGitHub("https://api.github.com", WithPageSize(5000))
	if err != nil {
		t.Fatal(err)
	}
	if g.b.pageSize != maxPageSize {
		t.Errorf("pageSize = %d, want %d", g.b.pageSize, maxPageSize)
	}
	if g2, err := NewGitHub("https://api.github.com", WithPageSize(0)); err != nil || g2.b.pageSize != defaultPageSize {
		t.Errorf("zero page size = %d, %v; want %d, nil", g2.b.pageSize, err, defaultPageSize)
	}
}

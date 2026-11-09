package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// dependencyItem builds one blocker row the way Gitea reports it: a full issue object that also names
// the repository the blocker lives on, since dependency links cross repositories.
func dependencyItem(owner, name string, number int, state string) string {
	return fmt.Sprintf(`{"number":%d,"title":"blocker %d","state":%q,`+
		`"html_url":"https://git.test/%s/%s/issues/%d",`+
		`"repository":{"full_name":%q}}`, number, number, state, owner, name, number, owner+"/"+name)
}

func TestGiteaIssueBlockersWalksPagesAndKeepsEachRepo(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, strings.TrimPrefix(r.URL.Path, "/api/v1")+":"+r.URL.Query().Get("page"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "1":
			_, _ = w.Write([]byte("[" + dependencyItem("octo", "repo", 7, "open") + "," +
				dependencyItem("other", "thing", 3, "closed") + "]"))
		case "2":
			_, _ = w.Write([]byte("[" + dependencyItem("octo", "repo", 9, "Open") + "]"))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	got, err := mustGitea(t, srv, WithPageSize(2)).IssueBlockers(context.Background(), Repo{Owner: "octo", Name: "repo"}, 12)
	if err != nil {
		t.Fatalf("IssueBlockers: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d blockers, want 3: %+v", len(got), got)
	}
	want := []string{"octo/repo#7", "other/thing#3", "octo/repo#9"}
	for i, w := range want {
		if got[i].RefString() != w {
			t.Errorf("blocker %d = %q, want %q", i, got[i].RefString(), w)
		}
	}
	if got[1].Repo.Owner != "other" || got[1].Repo.Name != "thing" {
		t.Errorf("cross-repository blocker lost its own repository: %+v", got[1])
	}
	if got[2].State != "closed" && got[2].State != "open" {
		t.Errorf("state %q not normalised to lowercase", got[2].State)
	}
	if got[2].State != "open" {
		t.Errorf("\"Open\" must normalise to \"open\", got %q", got[2].State)
	}
	if got[0].HTMLURL == "" {
		t.Error("blocker lost its html_url, which the drawer links to")
	}
	for i, p := range paths {
		if want := fmt.Sprintf("/repos/octo/repo/issues/12/dependencies:%d", i+1); p != want {
			t.Errorf("request %d = %q, want %q", i, p, want)
		}
	}
}

func TestGiteaIssueBlockersEmptyListIsNoError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)
	got, err := mustGitea(t, srv).IssueBlockers(context.Background(), Repo{Owner: "octo", Name: "repo"}, 4)
	if err != nil {
		t.Fatalf("an issue with no blockers must not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}

func TestGiteaIssueBlockersRefusesUnresolvableRows(t *testing.T) {
	cases := map[string]string{
		"no repository":     `[{"number":5,"state":"open","html_url":"https://git.test/x/y/issues/5"}]`,
		"ownerless ref":     `[{"number":5,"state":"open","repository":{"full_name":"onlyrepo"}}]`,
		"nameless ref":      `[{"number":5,"state":"open","repository":{"full_name":"owner/"}}]`,
		"repo-shaped wrong": `[{"number":5,"state":"open","repository":{"full_name":"a/b/c"}}]`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			if _, err := mustGitea(t, srv).IssueBlockers(context.Background(), Repo{Owner: "octo", Name: "repo"}, 4); err == nil {
				t.Fatal("a blocker the release path could never resolve must be an error, not a silent skip")
			}
		})
	}
}

func TestGiteaIssueBlockersRejectsBadIssueNumber(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a non-positive issue number must not reach the network")
	}))
	t.Cleanup(srv.Close)
	if _, err := mustGitea(t, srv).IssueBlockers(context.Background(), Repo{Owner: "octo", Name: "repo"}, 0); err == nil {
		t.Fatal("IssueBlockers(0) = nil error, want refusal")
	}
}

func TestGiteaIssueBlockersStopsAtThePageCap(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		// A full page every time: the provider is ignoring the page parameter, or the issue really
		// has thousands of blockers. Either way one intake check must not crawl forever.
		_, _ = w.Write([]byte("[" + dependencyItem("octo", "repo", requests, "open") + "]"))
	}))
	t.Cleanup(srv.Close)
	_, err := mustGitea(t, srv, WithPageSize(1)).IssueBlockers(context.Background(), Repo{Owner: "octo", Name: "repo"}, 4)
	if err == nil {
		t.Fatal("an endless blocker stream must end in an error")
	}
	if !strings.Contains(err.Error(), "pages") {
		t.Errorf("error = %v, want it to name the page cap", err)
	}
	if requests != maxDependencyPages {
		t.Errorf("%d requests, want the cap %d", requests, maxDependencyPages)
	}
}

func TestGiteaIssueBlockersReportsHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"not found"}`))
	}))
	t.Cleanup(srv.Close)
	if _, err := mustGitea(t, srv).IssueBlockers(context.Background(), Repo{Owner: "octo", Name: "repo"}, 4); err == nil {
		t.Fatal("a 404 must be an error the caller fails open on, not an empty blocker list")
	}
}

func TestGitHubIssueBlockersIsUnsupported(t *testing.T) {
	gh, err := NewGitHub("https://api.github.com")
	if err != nil {
		t.Fatalf("NewGitHub: %v", err)
	}
	_, err = gh.IssueBlockers(context.Background(), Repo{Owner: "octo", Name: "repo"}, 4)
	if !errors.Is(err, ErrUnsupportedDependencies) {
		t.Errorf("GitHub IssueBlockers err = %v, want ErrUnsupportedDependencies", err)
	}
}

// fakeDependencyReader is a provider that reports a fixed blocker list.
type fakeDependencyReader struct {
	deps []Dependency
	err  error
}

func (f fakeDependencyReader) IssueBlockers(context.Context, Repo, int) ([]Dependency, error) {
	return f.deps, f.err
}

func TestDependenciesOfSkipsSelfLinksAndClosedBlockers(t *testing.T) {
	repo := Repo{Owner: "octo", Name: "repo"}
	reader := fakeDependencyReader{deps: []Dependency{
		{Repo: repo, Number: 12, State: "open"}, // the issue itself: a self-link is not a reason to wait
		{Repo: repo, Number: 7, State: "open"},
		{Repo: Repo{Owner: "other", Name: "thing"}, Number: 3, State: "closed"},
		{Repo: repo, Number: 8, State: ""}, // no state reported: not provably done, so it holds
	}}
	open, all, err := DependenciesOf(context.Background(), reader, repo, 12)
	if err != nil {
		t.Fatalf("DependenciesOf: %v", err)
	}
	if len(all) != 4 {
		t.Errorf("all = %d, want every row the provider reported", len(all))
	}
	var refs []string
	for _, d := range open {
		refs = append(refs, d.RefString())
	}
	if strings.Join(refs, ",") != "octo/repo#7,octo/repo#8" {
		t.Errorf("open = %v, want the self-link and the closed blocker dropped", refs)
	}
}

func TestDependenciesOfDistinguishesSilenceFromAnEmptyList(t *testing.T) {
	repo := Repo{Owner: "octo", Name: "repo"}
	// A client that is not a DependencyReader at all: "this tracker cannot tell me", not "nothing
	// blocks" — a caller that released a hold on the strength of an empty list would be wrong.
	open, all, err := DependenciesOf(context.Background(), struct{}{}, repo, 4)
	if !errors.Is(err, ErrUnsupportedDependencies) || open != nil || all != nil {
		t.Errorf("non-reader = (%v, %v, %v), want ErrUnsupportedDependencies", open, all, err)
	}
	// A client that is one but reports its provider has no dependency model (GitHub).
	open, all, err = DependenciesOf(context.Background(),
		fakeDependencyReader{err: fmt.Errorf("provider/github: %w", ErrUnsupportedDependencies)}, repo, 4)
	if !errors.Is(err, ErrUnsupportedDependencies) || open != nil || all != nil {
		t.Errorf("unsupported = (%v, %v, %v), want ErrUnsupportedDependencies", open, all, err)
	}
	// An issue with no links is the empty-list answer, and it is not an error.
	open, all, err = DependenciesOf(context.Background(), fakeDependencyReader{}, repo, 4)
	if err != nil || len(open) != 0 || len(all) != 0 {
		t.Errorf("no links = (%v, %v, %v), want empty and no error", open, all, err)
	}
	// Any other error must reach the caller, which logs and fails open.
	if _, _, err := DependenciesOf(context.Background(),
		fakeDependencyReader{err: errors.New("boom")}, repo, 4); err == nil {
		t.Error("a real read failure must not be swallowed")
	}
}

func TestDependencyRefStringIsCanonical(t *testing.T) {
	d := Dependency{Repo: Repo{Owner: "stump.wtf", Name: "cairn"}, Number: 41}
	if got := d.RefString(); got != "stump.wtf/cairn#41" {
		t.Errorf("RefString = %q", got)
	}
	var raw json.RawMessage = []byte(`{"number":9,"state":"open","repo":{"owner":"a","name":"b"}}`)
	var back Dependency
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Dependency is not round-trippable: %v", err)
	}
	if back.Repo.Owner != "a" || back.Number != 9 {
		t.Errorf("decoded %+v", back)
	}
}

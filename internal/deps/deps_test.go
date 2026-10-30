package deps

// Unit tests for the dependency checker that need no database and dial nothing: what a ref parses to,
// what a payload says, and every way a read can be unknown — each of which must mean "no hold".
//
// The store-backed halves (release, re-hold, the re-check pass) live with the store tests, which run
// only with SWITCHBOARD_TEST_DATABASE_URL. Governing: ADR-0055 (proposed), stump.wtf/switchboard#583.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/provider"
	"github.com/stump-wtf/switchboard/internal/store"
)

// fakeProvider reports a fixed blocker list and counts how often it was asked.
type fakeProvider struct {
	deps    []provider.Dependency
	err     error
	asked   int
	lastNum int
}

func (f *fakeProvider) IssueBlockers(_ context.Context, repo provider.Repo, number int) ([]provider.Dependency, error) {
	f.asked++
	f.lastNum = number
	return f.deps, f.err
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func deps(owner, name string, number int, state string) provider.Dependency {
	return provider.Dependency{Repo: provider.Repo{Owner: owner, Name: name}, Number: number, State: state,
		HTMLURL: "https://git.test/" + owner + "/" + name + "/issues/" + strconv.Itoa(number)}
}

func TestParseRefRoundTripsTheStoredForm(t *testing.T) {
	for _, s := range []string{"octo/repo#1", "stump.wtf/switchboard#583", "a/b.c-d#12345"} {
		ref, err := ParseRef(s)
		if err != nil {
			t.Fatalf("ParseRef(%q): %v", s, err)
		}
		if ref.RefString() != s {
			t.Errorf("ParseRef(%q).RefString() = %q", s, ref.RefString())
		}
	}
	for _, s := range []string{"", "octo/repo", "octo#1", "repo#1", "octo/repo#0", "octo/repo#x", "octo//1", "octo/repo#-3"} {
		if _, err := ParseRef(s); err == nil {
			t.Errorf("ParseRef(%q) = nil error, want refusal", s)
		}
	}
}

func TestIssueRefFromPayload(t *testing.T) {
	cases := []struct {
		name string
		body string
		want IssueRef
		ok   bool
	}{
		{"issue closed", `{"issue":{"number":12,"state":"closed"},"repository":{"full_name":"octo/repo"}}`,
			IssueRef{Owner: "octo", Repo: "repo", Number: 12, State: "closed"}, true},
		{"issue open", `{"issue":{"number":7,"state":"open"},"repository":{"full_name":"octo/repo"}}`,
			IssueRef{Owner: "octo", Repo: "repo", Number: 7, State: "open"}, true},
		{"owner and name instead of full_name", `{"issue":{"number":3},"repository":{"owner":{"login":"octo"},"name":"repo"}}`,
			IssueRef{Owner: "octo", Repo: "repo", Number: 3}, true},
		{"pull request is not an issue hold", `{"pull_request":{"number":9},"repository":{"full_name":"octo/repo"}}`,
			IssueRef{}, false},
		{"no repository named", `{"issue":{"number":9}}`, IssueRef{}, false},
		{"no issue", `{"repository":{"full_name":"octo/repo"}}`, IssueRef{}, false},
		{"zero number", `{"issue":{"number":0},"repository":{"full_name":"octo/repo"}}`, IssueRef{}, false},
		{"not json", `nope`, IssueRef{}, false},
	}
	for _, c := range cases {
		got, ok := IssueRefFromPayload([]byte(c.body))
		if ok != c.ok {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.ok)
		}
		if ok && got != c.want {
			t.Errorf("%s: ref = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestIssueRefClosedIsStateCaseInsensitive(t *testing.T) {
	if !(IssueRef{State: "Closed"}).Closed() {
		t.Error("\"Closed\" must read as closed")
	}
	if (IssueRef{State: ""}).Closed() || (IssueRef{State: "open"}).Closed() {
		t.Error("an event that does not say closed must not read as closed")
	}
	if (IssueRef{Owner: "a", Repo: "b", Number: 2}).RepoRef() != (provider.Repo{Owner: "a", Name: "b"}) {
		t.Error("RepoRef lost the repository")
	}
}

func TestConfigDefaultsAndInterval(t *testing.T) {
	var c Config
	if c.normalized().ReadTimeout != DefaultReadTimeout || c.normalized().CacheTTL != DefaultCacheTTL ||
		c.normalized().RecheckLimit != DefaultRecheckLimit {
		t.Errorf("zero Config did not take the defaults: %+v", c.normalized())
	}
	if c.RecheckInterval() != DefaultRecheckInterval {
		t.Errorf("RecheckInterval = %v, want %v", c.RecheckInterval(), DefaultRecheckInterval)
	}
	if got := (Config{RecheckInterv: time.Second}).RecheckInterval(); got != time.Second {
		t.Errorf("an explicit interval was ignored: %v", got)
	}
	st := &store.Store{}
	if got := New(st, nil, Config{}, nil).RecheckInterval(); got != DefaultRecheckInterval {
		t.Errorf("Checker.RecheckInterval = %v, want the default", got)
	}
}

func TestCheckerWithoutResolverIsDisabled(t *testing.T) {
	c := New(&store.Store{}, discardLog(), Config{}, nil)
	if c.Enabled() {
		t.Fatal("a Checker with no resolver reports itself enabled")
	}
	open, ok := c.BlockersFor(context.Background(), "wh-1", IssueRef{Owner: "a", Repo: "b", Number: 1})
	if ok || open != nil {
		t.Errorf("disabled checker = (%v, %v), want nothing known", open, ok)
	}
	var nilChecker *Checker
	if nilChecker.Enabled() {
		t.Error("a nil Checker must report disabled, not panic")
	}
	report, err := nilChecker.Recheck(context.Background())
	if err != nil || report.Scanned != 0 {
		t.Errorf("nil checker Recheck = %+v, %v, want an empty report", report, err)
	}
}

func TestBlockersForReportsOpenBlockersOncePerTTL(t *testing.T) {
	fp := &fakeProvider{deps: []provider.Dependency{
		deps("octo", "repo", 7, "open"),
		deps("other", "thing", 3, "closed"),
	}}
	now := time.Unix(1700000000, 0)
	c := New(&store.Store{}, discardLog(), Config{CacheTTL: time.Minute}, func(context.Context, string) (*Client, error) {
		return &Client{Reader: fp}, nil
	})
	c.SetClock(func() time.Time { return now })

	open, ok := c.BlockersFor(context.Background(), "wh-1", IssueRef{Owner: "octo", Repo: "repo", Number: 12})
	if !ok {
		t.Fatal("a successful read reported nothing known")
	}
	if len(open) != 1 || open[0].RefString() != "octo/repo#7" || open[0].State != "open" {
		t.Fatalf("open = %+v, want only octo/repo#7", open)
	}
	if open[0].HTMLURL == "" {
		t.Errorf("recorded ref lost the link an operator would click: %+v", open[0])
	}
	if _, ok := c.BlockersFor(context.Background(), "wh-1", IssueRef{Owner: "octo", Repo: "repo", Number: 12}); !ok {
		t.Fatal("cached read reported nothing known")
	}
	if fp.asked != 1 {
		t.Errorf("%d provider reads inside the TTL, want 1", fp.asked)
	}
	now = now.Add(time.Minute + time.Second)
	if _, ok := c.BlockersFor(context.Background(), "wh-1", IssueRef{Owner: "octo", Repo: "repo", Number: 12}); !ok {
		t.Fatal("expired cache entry reported nothing known")
	}
	if fp.asked != 2 {
		t.Errorf("%d provider reads after the TTL expired, want 2", fp.asked)
	}
	if fp.lastNum != 12 {
		t.Errorf("asked about issue %d, want 12", fp.lastNum)
	}
}

func TestBlockersForFailsOpenOnEveryUnknown(t *testing.T) {
	cases := map[string]struct {
		resolver Resolver
		reader   *fakeProvider
	}{
		"no connection configured": {resolver: func(context.Context, string) (*Client, error) {
			return nil, ErrNoConnection
		}},
		"connection unreadable": {resolver: func(context.Context, string) (*Client, error) {
			return nil, errors.New("store exploded")
		}},
		"resolver answers nothing": {resolver: func(context.Context, string) (*Client, error) {
			return nil, nil
		}},
		"provider unreachable": {resolver: func(context.Context, string) (*Client, error) {
			return &Client{Reader: &fakeProvider{err: errors.New("dial tcp: no route")}}, nil
		}},
		"provider has no dependency model": {resolver: func(context.Context, string) (*Client, error) {
			return &Client{Reader: &fakeProvider{err: provider.ErrUnsupportedDependencies}}, nil
		}},
		"client with no reader": {resolver: func(context.Context, string) (*Client, error) {
			return &Client{}, nil
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := New(&store.Store{}, discardLog(), Config{}, tc.resolver)
			open, ok := c.BlockersFor(context.Background(), "wh-1", IssueRef{Owner: "a", Repo: "b", Number: 1})
			if ok || len(open) != 0 {
				t.Fatalf("unknown answered (open=%v, ok=%v); unknown must mean no hold", open, ok)
			}
		})
	}
}

func TestBlockersForHonoursTheContextDeadline(t *testing.T) {
	c := New(&store.Store{}, discardLog(), Config{ReadTimeout: time.Millisecond},
		func(context.Context, string) (*Client, error) { return &Client{Reader: slowReader{}}, nil })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if open, ok := c.BlockersFor(ctx, "wh-1", IssueRef{Owner: "a", Repo: "b", Number: 1}); ok || open != nil {
		t.Errorf("a read past its deadline = (%v, %v), want the fail-open answer", open, ok)
	}
}

// slowReader reports its context's failure, which is what a read that outlived its deadline does.
type slowReader struct{}

func (slowReader) IssueBlockers(ctx context.Context, _ provider.Repo, _ int) ([]provider.Dependency, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestClientTimeoutPrefersTheConnection(t *testing.T) {
	if got := (&Client{}).timeout(DefaultReadTimeout); got != DefaultReadTimeout {
		t.Errorf("default timeout = %v, want %v", got, DefaultReadTimeout)
	}
	if got := (&Client{Timeout: 250 * time.Millisecond}).timeout(DefaultReadTimeout); got != 250*time.Millisecond {
		t.Errorf("per-connection timeout = %v, want 250ms", got)
	}
}

func TestParseRepoSpecNeedsBothHalves(t *testing.T) {
	if r, err := parseRepoSpec("octo/repo"); err != nil || r.Owner != "octo" || r.Name != "repo" {
		t.Errorf("parseRepoSpec = (%+v, %v)", r, err)
	}
	for _, s := range []string{"", "octo", "/", "octo/", "/repo", "  "} {
		if _, err := parseRepoSpec(s); err == nil {
			t.Errorf("parseRepoSpec(%q) = nil error, want refusal", s)
		}
	}
}

func TestRememberEvictsOldestBeyondTheCap(t *testing.T) {
	c := New(&store.Store{}, discardLog(), Config{CacheTTL: time.Hour}, nil)
	for i := 0; i < cacheCap+10; i++ {
		c.remember(store.IssueRefOf("a", "b", i), []store.BlockerRef{{Owner: "a", Repo: "b", Number: 1}}, nil)
	}
	if len(c.cache) > cacheCap {
		t.Errorf("cache holds %d entries, cap is %d", len(c.cache), cacheCap)
	}
	if _, fresh := c.cached(store.IssueRefOf("a", "b", cacheCap+9)); !fresh {
		t.Error("the freshest entry must survive eviction")
	}
	c.forget(store.IssueRefOf("a", "b", cacheCap+9))
	if _, fresh := c.cached(store.IssueRefOf("a", "b", cacheCap+9)); fresh {
		t.Error("forget left the entry cached")
	}
}

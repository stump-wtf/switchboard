// Package deps consults the tracker about issue dependencies and turns the answer into queue state:
// a todo whose issue is blocked waits as `blocked`, and it is released when its blockers close.
//
// It exists as its own package because the answer costs an outbound read. The delivery payload carries
// no dependency information, and Gitea fires no webhook when a dependency link is added or removed, so
// the only way switchboard can honour "blocked by" is to ask the tracker — with the read-only
// credential the webhook's owner configured for it (internal/store/webhook_provider.go), never one a
// caller supplied. Three callers ask: intake, when a delivery would mint work; the release path, when a
// close event names an issue that may have been a blocker; and the periodic pass, which catches the
// silent link edits and the reopens no event mentions.
//
// Everything here fails OPEN. A tracker that cannot be reached, a webhook with no credential, and a
// provider with no dependency model all mean "switchboard does not know", and not knowing is not a
// reason to stop work: the delivery is minted `pending` and the next pass re-asks. A hold is only ever
// asserted on evidence that a blocker is open.
//
// Governing: stump.wtf/switchboard#583, ADR-0055 (proposed: what a provider read may be used for),
// ADR-0053 (read clients behind one interface), ADR-0052 (the per-webhook connection this dials),
// ADR-0037 (the sealed credential this reads through the store, never directly).
package deps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stump-wtf/switchboard/internal/provider"
	"github.com/stump-wtf/switchboard/internal/store"
)

// Defaults for the read and the passes. A delivery waits at most DefaultReadTimeout on the tracker:
// intake is a request handler, and a slow tracker must not turn into a slow webhook, so the check
// gives up and the delivery is minted as it would have been before this feature existed.
const (
	DefaultReadTimeout = 3 * time.Second
	// DefaultCacheTTL bounds how often the same issue's blockers are re-read. Intake and the periodic
	// pass both ask, and a busy queue re-delivers; one read per issue per TTL is enough to keep holds
	// honest without turning every event into an API call.
	DefaultCacheTTL = 30 * time.Second
	// DefaultRecheckInterval is how often the periodic pass re-resolves live holds. It is the safety
	// net for the case the tracker cannot report: a dependency link added or removed silently, and a
	// blocker reopened after its dependent was released.
	DefaultRecheckInterval = 5 * time.Minute
	// DefaultRecheckLimit caps one pass, so a large backlog of holds cannot make one tick dial the
	// tracker unbounded times.
	DefaultRecheckLimit = 50
	// cacheCap bounds the cache's memory: the freshest entries win, oldest evicted first.
	cacheCap = 512

	// RecheckStaleness is how old a hold's last recorded check must be before the periodic pass
	// re-resolves it. Inside the TTL the cached answer stands anyway, so re-checking sooner would
	// re-ask a question that was just answered.
	RecheckStaleness = 2 * time.Minute
)

// IssueRef names the tracker issue a delivery or a todo is about. State is the issue's own state as
// the event reported it ("" when the event does not say), which is what makes a close event the
// release signal without another read.
type IssueRef struct {
	Owner  string
	Repo   string
	Number int
	State  string
}

// RefString is the canonical identity stored on todos.issue_ref.
func (r IssueRef) RefString() string { return store.IssueRefOf(r.Owner, r.Repo, r.Number) }

// RepoRef is the issue's repository as a provider.Repo.
func (r IssueRef) RepoRef() provider.Repo { return provider.Repo{Owner: r.Owner, Name: r.Repo} }

// Closed reports whether this event says the issue is done.
func (r IssueRef) Closed() bool { return strings.EqualFold(r.State, "closed") }

// Config carries what an operator tunes; zero values take the defaults above.
type Config struct {
	ReadTimeout   time.Duration
	CacheTTL      time.Duration
	RecheckInterv time.Duration
	RecheckLimit  int
}

func (c Config) normalized() Config {
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = DefaultReadTimeout
	}
	if c.CacheTTL <= 0 {
		c.CacheTTL = DefaultCacheTTL
	}
	if c.RecheckInterv <= 0 {
		c.RecheckInterv = DefaultRecheckInterval
	}
	if c.RecheckLimit <= 0 {
		c.RecheckLimit = DefaultRecheckLimit
	}
	return c
}

// RecheckInterval is the period the server's tick should run at.
func (c Config) RecheckInterval() time.Duration { return c.normalized().RecheckInterv }

// Resolver builds a provider client for one webhook's connection. The server wires the real one; tests
// substitute a fake so no test dials anything.
type Resolver func(ctx context.Context, webhookID string) (client *Client, err error)

// Client is one webhook's resolved provider connection: the reader to ask, and the repository the
// webhook's own config names (used only as a fallback when an event names no repository).
type Client struct {
	// Reader is the provider client: a *provider.Gitea or *provider.GitHub. A provider with no
	// dependency model satisfies it and reports ErrUnsupportedDependencies, which means "no hold".
	Reader  any
	Repo    provider.Repo
	Timeout time.Duration
}

// Checker answers dependency questions and acts on the answers.
type Checker struct {
	store    *store.Store
	log      *slog.Logger
	cfg      Config
	resolver Resolver
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	open   []store.BlockerRef
	all    []store.BlockerRef
	expiry time.Time
}

// New builds a Checker. resolver may be nil, which keeps the feature off: every question answers "I do
// not know" and nothing is ever held, so an instance with no provider credential behaves exactly as it
// did before this existed.
func New(st *store.Store, log *slog.Logger, cfg Config, resolver Resolver) *Checker {
	if log == nil {
		log = slog.Default()
	}
	return &Checker{store: st, log: log, cfg: cfg.normalized(), resolver: resolver,
		now: time.Now, cache: map[string]cacheEntry{}}
}

// SetClock replaces the clock the cache reads (tests).
func (c *Checker) SetClock(now func() time.Time) {
	if now != nil {
		c.now = now
	}
}

// Enabled reports whether anything can answer a dependency question.
func (c *Checker) Enabled() bool { return c != nil && c.resolver != nil }

// BlockersFor resolves the open blockers of one issue through one webhook's connection. ok is false
// when nothing can be known — no resolver wired, no credential, an unsupported provider, a read that
// failed or timed out — and false means the caller must NOT hold: intake mints the todo as it always
// did. Blockers returned are the open ones, in provider order.
func (c *Checker) BlockersFor(ctx context.Context, webhookID string, ref IssueRef) (open []store.BlockerRef, ok bool) {
	if !c.Enabled() {
		return nil, false
	}
	key := ref.RefString()
	if entry, fresh := c.cached(key); fresh {
		return entry.open, true
	}
	client, err := c.resolver(ctx, webhookID)
	if err != nil || client == nil {
		if err != nil && !errors.Is(err, ErrNoConnection) {
			c.log.Warn("dependency read refused; delivering without a hold",
				"issue", key, "webhook", webhookID, "err", err)
		}
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, client.timeout(c.cfg.ReadTimeout))
	defer cancel()
	openDeps, all, err := provider.DependenciesOf(ctx, client.Reader, ref.RepoRef(), ref.Number)
	if err != nil {
		if errors.Is(err, provider.ErrUnsupportedDependencies) {
			// The tracker cannot answer this question (GitHub has no dependency model). Silence means
			// no hold on the way in, and never a release of a hold that is already in place.
			return nil, false
		}
		c.log.Warn("dependency read failed; delivering without a hold",
			"issue", key, "webhook", webhookID, "err", err)
		return nil, false
	}
	open = toRefs(openDeps)
	c.remember(key, open, toRefs(all))
	return open, true
}

// ErrNoConnection is a webhook with no provider connection configured: the normal case on an instance
// that has not enabled the feature, and never worth a warning line per delivery.
var ErrNoConnection = errors.New("deps: this webhook has no provider connection configured")

// ReleaseBlocker records what a close event says about one issue and releases the dependents it was
// holding. It needs no provider read: the refs already stored say which todos waited on it, and a
// close event is the tracker's own word that it is done.
func (c *Checker) ReleaseBlocker(ctx context.Context, ref IssueRef) ([]store.Todo, error) {
	released, err := c.store.MarkBlockerState(ctx, ref.Owner, ref.Repo, ref.Number, "closed")
	if err != nil {
		return nil, err
	}
	if len(released) > 0 {
		c.log.Info("released dependents of a closed issue", "issue", ref.RefString(), "todos", len(released))
	}
	c.forget(ref.RefString())
	return released, nil
}

// MarkBlockerOpen records that an issue is open again, which re-holds the dependents the re-check finds
// claimable. Nothing releases from here: only a close does.
func (c *Checker) MarkBlockerOpen(ctx context.Context, ref IssueRef) error {
	_, err := c.store.MarkBlockerState(ctx, ref.Owner, ref.Repo, ref.Number, "open")
	c.forget(ref.RefString())
	return err
}

// Recheck resolves the holds whose recorded checks have gone stale and acts on what the tracker says
// now: a held todo whose blockers are all closed is released, a claimable todo whose blockers are open
// again is re-held, and a hold whose links were deleted out from under it is released and reported —
// an orphan hold must not wait forever on a link that no longer exists.
//
// It is the safety net the tracker cannot provide: adding or removing a dependency link fires no
// webhook at all, and a close event is sent for the blocker only, never for its dependents.
func (c *Checker) Recheck(ctx context.Context) (RecheckReport, error) {
	var report RecheckReport
	if !c.Enabled() {
		return report, nil
	}
	candidates, err := c.store.TodosWithHolds(ctx, RecheckStaleness, c.cfg.RecheckLimit)
	if err != nil {
		return report, err
	}
	report.Scanned = len(candidates)
	for _, cand := range candidates {
		ref, err := ParseRef(cand.Todo.IssueRef)
		if err != nil {
			c.log.Warn("hold carries an unreadable issue ref", "todo", cand.Todo.ID, "issue_ref", cand.Todo.IssueRef, "err", err)
			continue
		}
		open, ok := c.BlockersFor(ctx, cand.WebhookID, ref)
		if !ok {
			continue // fail open: nothing known, nothing changed
		}
		switch {
		case len(open) == 0 && cand.Todo.State == store.StateBlocked:
			if len(cand.Blockers) == 0 {
				report.Orphans++ // a hold with no links left: released, and counted so it is visible
			}
			if _, err := c.store.ReleaseHeldTodo(ctx, cand.Todo.ID); err != nil {
				c.log.Warn("recheck could not release a hold", "todo", cand.Todo.ID, "err", err)
				continue
			}
			report.Released++
		case len(open) == 0 && cand.Todo.State == "pending" && len(cand.Blockers) > 0:
			// Every link closed and nothing waiting: refresh the timestamps so the next pass leaves it
			// alone, and leave the row pending — it is already claimable.
			if err := c.store.HoldRefresh(ctx, cand.Todo.ID, open); err != nil {
				c.log.Warn("recheck could not refresh a hold's refs", "todo", cand.Todo.ID, "err", err)
			}
		case len(open) > 0 && cand.Todo.State == "pending":
			// A blocker reopened, or a link appeared with no event to announce it.
			if _, err := c.store.HoldTodo(ctx, cand.Todo.ID, ref.RefString(), open); err != nil {
				c.log.Warn("recheck could not re-hold a released todo", "todo", cand.Todo.ID, "err", err)
				continue
			}
			report.ReHeld++
		default:
			if err := c.store.HoldRefresh(ctx, cand.Todo.ID, open); err != nil {
				c.log.Warn("recheck could not refresh a hold's refs", "todo", cand.Todo.ID, "err", err)
			}
		}
	}
	return report, nil
}

// RecheckReport is one pass's outcome, for the log line and the metrics a sweep reports.
type RecheckReport struct {
	Scanned  int
	Released int
	ReHeld   int
	Orphans  int
}

// NewResolver builds the real Resolver: it opens each webhook's stored connection and dials it with
// the token in an Authorization header. The token never leaves the transport it is put in — it is not
// logged, not returned, and not stored by this package.
func NewResolver(st *store.Store) Resolver {
	return func(ctx context.Context, webhookID string) (*Client, error) {
		conn, err := st.ProviderReadConnection(ctx, webhookID)
		if err != nil {
			return nil, err
		}
		if conn.State != store.ProviderConnectionReady {
			return nil, ErrNoConnection
		}
		repo, err := parseRepoSpec(conn.Repo)
		if err != nil {
			return nil, err
		}
		hc := &http.Client{Transport: &bearerTransport{token: conn.Token, next: http.DefaultTransport}}
		reader, err := provider.NewGitea(conn.BaseURL, provider.WithHTTPClient(hc))
		if err != nil {
			return nil, err
		}
		return &Client{Reader: reader, Repo: repo}, nil
	}
}

// bearerTransport adds the stored read token to every request. GitHub reads would use the same shape;
// since GitHub has no dependency model, only the Gitea client is built today.
type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.token != "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.next.RoundTrip(req)
}

// parseRepoSpec reads a stored "owner/name" into a provider.Repo.
func parseRepoSpec(spec string) (provider.Repo, error) {
	owner, name, ok := strings.Cut(strings.TrimSpace(spec), "/")
	if !ok || owner == "" || name == "" {
		return provider.Repo{}, fmt.Errorf("deps: webhook connection names no repository (%q)", spec)
	}
	return provider.Repo{Owner: owner, Name: name}, nil
}

// ParseRef reads a stored todos.issue_ref ("owner/repo#number") back into an IssueRef.
func ParseRef(s string) (IssueRef, error) {
	repoPart, numPart, ok := strings.Cut(s, "#")
	if !ok {
		return IssueRef{}, fmt.Errorf("deps: issue ref %q has no number", s)
	}
	owner, name, ok := strings.Cut(repoPart, "/")
	if !ok || owner == "" || name == "" {
		return IssueRef{}, fmt.Errorf("deps: issue ref %q has no repository", s)
	}
	n, err := strconv.Atoi(numPart)
	if err != nil || n < 1 {
		return IssueRef{}, fmt.Errorf("deps: issue ref %q has no usable number", s)
	}
	return IssueRef{Owner: owner, Repo: name, Number: n}, nil
}

// IssueRefFromPayload reads the tracker issue a forge delivery is about. It recognises the issue shape
// both Gitea and GitHub post ("issue.number" plus "repository.full_name"), and takes the issue's own
// state when the payload carries one — that is what makes an issues/closed delivery the release signal
// with no read at all. A delivery that is not about an issue (a pull request, a comment on a commit, a
// ping) reports false, and the delivery routes exactly as it did before.
//
// The payload is untrusted external data: it chooses which issue this delivery is about and nothing
// else. Nothing here dials an address taken from the body — the connection dialed is the one the
// webhook's owner stored.
func IssueRefFromPayload(body []byte) (IssueRef, bool) {
	var p struct {
		Issue *struct {
			Number int    `json:"number"`
			State  string `json:"state"`
		} `json:"issue"`
		Repository *struct {
			FullName string `json:"full_name"`
			Name     string `json:"name"`
			Owner    struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil || p.Issue == nil || p.Issue.Number < 1 {
		return IssueRef{}, false
	}
	owner, name := "", ""
	if p.Repository != nil {
		if full := strings.TrimSpace(p.Repository.FullName); full != "" {
			owner, name, _ = strings.Cut(full, "/")
		}
		if owner == "" {
			owner, name = p.Repository.Owner.Login, p.Repository.Name
		}
	}
	if owner == "" || name == "" {
		return IssueRef{}, false
	}
	return IssueRef{Owner: owner, Repo: name, Number: p.Issue.Number, State: p.Issue.State}, true
}

func toRefs(deps []provider.Dependency) []store.BlockerRef {
	out := make([]store.BlockerRef, 0, len(deps))
	for _, d := range deps {
		out = append(out, store.BlockerRef{
			Owner: d.Repo.Owner, Repo: d.Repo.Name, Number: d.Number,
			State: d.State, HTMLURL: d.HTMLURL,
		})
	}
	return out
}

func (c *Checker) cached(key string) (cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[key]
	if !ok || c.now().After(e.expiry) {
		return cacheEntry{}, false
	}
	return e, true
}

func (c *Checker) remember(key string, open, all []store.BlockerRef) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) >= cacheCap {
		var oldest string
		var when time.Time
		for k, e := range c.cache {
			if oldest == "" || e.expiry.Before(when) {
				oldest, when = k, e.expiry
			}
		}
		delete(c.cache, oldest)
	}
	c.cache[key] = cacheEntry{open: open, all: all, expiry: c.now().Add(c.cfg.CacheTTL)}
}

func (c *Checker) forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cache, key)
}

// timeout is the per-client override over the configured read timeout.
func (cl *Client) timeout(def time.Duration) time.Duration {
	if cl != nil && cl.Timeout > 0 {
		return cl.Timeout
	}
	return def
}

// RecheckInterval is how often the server should run Recheck.
func (c *Checker) RecheckInterval() time.Duration { return c.cfg.RecheckInterv }

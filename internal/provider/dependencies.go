package provider

// Issue Dependencies: "Which Issues Block This One?"
//
// Gitea has first-class issue dependencies: a person or an agent declares "blocked by" in the tracker,
// and the API reads it back. Switchboard consumes that read rather than inventing a second dependency
// model (issue #583, ADR-0055), which is why this file exists and why there is no dependency API on
// switchboard itself: declaring a dependency stays where the work is declared.
//
// The read is read-only, and it is the reason a webhook now needs a provider connection configured
// (internal/store/webhook_provider.go): the delivery payload carries no dependency information at all,
// and adding or removing a link fires no webhook, so nothing but an API call can answer the question.
//
// GitHub has no issue-dependency API, so its client reports ErrUnsupportedDependencies and the caller
// treats the provider as saying nothing (which means: no hold, deliver as usual). Silence is the right
// answer for an unsupported provider; an error would let an unsupported tracker hold work hostage.
//
// Governing: stump.wtf/switchboard#583, ADR-0053 (read clients behind one interface), ADR-0052 (the
// per-webhook connection they dial through), ADR-0055 (proposed: what a provider read may be used
// for).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrUnsupportedDependencies reports a provider with no issue-dependency model to read.
var ErrUnsupportedDependencies = errors.New("provider: this provider has no issue dependencies")

// maxDependencyPages bounds the walk of one issue's blockers. Real issues carry a handful; the cap
// keeps a provider that ignores the page parameter from turning one intake check into an unbounded
// crawl.
const maxDependencyPages = 20

// Dependency is one issue as its dependent's blocker list reported it, including the repository it
// lives on: Gitea links cross repositories, so the blocker of an issue in one repo can be an issue in
// another, and the release lookup needs the blocker's own owner/name.
type Dependency struct {
	Repo    Repo   `json:"repo"`
	Number  int    `json:"number"`
	Title   string `json:"title,omitempty"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url,omitempty"`
}

// RefString is the canonical "owner/repo#number" identity of the blocker.
func (d Dependency) RefString() string {
	return d.Repo.Owner + "/" + d.Repo.Name + "#" + strconv.Itoa(d.Number)
}

// DependencyReader is implemented by providers that expose issue dependencies. Callers type-assert for
// it: a provider that is not one says nothing about dependencies, which is not an error.
type DependencyReader interface {
	// IssueBlockers returns the issues blocking the given one, across every repository the links
	// reach. An issue with no blockers returns an empty slice and a nil error.
	IssueBlockers(ctx context.Context, repo Repo, number int) ([]Dependency, error)
}

var _ DependencyReader = (*Gitea)(nil)

// giteaDependency is a dependency row: an issue object that also names the repository it lives on,
// which is what makes cross-repository links resolvable.
type giteaDependency struct {
	giteaIssue
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// IssueBlockers reads GET /repos/{owner}/{repo}/issues/{number}/dependencies — the blockers of the
// issue, paginated, each row carrying its own state and repository.
func (g *Gitea) IssueBlockers(ctx context.Context, repo Repo, number int) ([]Dependency, error) {
	const op = "issue blockers"
	if number < 1 {
		return nil, fmt.Errorf("provider/gitea: %s: issue number %d is not positive", op, number)
	}
	var out []Dependency
	for page := 1; page <= maxDependencyPages; page++ {
		endpoint, err := g.b.repoURL(op, repo,
			"issues/"+strconv.Itoa(number)+"/dependencies", giteaPageQuery(g.b.pageSize, page))
		if err != nil {
			return nil, err
		}
		data, _, err := g.b.getJSON(ctx, op, endpoint)
		if err != nil {
			return nil, err
		}
		var raw []giteaDependency
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("provider/gitea: %s: decode dependency page: %w", op, err)
		}
		for _, d := range raw {
			dep, err := parseDependency(d)
			if err != nil {
				return nil, fmt.Errorf("provider/gitea: %s: %w", op, err)
			}
			out = append(out, dep)
		}
		if len(raw) < g.b.pageSize {
			return out, nil
		}
	}
	return out, fmt.Errorf("provider/gitea: %s: %s/%s#%d has more than %d pages of blockers",
		op, repo.Owner, repo.Name, number, maxDependencyPages)
}

// IssueBlockers always reports ErrUnsupportedDependencies: GitHub has no issue-dependency model, so
// there is nothing for switchboard to honour there.
func (*GitHub) IssueBlockers(context.Context, Repo, int) ([]Dependency, error) {
	return nil, fmt.Errorf("provider/github: %w", ErrUnsupportedDependencies)
}

// parseDependency turns one provider row into a Dependency, refusing a row whose repository is not
// exactly "owner/name" rather than guessing at a link the release path could never resolve.
func parseDependency(d giteaDependency) (Dependency, error) {
	parts := strings.SplitN(strings.TrimSpace(d.Repository.FullName), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Dependency{}, fmt.Errorf("dependency %q names no repository (%q)",
			d.HTMLURL, d.Repository.FullName)
	}
	repo := Repo{Owner: parts[0], Name: parts[1]}
	if err := repo.validate(); err != nil {
		return Dependency{}, err
	}
	return Dependency{
		Repo:    repo,
		Number:  d.Number,
		Title:   d.Title,
		State:   strings.ToLower(strings.TrimSpace(d.State)),
		HTMLURL: d.HTMLURL,
	}, nil
}

// DependenciesOf reads every issue blocking repo#number, in one call, and says which of them are still
// open. It is the question the intake gate asks. An issue with no links answers with empty lists and no
// error: nothing is blocking, deliver as usual.
//
// A provider with no dependency model — GitHub, or a client that exposes no dependency read at all —
// answers ErrUnsupportedDependencies rather than an empty list, because "this tracker cannot tell me"
// and "this issue has no blockers" are different answers. Callers must treat the first as silence: no
// hold on a new delivery, and never a release of a hold already in place.
func DependenciesOf(ctx context.Context, reader any, repo Repo, number int) (open, all []Dependency, err error) {
	dr, ok := reader.(DependencyReader)
	if !ok {
		return nil, nil, fmt.Errorf("provider: %T exposes no dependency read: %w", reader, ErrUnsupportedDependencies)
	}
	all, err = dr.IssueBlockers(ctx, repo, number)
	if err != nil {
		return nil, nil, err
	}
	for _, d := range all {
		if d.Number == number && d.Repo == repo {
			continue // a self-link is not a reason to wait
		}
		if !strings.EqualFold(d.State, "closed") {
			open = append(open, d)
		}
	}
	return open, all, nil
}

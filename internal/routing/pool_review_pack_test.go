package routing

// The pool-review rule pack (docs/routing/rule-packs/pool-review.json): installed on each identity's
// forge pool webhooks with params.identity set to that identity. Both identities subscribe their pools
// to the same org events, so without it every PR review request reaches both — and on 2026-09-11 a
// joestump pool worker reviewed and merged joestump's own PRs that way. The pack delivers a review
// request only to the pool of the identity actually requested, never lets a pool review its own
// identity's PR, and leaves everything else (comments, issues) routing to the pool's target queue.
//
// Governing: ADR-0025 (single executor, cross-identity review), ADR-0024.

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func loadPoolPack(t *testing.T, identity string) Config {
	t.Helper()
	raw, err := os.ReadFile("../../docs/routing/rule-packs/pool-review.json")
	if err != nil {
		t.Fatalf("read pack: %v", err)
	}
	var cfg Config
	dec := json.NewDecoder(bytesReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		t.Fatalf("decode pack: %v", err)
	}
	// An empty identity models an operator who saved the rules with no params at all: every
	// identity-keyed rule then fails closed. Otherwise keep the pack's own params (the bot list) and
	// override only the identity, the way an operator installs it per pool.
	if identity == "" {
		cfg.Params = nil
	} else {
		if cfg.Params == nil {
			cfg.Params = map[string]any{}
		}
		cfg.Params["identity"] = identity
	}
	return cfg
}

func TestPoolReviewPack(t *testing.T) {
	pool := "00000000-0000-0000-0000-0000000000f0"
	g := Grant{TargetQueue: "forge", Queues: []string{"forge"}, Endpoints: []string{pool}}
	forAgent := map[string]any{"requested_reviewer": map[string]any{"login": "joestump-agent"}}
	forJoe := map[string]any{
		"requested_reviewer": map[string]any{"login": "joestump"},
		"pull_request":       map[string]any{"user": map[string]any{"login": "joestump-agent"}},
	}
	// A bot-authored delivery. sender.login is the actor field that matters: the review events carry
	// no comment object at all, and where one exists it never disagrees with sender.
	botSender := map[string]any{
		"sender":  map[string]any{"login": "gitea-actions"},
		"comment": map[string]any{"user": map[string]any{"login": "gitea-actions"}},
	}
	// The sibling agent approving joestump's PR: the merge-gate signal for joestump, a self-echo for
	// joestump-agent. The sample's pull_request.user is joestump.
	agentApproval := map[string]any{"sender": map[string]any{"login": "joestump-agent"}}
	cases := []struct {
		name     string
		identity string
		sample   string
		patch    map[string]any
		dropRule string // "" means it reaches the pool's target queue
	}{
		{"joestump's PR, agent requested: dropped on joestump's pool", "joestump", "gitea-pull-request-review-requested", forAgent, "review-request-not-for-me"},
		{"joestump's PR, agent requested: delivered to the agent's pool", "joestump-agent", "gitea-pull-request-review-requested", forAgent, ""},
		{"agent's PR, joestump requested: delivered to joestump's pool", "joestump", "gitea-pull-request-review-requested", forJoe, ""},
		{"agent's PR, joestump requested: dropped on the agent's pool", "joestump-agent", "gitea-pull-request-review-requested", forJoe, "review-request-not-for-me"},
		{"a removed request for someone else is dropped", "joestump", "gitea-pull-request-review-requested",
			map[string]any{"action": "review_request_removed"}, "review-request-not-for-me"},
		{"own PR opened never reaches the author's pool", "joestump", "gitea-pull-request-review-requested",
			map[string]any{"action": "opened", "requested_reviewer": nil}, "own-pr-review-trigger"},
		{"own PR pushed to never reaches the author's pool", "joestump", "gitea-pull-request-review-requested",
			map[string]any{"action": "synchronized", "requested_reviewer": nil}, "own-pr-review-trigger"},
		{"another identity's PR opened still reaches the pool", "joestump-agent", "gitea-pull-request-review-requested",
			map[string]any{"action": "opened", "requested_reviewer": nil}, ""},
		{"review feedback on your own PR still reaches you", "joestump", "gitea-pull-request-comment", nil, ""},
		{"and reaches the reviewer's pool too", "joestump-agent", "gitea-pull-request-comment", nil, ""},
		{"an issue event is untouched", "joestump", "gitea-issue-opened", nil, ""},
		{"with no identity set, every review request fails closed", "", "gitea-pull-request-review-requested", forJoe, "review-request-not-for-me"},
		{"with no identity set, a team request (no requested_reviewer) fails closed too", "", "gitea-pull-request-review-requested",
			map[string]any{"requested_reviewer": nil}, "review-request-not-for-me"},
		{"a team request is not a request for this identity", "joestump-agent", "gitea-pull-request-review-requested",
			map[string]any{"requested_reviewer": nil}, "review-request-not-for-me"},
		{"with no identity set, comments still route", "", "gitea-pull-request-comment", nil, ""},

		// Noise drops. These also pin the `as $s` binding in the bot rules: written the obvious way,
		// as any(($params.bot_actors // [])[]; . == (.payload.sender.login // "")), the `.` inside
		// any() is the generator's element, so .payload indexes a string, the rule faults, and a
		// faulted rule is a no-match — the drop silently never fires. Every bot case below fails if
		// anyone rewrites these expressions in that unbound form.
		{"a bot's PR comment is noise", "joestump", "gitea-pull-request-comment", botSender, "bot-comment-noise"},
		{"a bot's comment is noise on the other pool too", "joestump-agent", "gitea-pull-request-comment", botSender, "bot-comment-noise"},
		{"a bot's approval notification is noise", "joestump", "gitea-pull-request-approved", nil, "bot-review-outcome"},
		{"a bot's rejection notification is noise", "joestump", "gitea-pull-request-approved",
			map[string]any{"review": map[string]any{"type": "pull_request_review_rejected"}}, "bot-review-outcome"},
		// With no params at all there is no bot list either, so the bot rules match nothing and the
		// identity-keyed outcome rule is what fails closed. Still dropped, by a different rule.
		{"with no params at all, a bot approval still fails closed", "", "gitea-pull-request-approved", nil, "review-outcome-not-my-pr"},

		// Review outcomes: the author's own approval is the merge-gate signal and must survive; the
		// reviewer-side echo of that same approval is dropped.
		{"an approval of this identity's own PR still wakes it", "joestump", "gitea-pull-request-approved", agentApproval, ""},
		{"the reviewer's echo of its own approval is dropped", "joestump-agent", "gitea-pull-request-approved", agentApproval, "review-outcome-not-my-pr"},
		{"a rejection of this identity's own PR still wakes it", "joestump", "gitea-pull-request-approved",
			map[string]any{"action": "reviewed", "review": map[string]any{"type": "pull_request_review_rejected"},
				"sender": map[string]any{"login": "joestump-agent"}}, ""},
		{"with no identity set, review outcomes fail closed", "", "gitea-pull-request-approved", agentApproval, "review-outcome-not-my-pr"},

		// Human and sibling-agent signal is never noise.
		{"a human's PR comment always reaches the pool", "joestump", "gitea-pull-request-comment",
			map[string]any{"sender": map[string]any{"login": "joestump"}}, ""},
		{"the sibling agent's PR comment always reaches the pool", "joestump", "gitea-pull-request-comment",
			map[string]any{"sender": map[string]any{"login": "joestump-agent"}}, ""},
	}
	sb := testSandbox(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := loadPoolPack(t, c.identity)
			if err := Validate(cfg, g); err != nil {
				t.Fatalf("pack does not validate: %v", err)
			}
			in := caseInput(t, packCase{Sample: c.sample, Patch: c.patch, TrustMode: "token", Verified: ptrBool(false)})
			in.Source = "generic" // pool webhooks are token-trust generic hooks
			d := InProcess{}.Route(context.Background(), cfg, g, in)
			if len(d.Trace.Faults) > 0 {
				t.Fatalf("faults: %+v", d.Trace.Faults)
			}
			if c.dropRule != "" {
				if !d.Drop || d.Trace.RuleID != c.dropRule {
					t.Fatalf("decision = %+v, want drop by %s", d, c.dropRule)
				}
			} else if d.Drop || d.Queue != "forge" || d.Trace.Cause != CauseNoMatch {
				t.Fatalf("decision = %+v, want the pool's forge queue", d)
			}
			if got := sb.Route(context.Background(), cfg, g, in); !reflect.DeepEqual(got, d) {
				t.Fatalf("sandbox %+v differs from in-process %+v", got, d)
			}
		})
	}
}

func ptrBool(b bool) *bool { return &b }

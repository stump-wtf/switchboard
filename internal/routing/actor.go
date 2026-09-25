package routing

// Trusted actors (ADR-0031, SPEC-0026 REQ-5, REQ-10). "Who may start work here" is a first-class,
// owner-set field on each github, gitea and cairn webhook. It is evaluated in Go from the VERIFIED
// body, before any rule runs, so a rule list that forgets to check trust, or an agent's rule edit
// that never mentions it, cannot route untrusted input. The same projection feeds the envelope's
// unforgeable .actor field, so rules can still tell a trusted maintainer's label on an outsider's
// issue (author_trusted false) from the maintainer's own work.
//
// Fail closed everywhere:
//   - an empty list trusts no one;
//   - trusting every verified sender is the explicit, flagged {"allow_all": true};
//   - a missing or unreadable field on a source that has an actor projection is treated as an empty
//     list by EvaluateTrust's callers, never as "gate off".
//
// Governing: ADR-0031, SPEC-0026 REQ-5 "Trusted Actors", REQ-10 "Envelope and Work Order Additions".
//
// @joestump-agent 09/25/2026 - Added for #385.

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Trust-list limits (SPEC-0026 REQ-5).
const (
	MaxTrustedActors     = 256
	MaxTrustedActorBytes = 128

	MatchSender = "sender"
	MatchAuthor = "author"
	MatchBoth   = "both"
)

// ErrNoActorProjection is returned when trusted_actors is set on a source whose bodies carry no
// verifiable actor. Callers surface its message verbatim.
var ErrNoActorProjection = errors.New("trusted_actors needs a signed source with an actor projection (github, gitea or cairn)")

// HasActorProjection reports whether a source's verified body names its actor, which is exactly the
// set of sources the trust gate runs on. A token-trust (generic) body is unsigned, and stripe and
// slack bodies name no forge or cairn actor.
func HasActorProjection(source string) bool {
	switch source {
	case "github", "gitea", SourceCairn:
		return true
	}
	return false
}

// TrustedActors is a webhook's trust list in its canonical, stored form. Exactly one shape applies:
// {"allow_all": true}, {"logins": […], "match": …} for github and gitea, or {"actor_ids": […]} for
// cairn. Use ParseTrustedActors to build one from agent input and DecodeTrustedActors to read a stored
// one.
type TrustedActors struct {
	AllowAll bool     `json:"allow_all,omitempty"`
	Logins   []string `json:"logins,omitempty"`
	Match    string   `json:"match,omitempty"`
	ActorIDs []string `json:"actor_ids,omitempty"`
	source   string
}

// MarshalJSON writes the canonical shape, with an empty list written as [] rather than dropped, so
// a cleared list reads back as "trusts no one" rather than as a missing field.
func (t TrustedActors) MarshalJSON() ([]byte, error) {
	switch {
	case t.AllowAll:
		return []byte(`{"allow_all":true}`), nil
	case t.source == SourceCairn || t.ActorIDs != nil:
		return json.Marshal(struct {
			ActorIDs []string `json:"actor_ids"`
		}{nonNilStrings(t.ActorIDs)})
	default:
		m := t.Match
		if m == "" {
			m = MatchSender
		}
		return json.Marshal(struct {
			Logins []string `json:"logins"`
			Match  string   `json:"match"`
		}{nonNilStrings(t.Logins), m})
	}
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// TrustedActorsInput is the agent-facing shape. Pointers keep "absent" distinct from "empty", which
// is what lets allow_all be exclusive with the other fields and a source's foreign field be refused.
type TrustedActorsInput struct {
	AllowAll *bool     `json:"allow_all,omitempty" jsonschema:"true trusts every verified sender (explicit, flagged opt-in; exclusive with the other fields)"`
	Logins   *[]string `json:"logins,omitempty" jsonschema:"github/gitea: trusted logins, compared case-insensitively (0-256 entries, each 1-128 bytes)"`
	Match    *string   `json:"match,omitempty" jsonschema:"github/gitea: which actor must be trusted: sender (default), author, or both"`
	ActorIDs *[]string `json:"actor_ids,omitempty" jsonschema:"cairn: trusted signed actor_id values, compared exactly (0-256 entries, each 1-128 bytes)"`
}

// DefaultTrustedActors is what a new webhook of source gets when its creator names no trust list:
// an empty list, which trusts no one. ok is false for a source with no actor projection.
func DefaultTrustedActors(source string) (TrustedActors, bool) {
	switch source {
	case "github", "gitea":
		return TrustedActors{Logins: []string{}, Match: MatchSender, source: source}, true
	case SourceCairn:
		return TrustedActors{ActorIDs: []string{}, source: source}, true
	}
	return TrustedActors{}, false
}

// ParseTrustedActors validates agent input for a webhook of source and returns the canonical value.
// Every refusal is a plain error whose message is safe to show the caller.
func ParseTrustedActors(source string, in TrustedActorsInput) (TrustedActors, error) {
	if !HasActorProjection(source) {
		if source == "generic" {
			return TrustedActors{}, fmt.Errorf("%w: a generic webhook's body is not signed, so its sender cannot be verified", ErrNoActorProjection)
		}
		return TrustedActors{}, fmt.Errorf("%w: a %s webhook's body names no verifiable actor", ErrNoActorProjection, source)
	}
	if in.AllowAll != nil {
		if in.Logins != nil || in.Match != nil || in.ActorIDs != nil {
			return TrustedActors{}, errors.New("allow_all is exclusive with logins, match and actor_ids")
		}
		if !*in.AllowAll {
			return TrustedActors{}, errors.New("allow_all may only be true; to trust no one, set an empty list")
		}
		return TrustedActors{AllowAll: true, source: source}, nil
	}
	if source == SourceCairn {
		if in.Logins != nil || in.Match != nil {
			return TrustedActors{}, errors.New("a cairn webhook trusts actor_ids; logins and match are for github and gitea")
		}
		if in.ActorIDs == nil {
			return TrustedActors{}, errors.New("give actor_ids (or allow_all)")
		}
		ids, err := checkTrustList("actor_ids", *in.ActorIDs)
		if err != nil {
			return TrustedActors{}, err
		}
		return TrustedActors{ActorIDs: ids, source: source}, nil
	}
	if in.ActorIDs != nil {
		return TrustedActors{}, fmt.Errorf("a %s webhook trusts logins; actor_ids is for cairn", source)
	}
	if in.Logins == nil {
		return TrustedActors{}, errors.New("give logins (or allow_all)")
	}
	logins, err := checkTrustList("logins", *in.Logins)
	if err != nil {
		return TrustedActors{}, err
	}
	match := MatchSender
	if in.Match != nil {
		match = *in.Match
	}
	switch match {
	case MatchSender, MatchAuthor, MatchBoth:
	default:
		return TrustedActors{}, errors.New("match must be sender, author or both")
	}
	return TrustedActors{Logins: logins, Match: match, source: source}, nil
}

func checkTrustList(field string, list []string) ([]string, error) {
	if len(list) > MaxTrustedActors {
		return nil, fmt.Errorf("%s has %d entries; the limit is %d", field, len(list), MaxTrustedActors)
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		switch {
		case strings.TrimSpace(v) == "":
			return nil, fmt.Errorf("%s entries must be non-empty", field)
		case len(v) > MaxTrustedActorBytes:
			return nil, fmt.Errorf("%s entries must be at most %d bytes", field, MaxTrustedActorBytes)
		}
		out = append(out, v)
	}
	return out, nil
}

// DecodeTrustedActors reads a stored trust list for a webhook of source. A missing (nil) or
// unreadable value decodes to the source's empty list, so the gate fails closed; ok reports whether
// the stored value was present and well-formed, for the caller to log.
func DecodeTrustedActors(source string, raw []byte) (t TrustedActors, ok bool) {
	empty, _ := DefaultTrustedActors(source)
	if len(raw) == 0 {
		return empty, false
	}
	var in TrustedActorsInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return empty, false
	}
	t, err := ParseTrustedActors(source, in)
	if err != nil {
		return empty, false
	}
	return t, true
}

// Actor is who a verified delivery says acted: the sender (who triggered the event) and the author
// (who wrote the thing it is about). Either is "" when the body does not name one.
type Actor struct {
	Sender string
	Author string
}

// forgeActorBody is the subset of a github/gitea payload the actor projection reads.
type forgeActorBody struct {
	Sender      *forgeLogin `json:"sender"`
	Comment     *actorOwned `json:"comment"`
	Review      *actorOwned `json:"review"`
	PullRequest *actorOwned `json:"pull_request"`
	Issue       *actorOwned `json:"issue"`
}

type actorOwned struct {
	User *forgeLogin `json:"user"`
}

func (o *actorOwned) login() string {
	if o == nil || o.User == nil {
		return ""
	}
	return o.User.Login
}

// ActorOf projects a verified body onto its actor. For github and gitea, sender is sender.login, and
// author is the first present of comment.user.login, review.user.login, pull_request.user.login and
// issue.user.login. For cairn, both are the signed actor_id; on_behalf_of is the sharing client's
// self-reported name and is never used. Other sources have no projection and return nil.
func ActorOf(source string, body []byte) *Actor {
	switch source {
	case "github", "gitea":
		var p forgeActorBody
		if json.Unmarshal(body, &p) != nil {
			return &Actor{}
		}
		a := &Actor{}
		if p.Sender != nil {
			a.Sender = p.Sender.Login
		}
		for _, o := range []*actorOwned{p.Comment, p.Review, p.PullRequest, p.Issue} {
			if l := o.login(); l != "" {
				a.Author = l
				break
			}
		}
		return a
	case SourceCairn:
		var p cairnSubjectBody
		if json.Unmarshal(body, &p) != nil {
			return &Actor{}
		}
		return &Actor{Sender: p.Data.ActorID, Author: p.Data.ActorID}
	}
	return nil
}

// ActorTrust is a delivery's actor with the trust gate's verdict: the .actor envelope field. Every
// flag is null on a source with no actor projection; the per-actor flags are null under allow_all.
type ActorTrust struct {
	Sender        *string `json:"sender"`
	Author        *string `json:"author"`
	SenderTrusted *bool   `json:"sender_trusted"`
	AuthorTrusted *bool   `json:"author_trusted"`
	Trusted       *bool   `json:"trusted"`
}

// IsTrusted reports the gate's verdict; a nil receiver or null verdict is not trusted.
func (a *ActorTrust) IsTrusted() bool {
	return a != nil && a.Trusted != nil && *a.Trusted
}

// EvaluateTrust runs the trust gate for a verified delivery (SPEC-0026 REQ-5 "Evaluation"). It
// returns nil for a source with no actor projection (no gate). Logins compare case-insensitively and
// actor ids exactly. When the body names no author, author_trusted equals sender_trusted.
func EvaluateTrust(source string, t TrustedActors, body []byte) *ActorTrust {
	a := ActorOf(source, body)
	if a == nil {
		return nil
	}
	out := &ActorTrust{Sender: strPtr(a.Sender), Author: strPtr(a.Author)}
	if t.AllowAll {
		out.Trusted = boolPtr(true)
		return out
	}
	in := func(name string) bool {
		if name == "" {
			return false
		}
		if source == SourceCairn {
			return slices.Contains(t.ActorIDs, name)
		}
		return slices.ContainsFunc(t.Logins, func(l string) bool { return strings.EqualFold(l, name) })
	}
	sender := in(a.Sender)
	author := sender
	if a.Author != "" {
		author = in(a.Author)
	}
	trusted := sender
	switch t.Match {
	case MatchAuthor:
		trusted = author
	case MatchBoth:
		trusted = sender && author
	}
	out.SenderTrusted, out.AuthorTrusted, out.Trusted = boolPtr(sender), boolPtr(author), boolPtr(trusted)
	return out
}

// Trust-gate trace stage and cause.
const (
	StageTrustGate      = "trust_gate"
	CauseUntrustedActor = "untrusted_actor"
)

// UntrustedDecision is the outcome for a delivery the trust gate held: no rule ran, nothing is
// routed, and the trace records the actor verdict. The receiver quarantines it on the owner endpoint
// as untrusted_actor.
// Governing: SPEC-0026 REQ-5 "Evaluation" (an untrusted delivery is not evaluated by rules), REQ-6.
func UntrustedDecision(a *ActorTrust) Decision {
	return Decision{Untrusted: true,
		Trace: Trace{Stage: StageTrustGate, Cause: CauseUntrustedActor, Actor: a}}
}

// actorEnvelope renders .actor. A delivery the gate never saw (a source with no projection, or a
// dry-run with no trust input) still gets the names when they can be parsed, with null flags.
func actorEnvelope(in EnvelopeInput) map[string]any {
	a := in.Actor
	if a == nil {
		if p := ActorOf(in.Source, in.Body); p != nil {
			a = &ActorTrust{Sender: strPtr(p.Sender), Author: strPtr(p.Author)}
		} else {
			a = &ActorTrust{}
		}
	}
	return map[string]any{
		"sender": derefString(a.Sender), "author": derefString(a.Author),
		"sender_trusted": derefBool(a.SenderTrusted), "author_trusted": derefBool(a.AuthorTrusted),
		"trusted": derefBool(a.Trusted),
	}
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func boolPtr(b bool) *bool { return &b }

func derefString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func derefBool(p *bool) any {
	if p == nil {
		return nil
	}
	return *p
}

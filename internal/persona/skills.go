// Package persona holds the pure, storage-independent projections a persona publishes — chiefly the
// derivation of its advertised A2A skills from the verbs it actually vends. Keeping this logic free of
// any store or HTTP dependency lets the well-known Agent Card route, the operator Personas view, and
// the persona wizard's live card preview (SPEC-0015) all build a card from the same authoritative map.
//
// Governing: ADR-0009 (personas as scoped Agent Cards), SPEC-0009 REQ "Skills Derived From Vended
// Capability", SPEC-0015 REQ "Personas View And Wizard".
package persona

import "sort"

// Skill is an A2A AgentSkill-shaped descriptor: the outward, discovery-only advertisement of one thing
// a persona can do. Its fields map directly onto the A2A Agent Card `skills[]` entries the well-known
// endpoint serves. A Skill is never hand-authored on a persona; it is derived from the persona's
// vended verb_subset by DeriveSkills, so "what a card says it does" equals "what the persona can do" by
// construction. Governing: SPEC-0009 REQ "Skills Derived From Vended Capability", REQ "Agent Card
// Mapping".
type Skill struct {
	// ID is the stable A2A skill identifier (kebab-case), unique within a card.
	ID string
	// Name is the human-readable skill name surfaced on the card.
	Name string
	// Description is a one-line summary of the capability, grounded in the verbs that back it.
	Description string
	// Tags are coarse A2A discovery facets peers can filter on.
	Tags []string
}

// skillDefinition pairs an advertised Skill with the verbs a persona MUST hold to perform it. The
// derivation is all-of: every verb in requiredVerbs must be present in a persona's verb_subset for the
// skill to be advertised. A skill therefore never appears unless the persona can actually carry out
// every verb it depends on. Governing: SPEC-0009 REQ "Skills Derived From Vended Capability" (scenario
// "A skill requiring an ungranted verb is never advertised").
type skillDefinition struct {
	skill         Skill
	requiredVerbs []string
}

// skillCatalog is the authoritative verb→skill map (SPEC-0009 / ADR-0009: canonical in code with a
// covering test). It is maintained alongside the verb registry (internal/mcp) so the derivation stays
// current as verbs are added. Order here is the order skills are advertised on a card, so it is kept
// deterministic. This is THE single source of truth: the well-known Agent Card endpoint
// (internal/web/agentcard.go) and the wizard's live card preview both derive from it, so preview and
// published card can never disagree.
//
// The entries are grounded in the real vended verb surface: the SPEC-0006 drain verbs
// (list_todos/claim/complete), the SPEC-0007 delegation verb (create_for), and the SPEC-0005 replay
// verb (replay_webhook_event).
// Governing: ADR-0009 (derivation rule), SPEC-0009 REQ "Skills Derived From Vended Capability".
var skillCatalog = []skillDefinition{
	{
		skill: Skill{
			ID:          "process-work",
			Name:        "Process work",
			Description: "Drains todos from its queues: lists pending work, claims it under a lease, and reports completion.",
			Tags:        []string{"todo", "queue", "worker"},
		},
		requiredVerbs: []string{"list_todos", "claim", "complete"},
	},
	{
		skill: Skill{
			ID:          "delegate-work",
			Name:        "Delegate work",
			Description: "Creates todos on behalf of other principals, routing work into their queues.",
			Tags:        []string{"todo", "delegation"},
		},
		requiredVerbs: []string{"create_for"},
	},
	{
		skill: Skill{
			ID:          "replay-events",
			Name:        "Replay inbound events",
			Description: "Replays a previously verified inbound webhook event to its configured destination.",
			Tags:        []string{"webhook", "replay"},
		},
		requiredVerbs: []string{"replay_webhook_event"},
	},
}

// DeriveSkills returns the A2A skills advertised by a persona holding verbSubset, in catalog order. A
// skill is included only when the persona holds every verb the skill requires (all-of), so advertised
// capability can never exceed vended capability: a persona lacking create_for never advertises
// delegate-work, and adding or removing a required verb recomputes the result. The return is always
// non-nil (empty when the subset backs no skill), and each call yields fresh Tags/Skill copies so a
// caller can never mutate the shared catalog. Governing: ADR-0009 (skills derived from capability),
// SPEC-0009 REQ "Skills Derived From Vended Capability".
func DeriveSkills(verbSubset []string) []Skill {
	held := make(map[string]struct{}, len(verbSubset))
	for _, v := range verbSubset {
		held[v] = struct{}{}
	}

	out := make([]Skill, 0, len(skillCatalog))
	for _, def := range skillCatalog {
		if holdsAll(held, def.requiredVerbs) {
			out = append(out, cloneSkill(def.skill))
		}
	}
	return out
}

// SkillIDs returns just the ids of the derived skills, sorted, for terse comparisons and logging. It is
// a convenience over DeriveSkills for callers (and tests) that care only about which skills are
// advertised, not their full descriptors.
func SkillIDs(verbSubset []string) []string {
	skills := DeriveSkills(verbSubset)
	ids := make([]string, 0, len(skills))
	for _, s := range skills {
		ids = append(ids, s.ID)
	}
	sort.Strings(ids)
	return ids
}

// holdsAll reports whether held contains every verb in required. An empty required set is vacuously
// satisfied, but no catalog entry uses one — every advertised skill is backed by at least one verb.
func holdsAll(held map[string]struct{}, required []string) bool {
	for _, v := range required {
		if _, ok := held[v]; !ok {
			return false
		}
	}
	return true
}

// cloneSkill returns a deep copy of s so the shared catalog's slices can never be aliased or mutated by
// a caller.
func cloneSkill(s Skill) Skill {
	c := s
	if s.Tags != nil {
		c.Tags = append([]string(nil), s.Tags...)
	}
	return c
}

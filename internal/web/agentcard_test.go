package web

// Pure (DB-free) tests for the A2A Agent Card projection: the authoritative verb→skill derivation
// and the persona→card mapping. These assert the SPEC-0009 structural invariants — advertised skills
// are a function of verb_subset, and the card carries no owner PII or capability slice — without a
// database. The DB-backed handler behavior (discoverable → 200 card, unpublished/unknown → 404) lives
// in the server package's card_endpoint_test.go.
// Governing: SPEC-0009 REQ "Skills Derived From Vended Capability", REQ "Agent Card Mapping".

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/joestump/switchboard/internal/store"
)

func skillIDs(skills []agentSkill) []string {
	ids := make([]string, len(skills))
	for i, s := range skills {
		ids[i] = s.ID
	}
	return ids
}

func hasSkill(skills []agentSkill, id string) bool {
	for _, s := range skills {
		if s.ID == id {
			return true
		}
	}
	return false
}

// A skill is advertised only when EVERY verb it requires is present in the verb_subset (all-of
// semantics); a skill whose required verb is absent never appears. Governing: SPEC-0009 REQ "Skills
// Derived From Vended Capability" (scenario "A skill requiring an ungranted verb is never advertised").
func TestDeriveSkillsAllOfSemantics(t *testing.T) {
	// Full drain grant → process-work; no create_for → delegate-work absent.
	skills := deriveSkills([]string{"list_todos", "claim", "complete"})
	if !hasSkill(skills, "process-work") {
		t.Errorf("list_todos+claim+complete must derive process-work, got %v", skillIDs(skills))
	}
	if hasSkill(skills, "delegate-work") {
		t.Errorf("without create_for, delegate-work must NOT appear, got %v", skillIDs(skills))
	}

	// Partial grant (missing complete) → process-work must NOT appear (all-of, not any-of).
	partial := deriveSkills([]string{"list_todos", "claim"})
	if hasSkill(partial, "process-work") {
		t.Errorf("process-work requires all of list_todos+claim+complete; partial grant must not derive it, got %v",
			skillIDs(partial))
	}

	// create_for → delegate-work.
	deleg := deriveSkills([]string{"create_for"})
	if !hasSkill(deleg, "delegate-work") {
		t.Errorf("create_for must derive delegate-work, got %v", skillIDs(deleg))
	}

	// Empty subset → no skills, but never a nil slice (deterministic JSON []).
	empty := deriveSkills(nil)
	if len(empty) != 0 || empty == nil {
		t.Errorf("empty verb_subset must derive an empty, non-nil skill slice, got %#v", empty)
	}
}

// Changing the verb_subset changes the derived skills so the card reflects only currently-granted
// capability. Governing: SPEC-0009 REQ "Skills Derived From Vended Capability" (scenario "Changing
// the subset changes the card").
func TestDeriveSkillsChangesWithSubset(t *testing.T) {
	before := deriveSkills([]string{"list_todos", "claim", "complete"})
	after := deriveSkills([]string{"list_todos", "claim", "complete", "create_for"})
	if hasSkill(before, "delegate-work") {
		t.Fatalf("precondition: delegate-work should be absent before granting create_for")
	}
	if !hasSkill(after, "delegate-work") {
		t.Fatalf("adding create_for must add delegate-work to the derived set, got %v", skillIDs(after))
	}
}

// personaCard maps persona fields onto the A2A card shape and derives skills from the verb_subset,
// and its provider ties to the owning human's display name — never the agent's self-asserted id.
// Governing: SPEC-0009 REQ "Agent Card Mapping" (scenario "Card provider ties to the owning human").
func TestPersonaCardMapping(t *testing.T) {
	dp := store.DiscoverablePersona{
		Persona: store.Persona{
			ID:          "11111111-1111-1111-1111-111111111111",
			Name:        "Reviewer",
			Description: "Reviews pull requests.",
			VerbSubset:  []string{"list_todos", "claim", "complete"},
			Queues:      []string{"reviews"},
		},
		OwnerDisplayName: "Ada Lovelace",
	}
	card := personaCard("https://sb.example.com/", dp)

	if card.Name != "Reviewer" {
		t.Errorf("card.Name = %q, want persona name", card.Name)
	}
	if card.Description != "Reviews pull requests." {
		t.Errorf("card.Description = %q, want persona description", card.Description)
	}
	if card.URL != "https://sb.example.com/a/"+dp.ID+"/" {
		t.Errorf("card.URL = %q, want the persona discovery base path", card.URL)
	}
	if card.Provider.Organization != "Ada Lovelace" {
		t.Errorf("card.Provider.Organization = %q, want the owning human's display name", card.Provider.Organization)
	}
	if card.ProtocolVersion != a2aProtocolVersion {
		t.Errorf("card.ProtocolVersion = %q, want %q", card.ProtocolVersion, a2aProtocolVersion)
	}
	if !hasSkill(card.Skills, "process-work") {
		t.Errorf("card must advertise the derived process-work skill, got %v", skillIDs(card.Skills))
	}
	// Capabilities are discovery-only: no delegation intake advertised.
	if card.Capabilities.Streaming || card.Capabilities.PushNotifications || card.Capabilities.StateTransitionHistory {
		t.Errorf("card capabilities must be discovery-only (all false), got %+v", card.Capabilities)
	}
}

// When a persona has no explicit description, the card falls back to a bounded single-line summary of
// its system prompt — never the whole prompt. Governing: SPEC-0009 REQ "Agent Card Mapping".
func TestPersonaCardDescriptionFallbackToSummary(t *testing.T) {
	long := strings.Repeat("word ", 100) // 500 chars, single line
	dp := store.DiscoverablePersona{
		Persona: store.Persona{
			ID:           "22222222-2222-2222-2222-222222222222",
			Name:         "Summarized",
			SystemPrompt: long + "\nsecond line should not appear",
		},
	}
	card := personaCard("https://sb.example.com", dp)
	if card.Description == "" {
		t.Fatal("empty description must fall back to a system-prompt summary")
	}
	if strings.Contains(card.Description, "second line") {
		t.Errorf("summary must be a single line, got %q", card.Description)
	}
	if len([]rune(card.Description)) > 210 { // 200 + ellipsis slack
		t.Errorf("summary must be bounded, got %d runes", len([]rune(card.Description)))
	}
}

// The projected card, once serialized, must never leak owner PII (email, OIDC subject) or the
// persona's capability slice (verb_subset/queues/system_prompt). It carries only outward-facing
// discovery metadata. Governing: SPEC-0009 REQ "Agent Card Mapping"; #59 AC "not leaking owner PII".
func TestPersonaCardOmitsPIIAndCapabilitySlice(t *testing.T) {
	dp := store.DiscoverablePersona{
		Persona: store.Persona{
			ID:           "33333333-3333-3333-3333-333333333333",
			OwnerHumanID: "owner-uuid",
			AgentID:      "agent-uuid",
			Name:         "Deployer",
			Description:  "Ships approved builds.",
			SystemPrompt: "SECRET-PROMPT-do-not-leak",
			VerbSubset:   []string{"list_todos", "claim", "complete", "create_for"},
			Queues:       []string{"deploys-SECRET-QUEUE"},
		},
		OwnerDisplayName: "Grace Hopper",
	}
	body, err := json.Marshal(personaCard("https://sb.example.com", dp))
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	s := string(body)
	for _, leak := range []string{
		"owner-uuid", "agent-uuid", "SECRET-PROMPT", "deploys-SECRET-QUEUE",
		"verb_subset", "verbSubset", "systemPrompt", "system_prompt", "ownerHumanID", "oidc",
	} {
		if strings.Contains(s, leak) {
			t.Errorf("card JSON leaked %q: %s", leak, s)
		}
	}
	// Sanity: the intended discovery fields ARE present.
	for _, want := range []string{"Deployer", "Ships approved builds.", "Grace Hopper", "process-work", "delegate-work"} {
		if !strings.Contains(s, want) {
			t.Errorf("card JSON missing expected discovery field %q: %s", want, s)
		}
	}
}

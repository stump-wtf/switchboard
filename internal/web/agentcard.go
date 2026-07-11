package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/store"
)

// a2aProtocolVersion is the A2A protocol version the published Agent Cards conform to. Switchboard
// implements only the discovery half of A2A (an owner-approved public card); it does NOT implement
// the JSON-RPC task-delegation surface, which the card's capabilities advertise as absent.
const a2aProtocolVersion = "0.3.0"

// agentSkill is one entry of an A2A Agent Card's `skills` array: a stable id, a human name, a short
// description, and tags. Governing: SPEC-0009 REQ "Skills Derived From Vended Capability".
type agentSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}

// skillDerivation is one row of the authoritative verb→skill map: a skill is advertised on a
// persona's card only when EVERY verb in requiredVerbs is present in the persona's verb_subset
// (all-of semantics). This makes over-advertisement structurally impossible — a persona physically
// cannot advertise a skill outside its vended grant.
// Governing: SPEC-0009 REQ "Skills Derived From Vended Capability".
type skillDerivation struct {
	skill         agentSkill
	requiredVerbs []string
}

// verbSkillMap is the authoritative verb→skill map maintained with the code (SPEC-0009 / ADR-0009:
// the map is canonical in code with a covering test). Each skill lists the verbs it requires; a skill
// whose required verbs are not all present in a persona's verb_subset MUST NOT appear on its card.
// The map is ordered so derived skills come out deterministically. Until #58 lands a dedicated
// derivation package, this inline map is the single source of truth; keep it in sync as verbs change.
var verbSkillMap = []skillDerivation{
	{
		skill: agentSkill{
			ID:          "process-work",
			Name:        "Process work",
			Description: "Drains todos from its queues: lists pending work, claims it under a lease, and reports completion.",
			Tags:        []string{"todo", "queue", "worker"},
		},
		requiredVerbs: []string{"list_todos", "claim", "complete"},
	},
	{
		skill: agentSkill{
			ID:          "delegate-work",
			Name:        "Delegate work",
			Description: "Creates todos on behalf of other principals, routing work into their queues.",
			Tags:        []string{"todo", "delegation"},
		},
		requiredVerbs: []string{"create_for"},
	},
	{
		skill: agentSkill{
			ID:          "replay-events",
			Name:        "Replay inbound events",
			Description: "Replays a previously verified inbound webhook event to its configured destination.",
			Tags:        []string{"webhook", "replay"},
		},
		requiredVerbs: []string{"replay_webhook_event"},
	},
	{
		skill: agentSkill{
			ID:          "inspect-providers",
			Name:        "Inspect providers",
			Description: "Enumerates the configured inbound providers and their trust modes (never their secrets).",
			Tags:        []string{"providers", "read-only"},
		},
		requiredVerbs: []string{"list_providers"},
	},
}

// deriveSkills computes the advertised skills for a verb_subset by applying the authoritative
// verb→skill map: a skill is included only when every one of its required verbs is present. The
// result is a fresh slice (never nil) in map order so the card is deterministic.
// Governing: SPEC-0009 REQ "Skills Derived From Vended Capability".
func deriveSkills(verbSubset []string) []agentSkill {
	granted := make(map[string]struct{}, len(verbSubset))
	for _, v := range verbSubset {
		granted[v] = struct{}{}
	}
	out := make([]agentSkill, 0, len(verbSkillMap))
	for _, d := range verbSkillMap {
		allPresent := true
		for _, rv := range d.requiredVerbs {
			if _, ok := granted[rv]; !ok {
				allPresent = false
				break
			}
		}
		if allPresent {
			out = append(out, d.skill)
		}
	}
	return out
}

// agentCardCapabilities is the A2A `capabilities` object. Switchboard advertises ONLY discovery: no
// streaming, no push notifications, no state-transition history, and — by omission of any JSON-RPC
// interface — no direct task delegation. Work never flows through the card URL; it arrives as todos
// (ADR-0010). Governing: SPEC-0009 REQ "Agent Card Mapping" (scenario "Card does not advertise
// delegation intake").
type agentCardCapabilities struct {
	Streaming              bool `json:"streaming"`
	PushNotifications      bool `json:"pushNotifications"`
	StateTransitionHistory bool `json:"stateTransitionHistory"`
}

// agentCardProvider ties a card to the owning human's identity chain. It carries only the owner's
// display name and the switchboard base URL — never owner PII (email, OIDC subject).
// Governing: SPEC-0009 REQ "Agent Card Mapping" (scenario "Card provider ties to the owning human").
type agentCardProvider struct {
	Organization string `json:"organization"`
	URL          string `json:"url"`
}

// agentCard is a schema-shaped A2A Agent Card. Only outward-facing, owner-approved discovery metadata
// appears: persona name/description, the discovery URL, owner provenance, derived skills, and the
// discovery-only capabilities. No secret, credential, verb_subset, queue, or owner PII is projected.
// Governing: SPEC-0009 REQ "Agent Card Mapping", REQ "Well-Known Card Endpoint".
type agentCard struct {
	ProtocolVersion   string                `json:"protocolVersion"`
	Name              string                `json:"name"`
	Description       string                `json:"description"`
	URL               string                `json:"url"`
	Provider          agentCardProvider     `json:"provider"`
	Version           string                `json:"version"`
	Capabilities      agentCardCapabilities `json:"capabilities"`
	DefaultInputModes []string              `json:"defaultInputModes"`
	DefaultOutputMode []string              `json:"defaultOutputModes"`
	Skills            []agentSkill          `json:"skills"`
}

// cardVersion is the persona-card schema version switchboard stamps on every card. Personas do not
// carry a semantic runtime version, so the card advertises a fixed publisher version.
const cardVersion = "1.0.0"

// summarizeSystemPrompt derives a short, single-line card description from a persona's human-authored
// system prompt when no explicit description is set. It takes the first non-empty line and bounds it
// so the card never dumps an arbitrarily long prompt. Governing: SPEC-0009 REQ "Agent Card Mapping"
// (description from the persona description or a summary of its system_prompt).
func summarizeSystemPrompt(prompt string) string {
	const maxLen = 200
	line := prompt
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if len(line) > maxLen {
		// Trim on a rune boundary near maxLen, then append an ellipsis.
		cut := maxLen
		for cut > 0 && !isUTF8Start(line[cut]) {
			cut--
		}
		line = strings.TrimSpace(line[:cut]) + "…"
	}
	return line
}

// isUTF8Start reports whether b is a UTF-8 sequence start byte (not a continuation byte 10xxxxxx).
func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// personaCard projects a discoverable persona into its A2A Agent Card. It is a pure function of the
// persona plus the switchboard base URL, so it is fully unit-testable without a database. The card's
// name maps from the persona name; description from the persona description or a system-prompt
// summary; url from the persona's discovery base path; provider from the owning human's display name;
// and skills from the derived set. Governing: SPEC-0009 REQ "Agent Card Mapping".
func personaCard(baseURL string, p store.DiscoverablePersona) agentCard {
	base := strings.TrimSuffix(baseURL, "/")
	description := p.Description
	if description == "" {
		description = summarizeSystemPrompt(p.SystemPrompt)
	}
	org := p.OwnerDisplayName
	if org == "" {
		org = "switchboard operator"
	}
	return agentCard{
		ProtocolVersion: a2aProtocolVersion,
		Name:            p.Name,
		Description:     description,
		// Discovery identifier only — the card URL is NOT a work-intake channel (ADR-0010).
		URL:               base + "/a/" + p.ID + "/",
		Provider:          agentCardProvider{Organization: org, URL: base},
		Version:           cardVersion,
		Capabilities:      agentCardCapabilities{}, // discovery-only: all false
		DefaultInputModes: []string{"application/json"},
		DefaultOutputMode: []string{"application/json"},
		Skills:            deriveSkills(p.VerbSubset),
	}
}

// AgentCard serves a persona's public A2A Agent Card at /a/{persona_id}/.well-known/agent-card.json.
// It is the ONLY public (unauthenticated) route in the personas capability: A2A discovery requires
// peers to read a card before any friendship exists, and the card grants nothing — it exposes only
// owner-approved metadata (name, description, derived skills, owner display name). It is served ONLY
// for personas the owner has marked discoverable; an unknown id, a malformed id, or a
// non-discoverable persona all return 404 with no body detail, so the endpoint never leaks the
// existence of an unpublished persona. Read-only: the route is registered for GET only, so a
// state-changing method against it is rejected by the router (405).
// Governing: SPEC-0009 REQ "Well-Known Card Endpoint", REQ "Discoverability Is Owner-Controlled",
// REQ "Agent Card Mapping"; SPEC-0009 "Security Requirements" (public read-only, per-IP rate limit,
// default-src 'none' CSP).
func (h *Handler) AgentCard(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "persona_id")
	dp, err := h.store.GetDiscoverablePersona(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.fail(w, err)
		return
	}

	card := personaCard(h.cfg.BaseURL, dp)
	body, err := json.Marshal(card)
	if err != nil {
		h.fail(w, err)
		return
	}

	// The card is inert JSON: tighten the CSP to default-src 'none' (overriding the app-wide
	// default-src 'self' from secureHeaders) since the endpoint serves no active content.
	// Governing: SPEC-0009 "Security Headers".
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

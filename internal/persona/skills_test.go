package persona

import (
	"reflect"
	"testing"
)

// ids is a test helper: the ids of the skills derived from verbSubset, in catalog order.
func ids(verbSubset []string) []string {
	skills := DeriveSkills(verbSubset)
	out := make([]string, len(skills))
	for i, s := range skills {
		out[i] = s.ID
	}
	return out
}

// TestDeriveSkills_AllOfSemantics asserts a skill appears only when the persona holds every verb it
// requires. Governing: SPEC-0009 REQ "Skills Derived From Vended Capability".
func TestDeriveSkills_AllOfSemantics(t *testing.T) {
	cases := []struct {
		name  string
		verbs []string
		want  []string
	}{
		{"empty subset advertises nothing", nil, []string{}},
		{"full drain trio advertises process-work",
			[]string{"list_todos", "claim", "complete"}, []string{"process-work"}},
		{"partial drain trio advertises nothing",
			[]string{"list_todos", "claim"}, []string{}},
		{"claim+complete without list_todos advertises nothing",
			[]string{"claim", "complete"}, []string{}},
		{"create_for advertises delegate-work",
			[]string{"create_for"}, []string{"delegate-work"}},
		{"replay advertises replay-events",
			[]string{"replay_webhook_event"}, []string{"replay-events"}},
		{"unrelated verbs advertise nothing",
			[]string{"heartbeat", "fail", "list_webhook_events", "get_webhook_event"}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ids(tc.verbs)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DeriveSkills(%v) ids = %v, want %v", tc.verbs, got, tc.want)
			}
		})
	}
}

// TestDeriveSkills_MissingRequiredVerbNeverAdvertised is the spec's named scenario: a persona whose
// verb_subset lacks create_for must never advertise delegate-work, even alongside other granted verbs.
// Governing: SPEC-0009 REQ "Skills Derived From Vended Capability" (scenario "A skill requiring an
// ungranted verb is never advertised").
func TestDeriveSkills_MissingRequiredVerbNeverAdvertised(t *testing.T) {
	verbs := []string{"list_todos", "claim", "complete", "fail", "heartbeat"} // no create_for
	for _, s := range DeriveSkills(verbs) {
		if s.ID == "delegate-work" {
			t.Fatalf("delegate-work advertised without create_for in subset %v", verbs)
		}
	}
}

// TestDeriveSkills_AddingVerbChangesCard asserts adding a verb recomputes the advertised skills so the
// card reflects exactly the subset. Governing: SPEC-0009 REQ "Skills Derived From Vended Capability"
// (scenario "Changing the subset changes the card").
func TestDeriveSkills_AddingVerbChangesCard(t *testing.T) {
	base := []string{"list_todos", "claim", "complete"}
	before := ids(base)
	if !reflect.DeepEqual(before, []string{"process-work"}) {
		t.Fatalf("before = %v, want [process-work]", before)
	}

	withDelegate := append(append([]string{}, base...), "create_for")
	after := ids(withDelegate)
	if !reflect.DeepEqual(after, []string{"process-work", "delegate-work"}) {
		t.Fatalf("after adding create_for = %v, want [process-work delegate-work]", after)
	}
}

// TestDeriveSkills_RemovingVerbChangesCard asserts removing a required verb drops the skill it backed.
// Governing: SPEC-0009 REQ "Skills Derived From Vended Capability" (scenario "Changing the subset
// changes the card").
func TestDeriveSkills_RemovingVerbChangesCard(t *testing.T) {
	full := []string{"list_todos", "claim", "complete"}
	if got := ids(full); !reflect.DeepEqual(got, []string{"process-work"}) {
		t.Fatalf("full = %v, want [process-work]", got)
	}

	// Remove `complete`; process-work must vanish because it is no longer fully backed.
	reduced := []string{"list_todos", "claim"}
	if got := ids(reduced); len(got) != 0 {
		t.Fatalf("after removing complete = %v, want no skills", got)
	}
}

// TestDeriveSkills_Deterministic asserts the derivation is a pure function of the subset: order of the
// input verbs and duplicates do not change the result, and repeated calls are identical.
func TestDeriveSkills_Deterministic(t *testing.T) {
	a := []string{"complete", "list_todos", "claim", "create_for", "list_todos"}
	b := []string{"create_for", "claim", "complete", "list_todos"}
	want := []string{"process-work", "delegate-work"}
	if got := ids(a); !reflect.DeepEqual(got, want) {
		t.Fatalf("ids(a) = %v, want %v", got, want)
	}
	if got := ids(b); !reflect.DeepEqual(got, want) {
		t.Fatalf("ids(b) = %v, want %v", got, want)
	}
}

// TestDeriveSkills_ReturnsNonNilEmpty asserts an empty result is a non-nil zero-length slice so
// callers (and JSON encoders on the card) get `[]`, not `null`.
func TestDeriveSkills_ReturnsNonNilEmpty(t *testing.T) {
	got := DeriveSkills(nil)
	if got == nil {
		t.Fatal("DeriveSkills(nil) returned nil, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("DeriveSkills(nil) = %v, want empty", got)
	}
}

// TestDeriveSkills_ResultIsCopy asserts a caller mutating a returned skill's Tags cannot corrupt the
// shared catalog observed by later calls.
func TestDeriveSkills_ResultIsCopy(t *testing.T) {
	first := DeriveSkills([]string{"list_todos", "claim", "complete"})
	if len(first) == 0 || len(first[0].Tags) == 0 {
		t.Fatal("expected process-work with tags")
	}
	first[0].Tags[0] = "MUTATED"
	first[0].Description = "MUTATED"

	second := DeriveSkills([]string{"list_todos", "claim", "complete"})
	if second[0].Tags[0] == "MUTATED" || second[0].Description == "MUTATED" {
		t.Fatal("mutating a derived skill leaked into the shared catalog")
	}
}

// TestDeriveSkills_EveryCatalogSkillReachable asserts the union of all required verbs advertises every
// catalog skill — no skill is unreachable (a dead catalog entry that no subset can produce).
func TestDeriveSkills_EveryCatalogSkillReachable(t *testing.T) {
	var all []string
	for _, def := range skillCatalog {
		all = append(all, def.requiredVerbs...)
	}
	got := DeriveSkills(all)
	if len(got) != len(skillCatalog) {
		t.Fatalf("union of all required verbs derived %d skills, want %d (all catalog entries)",
			len(got), len(skillCatalog))
	}
}

// TestSkillIDs_Sorted asserts the SkillIDs convenience returns sorted ids regardless of catalog order.
func TestSkillIDs_Sorted(t *testing.T) {
	got := SkillIDs([]string{"create_for", "list_todos", "claim", "complete"})
	want := []string{"delegate-work", "process-work"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SkillIDs = %v, want %v (sorted)", got, want)
	}
}

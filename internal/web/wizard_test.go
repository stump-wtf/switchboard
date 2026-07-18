package web

// Unit contract for the wizard machinery (wizard.go): step ordering, server-side state lifecycle
// (begin/get/save/drop), TTL expiry, the copy-on-read guarantee, and the capacity cap. This is the
// reusable helper every SPEC-0015 wizard builds on, so its contract is pinned independently of the
// vend flow. Governing: SPEC-0015 REQ "Wizard Interaction Pattern", ADR-0018.

import (
	"fmt"
	"net/url"
	"testing"
	"time"
)

func TestWizardDefStepNavigation(t *testing.T) {
	d := wizardDef{name: "w", base: "/w", steps: []string{"a", "b", "c"}}
	if d.first() != "a" {
		t.Errorf("first = %q, want a", d.first())
	}
	for _, tc := range []struct{ slug, prev, next string }{
		{"a", "", "b"},
		{"b", "a", "c"},
		{"c", "b", ""},
	} {
		if got := d.prev(tc.slug); got != tc.prev {
			t.Errorf("prev(%s) = %q, want %q", tc.slug, got, tc.prev)
		}
		if got := d.next(tc.slug); got != tc.next {
			t.Errorf("next(%s) = %q, want %q", tc.slug, got, tc.next)
		}
	}
	if d.index("nope") != -1 {
		t.Error("index(unknown) should be -1")
	}
	if got := d.stepPath("b"); got != "/w/b" {
		t.Errorf("stepPath = %q", got)
	}
	if got := d.cookieName(); got != "sb_wiz_w" {
		t.Errorf("cookieName = %q", got)
	}
}

func TestWizardStatesLifecycle(t *testing.T) {
	s := newWizardStates(time.Minute)
	token, err := s.begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if token == "" {
		t.Fatal("begin returned an empty token")
	}
	values, ok := s.get(token)
	if !ok || len(values) != 0 {
		t.Fatalf("fresh state: ok=%v values=%v, want empty values", ok, values)
	}

	values.Set("name", "bot")
	values["queues"] = []string{"reviews", "deploys"}
	s.save(token, values)

	got, ok := s.get(token)
	if !ok || got.Get("name") != "bot" || len(got["queues"]) != 2 {
		t.Fatalf("saved state not preserved: ok=%v values=%v", ok, got)
	}

	// get returns a COPY: mutating the returned values must not corrupt the stored draft.
	got.Set("name", "mutated")
	again, _ := s.get(token)
	if again.Get("name") != "bot" {
		t.Error("get must return a copy — caller mutation leaked into the stored state")
	}

	s.drop(token)
	if _, ok := s.get(token); ok {
		t.Error("dropped state still resolvable")
	}
}

func TestWizardStatesExpire(t *testing.T) {
	s := newWizardStates(time.Minute)
	now := time.Now()
	s.now = func() time.Time { return now }
	token, err := s.begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Save refreshes the TTL: a completed step buys another full window.
	now = now.Add(50 * time.Second)
	s.save(token, url.Values{"name": {"bot"}})
	now = now.Add(50 * time.Second) // 100s after begin, 50s after save — still live
	if _, ok := s.get(token); !ok {
		t.Fatal("state expired despite the save-refreshed TTL")
	}
	now = now.Add(30 * time.Second) // 80s after the save — past the 60s TTL
	if _, ok := s.get(token); ok {
		t.Error("expired state still resolvable")
	}
	// Saving to an expired-but-unpruned token must not resurrect it for get… it may exist in the
	// map until pruned, but get enforces expiry.
	s.save(token, url.Values{"name": {"zombie"}})
	if _, ok := s.get(token); ok {
		t.Error("save resurrected an expired state")
	}
}

func TestWizardStatesCapEvictsClosestToExpiry(t *testing.T) {
	s := newWizardStates(time.Hour)
	now := time.Now()
	s.now = func() time.Time { return now }
	// Fill to the cap with staggered expiries (earlier begins expire sooner).
	tokens := make([]string, 0, maxWizardStates)
	for i := 0; i < maxWizardStates; i++ {
		now = now.Add(time.Millisecond)
		tok, err := s.begin()
		if err != nil {
			t.Fatalf("begin %d: %v", i, err)
		}
		tokens = append(tokens, tok)
	}
	// One more begin evicts exactly the entry closest to expiry (the first), not a newer one.
	if _, err := s.begin(); err != nil {
		t.Fatalf("begin at cap: %v", err)
	}
	if _, ok := s.get(tokens[0]); ok {
		t.Error("cap eviction kept the closest-to-expiry entry")
	}
	if _, ok := s.get(tokens[len(tokens)-1]); !ok {
		t.Error("cap eviction dropped a fresh entry")
	}
	if len(s.m) > maxWizardStates {
		t.Errorf("table size %d exceeds cap %d", len(s.m), maxWizardStates)
	}
}

func TestWizardTokensAreUnique(t *testing.T) {
	s := newWizardStates(time.Minute)
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		tok, err := s.begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if seen[tok] {
			t.Fatalf("duplicate wizard token minted: %s", tok)
		}
		seen[tok] = true
	}
}

func TestFirstIncompleteVendStep(t *testing.T) {
	for i, tc := range []struct {
		values url.Values
		want   string
	}{
		{url.Values{}, "persona"},
		{url.Values{"name": {"  "}}, "persona"},
		{url.Values{"name": {"bot"}}, "queues"},
		{url.Values{"name": {"bot"}, "queues": {"reviews"}}, "verbs"},
		{url.Values{"name": {"bot"}, "queues": {"reviews"}, "verbs": {"claim"}}, ""},
	} {
		if got := firstIncompleteVendStep(tc.values); got != tc.want {
			t.Errorf("case %d: firstIncompleteVendStep = %q, want %q", i, got, tc.want)
		}
	}
}

func TestDedupe(t *testing.T) {
	got := dedupe([]string{"a", "b", "a", "c", "b"})
	if fmt.Sprint(got) != "[a b c]" {
		t.Errorf("dedupe = %v", got)
	}
	if dedupe(nil) != nil {
		t.Error("dedupe(nil) should be nil")
	}
}

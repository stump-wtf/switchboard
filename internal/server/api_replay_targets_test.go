// Owned replay targets on the operator API (POST/GET /api/v1/endpoints): a vend may name the
// endpoint's replay targets, each checked by the shared SSRF guard before anything is minted, and
// only the owning human ever reads them back. The DB-backed cases skip without
// SWITCHBOARD_TEST_DATABASE_URL. Governing: ADR-0038, SPEC-0033 REQ "Owned Replay Targets" (F9).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/push"
)

type refusingResolver struct{}

func (refusingResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return nil, errors.New("no DNS in this test")
}

func TestNormalizeReplayTargets(t *testing.T) {
	ctx := context.Background()
	v := push.New(push.WithResolver(refusingResolver{}))

	got, err := normalizeReplayTargets(ctx, v, []string{" https://203.0.113.10/a ", "", "https://203.0.113.10/a", "https://203.0.113.11/b"})
	if err != nil || !slices.Equal(got, []string{"https://203.0.113.10/a", "https://203.0.113.11/b"}) {
		t.Fatalf("normalize = %v, %v; want trimmed, de-duplicated, in order", got, err)
	}
	if got, err := normalizeReplayTargets(ctx, v, nil); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("no targets = %#v, %v; want an empty, non-nil list", got, err)
	}

	many := make([]string, 0, maxReplayTargets+1)
	for i := 0; i <= maxReplayTargets; i++ {
		many = append(many, fmt.Sprintf("https://203.0.113.%d/", i+1))
	}
	for name, in := range map[string][]string{
		"loopback":        {"https://127.0.0.1/"},
		"private":         {"https://10.0.0.5/hook"},
		"metadata":        {"https://169.254.169.254/latest/meta-data/"},
		"plain http":      {"http://203.0.113.10/"},
		"unresolvable":    {"https://replay.example/"},
		"relative":        {"/hook"},
		"too long":        {"https://203.0.113.10/" + strings.Repeat("a", maxReplayTargetLen)},
		"too many":        many,
		"one bad of many": {"https://203.0.113.10/", "https://192.168.1.1/"},
	} {
		if got, err := normalizeReplayTargets(ctx, v, in); err == nil {
			t.Errorf("%s: accepted %v, want refusal", name, got)
		}
	}
}

func TestAPIVendOwnsReplayTargetsAndOnlyItsOwnerSeesThem(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	ownerA, _ := mintSession(t, st, ctx, "test|replay-a", "Owner A", "a@example.com")
	ownerB, _ := mintSession(t, st, ctx, "test|replay-b", "Owner B", "b@example.com")
	bearerA := mintOperatorBearer(t, st, ctx, ownerA.ID, "cid-replay-a")
	bearerB := mintOperatorBearer(t, st, ctx, ownerB.ID, "cid-replay-b")
	const target = "https://203.0.113.10/a-only-hook"

	rec := apiCall(t, r, http.MethodPost, "/api/v1/endpoints", bearerA,
		`{"name":"replayer","replay_targets":[" `+target+` ","`+target+`",""]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("vend: got %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var vended struct {
		Slug          string   `json:"slug"`
		ReplayTargets []string `json:"replay_targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &vended); err != nil {
		t.Fatalf("decode vend: %v", err)
	}
	if !slices.Equal(vended.ReplayTargets, []string{target}) {
		t.Fatalf("vended replay_targets = %v, want [%s]", vended.ReplayTargets, target)
	}

	// Stored on the endpoint the vend minted, readable through the store's endpoint-keyed read.
	cards, err := st.ListEndpointCards(ctx, ownerA.ID)
	if err != nil || len(cards) != 1 {
		t.Fatalf("A's cards = %v, %v", cards, err)
	}
	if got, err := st.EndpointReplayTargets(ctx, cards[0].ID); err != nil || !slices.Equal(got, []string{target}) {
		t.Fatalf("stored targets = %v, %v", got, err)
	}

	// A lists its own targets; B's listing carries neither A's endpoint nor A's target.
	recA := apiCall(t, r, http.MethodGet, "/api/v1/endpoints", bearerA, "")
	if recA.Code != http.StatusOK || !strings.Contains(recA.Body.String(), target) {
		t.Fatalf("A's listing = %d %s, want A's target", recA.Code, recA.Body.String())
	}
	recB := apiCall(t, r, http.MethodGet, "/api/v1/endpoints", bearerB, "")
	if recB.Code != http.StatusOK || strings.Contains(recB.Body.String(), target) ||
		strings.Contains(recB.Body.String(), vended.Slug) {
		t.Fatalf("B's listing = %d %s, leaked A's endpoint or target", recB.Code, recB.Body.String())
	}
}

// A target the guard refuses rejects the whole vend with 400 before anything is minted.
func TestAPIVendRefusesUnsafeReplayTargetAndMintsNothing(t *testing.T) {
	r, st, ctx := newDBRouter(t)
	owner, _ := mintSession(t, st, ctx, "test|replay-refused", "Owner", "o@example.com")
	bearer := mintOperatorBearer(t, st, ctx, owner.ID, "cid-replay-refused")

	for _, target := range []string{"https://10.0.0.5/hook", "https://127.0.0.1:8080/", "http://203.0.113.10/"} {
		rec := apiCall(t, r, http.MethodPost, "/api/v1/endpoints", bearer,
			`{"name":"refused","replay_targets":["`+target+`"]}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400 (body %s)", target, rec.Code, rec.Body.String())
		}
	}
	if cards, err := st.ListEndpointCards(ctx, owner.ID); err != nil || len(cards) != 0 {
		t.Fatalf("a refused vend minted %d endpoint(s) (%v)", len(cards), err)
	}
	if agents, err := st.ListAgents(ctx, owner.ID); err != nil || len(agents) != 0 {
		t.Fatalf("a refused vend minted %d agent(s) (%v)", len(agents), err)
	}
}

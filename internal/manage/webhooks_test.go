package manage

// Webhook lifecycle policy, against an in-memory store: the refusals that must happen before a write,
// who is handed a minted secret, and the one case where a secret is deliberately never echoed. The
// real-store coverage (ceiling counts under concurrency, ownership in the WHERE clause) lives with the
// store and the two adapters that call it.
//
// Governing: SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling"; SPEC-0032 REQ-1, REQ-2,
// REQ-3; SPEC-0035 REQ "Shared Implementation With MCP"; SPEC-0026 REQ-6.
//
// @joestump-agent 10/22/2026 - Added with webhooks.go (stump.wtf/switchboard#584).

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/sourcekind"
	"github.com/stump-wtf/switchboard/internal/store"
)

// memWebhooks records what reached the store and answers from a small row set.
type memWebhooks struct {
	creates   []store.CreateWebhookInput
	rows      map[string]store.Webhook
	rotations []string
	deletions []string
}

func newMemWebhooks() *memWebhooks {
	return &memWebhooks{rows: map[string]store.Webhook{
		hookW1: {ID: hookW1, EndpointID: epE1, SourceType: "github", TargetQueue: "q",
			TrustMode: sourcekind.TrustSigned, SecretOrigin: sourcekind.OriginMinted, SecretSet: true},
		hookStripe: {ID: hookStripe, EndpointID: epE1, SourceType: "stripe", TargetQueue: "q",
			TrustMode: sourcekind.TrustSigned, SecretOrigin: sourcekind.OriginProvider, SecretSet: true},
		hookGeneric: {ID: hookGeneric, EndpointID: epE1, SourceType: "generic", TargetQueue: "q",
			TrustMode: sourcekind.TrustToken, SecretOrigin: sourcekind.OriginNone},
	}}
}

const (
	hookStripe  = "webhook-stripe"
	hookGeneric = "webhook-generic"
	epCeiling   = "endpoint-ceiling"
)

func (m *memWebhooks) CreateWebhookWithOrigin(_ context.Context, in store.CreateWebhookInput) (store.Webhook, error) {
	m.creates = append(m.creates, in)
	return store.Webhook{ID: "webhook-new", EndpointID: in.EndpointID, SourceType: in.SourceType,
		TargetQueue: in.TargetQueue, TrustMode: in.TrustMode, SecretSet: in.Secret != "",
		SecretOrigin: originOf(in)}, nil
}

func (m *memWebhooks) RotateWebhookSecret(_ context.Context, id, endpointID, secret, ingestToken string) (store.Webhook, error) {
	row, ok := m.rows[id]
	if !ok || row.EndpointID != endpointID {
		return store.Webhook{}, store.ErrNotFound
	}
	m.rotations = append(m.rotations, id)
	row.IngestToken = ingestToken
	return row, nil
}

func (m *memWebhooks) DeleteWebhook(_ context.Context, id, endpointID string) error {
	row, ok := m.rows[id]
	if !ok || row.EndpointID != endpointID {
		return store.ErrNotFound
	}
	m.deletions = append(m.deletions, id)
	return nil
}

func (m *memWebhooks) WebhookForEndpoint(_ context.Context, id, endpointID string) (store.Webhook, error) {
	row, ok := m.rows[id]
	if !ok || row.EndpointID != endpointID {
		return store.Webhook{}, store.ErrNotFound
	}
	return row, nil
}

// originOf mirrors the store's derivation well enough for these tests: a minted-origin create with a
// secret is minted, a supplied one is supplied, and a provider source keeps its own.
func originOf(in store.CreateWebhookInput) string {
	switch {
	case in.SecretSupplied:
		return "supplied"
	case in.Secret == "":
		return sourcekind.OriginNone
	default:
		return sourcekind.OriginMinted
	}
}

// memWebhookHumans resolves a webhook's owning endpoint for a human caller, and refuses a webhook that
// belongs to someone else, which is the store's contract: not found rather than "not yours".
type memWebhookHumans struct{ rows map[string]memHookOwner }

type memHookOwner struct{ endpointID, humanID string }

func (m memWebhookHumans) WebhookOwnerForHuman(_ context.Context, webhookID, humanID string) (store.WebhookOwner, error) {
	owner, ok := m.rows[webhookID]
	if !ok || owner.endpointID == "" || owner.humanID != humanID {
		return store.WebhookOwner{}, store.ErrNotFound
	}
	return store.WebhookOwner{EndpointID: owner.endpointID, EndpointSlug: "e1", EndpointState: "active"}, nil
}

func TestCreateRefusesBeforeWritingAnything(t *testing.T) {
	st := newMemWebhooks()
	w := Webhooks{Store: st}
	ceiling := Ceiling{EndpointID: epCeiling, EndpointState: "active", WebhookMax: 2,
		SourceTypes: []string{"github", "stripe", "generic"}, WebhookQueues: []string{"q"}}

	for _, tcase := range []struct {
		name string
		in   CreateIn
		want Kind
	}{
		{"no source type", CreateIn{TargetQueue: "q"}, ErrInvalidArgument},
		{"no target queue", CreateIn{SourceType: "github"}, ErrInvalidArgument},
		{"the reserved queue", CreateIn{SourceType: "github", TargetQueue: "quarantine"}, ErrInvalidArgument},
		{"a source type outside the ceiling", CreateIn{SourceType: "gitea", TargetQueue: "q"}, ErrForbiddenSourceType},
		{"a queue outside the ceiling", CreateIn{SourceType: "github", TargetQueue: "other"}, ErrForbidden},
		{"a secret on a token source", CreateIn{SourceType: "generic", TargetQueue: "q", SigningSecret: strPtr("whsec_x")}, ErrInvalidArgument},
		{"a secret switchboard would mint", CreateIn{SourceType: "github", TargetQueue: "q", SigningSecret: strPtr("whsec_x")}, ErrInvalidArgument},
		{"a malformed provider secret", CreateIn{SourceType: "stripe", TargetQueue: "q", SigningSecret: strPtr("not-a-stripe-secret")}, ErrInvalidArgument},
	} {
		_, err := w.Create(context.Background(), EndpointPrincipal(epCeiling), ceiling, store.SecretActor{}, tcase.in)
		var me *Error
		if !errors.As(err, &me) || me.Kind != tcase.want {
			t.Errorf("%s: err = %v, want kind %s", tcase.name, err, tcase.want)
		}
	}
	if len(st.creates) != 0 {
		t.Fatalf("%d creates reached the store for refused inputs", len(st.creates))
	}
}

// TestCreateRefusesANonActiveEndpoint pins the endpoint-state check inside Create itself, not only in
// the route handler that happens to check first today: a surface that forgets the handler's check must
// still fail closed here, because a webhook minted on a revoked or paused endpoint delivers nothing.
func TestCreateRefusesANonActiveEndpoint(t *testing.T) {
	for _, tcase := range []struct {
		name  string
		state string
	}{
		{"revoked", "revoked"},
		{"paused", "paused"},
		{"unset", ""},
	} {
		st := newMemWebhooks()
		w := Webhooks{Store: st}
		ceiling := Ceiling{EndpointID: epCeiling, EndpointState: tcase.state, WebhookMax: 2,
			SourceTypes: []string{"github"}, WebhookQueues: []string{"q"}}
		_, err := w.Create(context.Background(), EndpointPrincipal(epCeiling), ceiling, store.SecretActor{},
			CreateIn{SourceType: "github", TargetQueue: "q"})
		var me *Error
		if !errors.As(err, &me) || me.Kind != ErrConflict {
			t.Errorf("%s: err = %v, want conflict", tcase.name, err)
			continue
		}
		if me.State != tcase.state {
			t.Errorf("%s: State = %q, want %q", tcase.name, me.State, tcase.state)
		}
		if len(st.creates) != 0 {
			t.Errorf("%s: %d creates reached the store for a non-active endpoint", tcase.name, len(st.creates))
		}
	}
}

func TestCreateMintsWhereTheSourceTypeSaysTo(t *testing.T) {
	st := newMemWebhooks()
	w := Webhooks{Store: st}
	ceiling := Ceiling{EndpointID: epCeiling, EndpointState: "active", WebhookMax: 2,
		SourceTypes: []string{"github", "stripe", "generic"}, WebhookQueues: []string{"q"}}

	// A minted-origin source gets a secret switchboard holds and reveals once.
	created, err := w.Create(context.Background(), EndpointPrincipal(epCeiling), ceiling, store.SecretActor{},
		CreateIn{SourceType: "github", TargetQueue: "q"})
	if err != nil {
		t.Fatalf("create github: %v", err)
	}
	if !strings.HasPrefix(created.Secret, "whsec_") {
		t.Errorf("minted secret = %q, want the whsec_ form the producer pastes", created.Secret)
	}
	if st.creates[0].Secret != created.Secret || st.creates[0].EndpointID != epCeiling {
		t.Errorf("stored create = %+v, want the minted secret on the endpoint the ceiling names", st.creates[0])
	}

	// A provider-origin source is created with no secret at all: awaiting the producer's own value is
	// honest, a value nobody signs with is not.
	created, err = w.Create(context.Background(), EndpointPrincipal(epCeiling), ceiling, store.SecretActor{},
		CreateIn{SourceType: "stripe", TargetQueue: "q"})
	if err != nil {
		t.Fatalf("create stripe: %v", err)
	}
	if created.Secret != "" || st.creates[1].Secret != "" {
		t.Errorf("provider create leaked a secret: %q / %+v", created.Secret, st.creates[1])
	}
}

func TestCreateNeverEchoesACallerSuppliedSecret(t *testing.T) {
	st := newMemWebhooks()
	optIn := true
	w := Webhooks{Store: st, AllowSuppliedSecret: func() bool { return optIn }}
	ceiling := Ceiling{EndpointID: epCeiling, EndpointState: "active", WebhookMax: 2,
		SourceTypes: []string{"github"}, WebhookQueues: []string{"q"}}
	supplied := "whsec_the_operators_own_value"

	created, err := w.Create(context.Background(), HumanPrincipal(humanH), ceiling, store.SecretActor{},
		CreateIn{SourceType: "github", TargetQueue: "q", SigningSecret: &supplied})
	if err != nil {
		t.Fatalf("create with a supplied secret: %v", err)
	}
	if created.Secret != "" {
		t.Errorf("the response carries %q, want nothing echoed back", created.Secret)
	}
	if len(st.creates) != 1 || st.creates[0].Secret != supplied || !st.creates[0].SecretSupplied {
		t.Fatalf("stored create = %+v, want the supplied value marked as supplied", st.creates)
	}

	// With the opt-in off, the same create is refused and nothing is written.
	optIn = false
	before := len(st.creates)
	if _, err := w.Create(context.Background(), HumanPrincipal(humanH), ceiling, store.SecretActor{},
		CreateIn{SourceType: "github", TargetQueue: "q", SigningSecret: &supplied}); err == nil {
		t.Error("a supplied secret accepted with the opt-in off")
	}
	if len(st.creates) != before {
		t.Errorf("the refused supplied-secret create still wrote a row")
	}
}

func TestRotateRevealsOnlyWhatSwitchboardMinted(t *testing.T) {
	st := newMemWebhooks()
	w := Webhooks{Store: st, Humans: memWebhookHumans{rows: map[string]memHookOwner{
		hookW1: {endpointID: epE1, humanID: humanH}, hookStripe: {endpointID: epE1, humanID: humanH},
	}}}

	created, err := w.Rotate(context.Background(), HumanPrincipal(humanH), hookW1)
	if err != nil {
		t.Fatalf("rotate a minted-origin webhook: %v", err)
	}
	if !strings.HasPrefix(created.Secret, "whsec_") {
		t.Errorf("rotate reveal = %q, want the fresh minted secret", created.Secret)
	}

	// A provider's secret stays: minting a fresh one would swap a value the producer signs with for one
	// it has never heard of, which is a broken webhook that looks healthy.
	created, err = w.Rotate(context.Background(), HumanPrincipal(humanH), hookStripe)
	if err != nil {
		t.Fatalf("rotate a provider-origin webhook: %v", err)
	}
	if created.Secret != "" {
		t.Errorf("rotate of a provider webhook revealed %q, want only the new URL", created.Secret)
	}

	// A webhook of someone else's resolves to nothing, so the write never happens.
	if _, err := w.Rotate(context.Background(), HumanPrincipal("human-stranger"), hookW1); err == nil {
		t.Error("rotate of an unreachable webhook succeeded")
	}
	if !slices.Contains(st.rotations, hookW1) || !slices.Contains(st.rotations, hookStripe) || len(st.rotations) != 2 {
		t.Errorf("rotations = %v, want exactly the two reachable webhooks", st.rotations)
	}
}

func TestDeleteScopesTheWriteToTheResolvedEndpoint(t *testing.T) {
	st := newMemWebhooks()
	w := Webhooks{Store: st, Humans: memWebhookHumans{rows: map[string]memHookOwner{
		hookW1: {endpointID: epE1, humanID: humanH},
	}}}
	if err := w.Delete(context.Background(), HumanPrincipal(humanH), hookW1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !slices.Contains(st.deletions, hookW1) {
		t.Fatalf("deletions = %v, want %s", st.deletions, hookW1)
	}
	if err := w.Delete(context.Background(), HumanPrincipal(humanH), "webhook-gone"); err == nil {
		t.Error("delete of an unknown webhook succeeded")
	} else {
		var me *Error
		if !errors.As(err, &me) || me.Kind != ErrNotFound {
			t.Errorf("delete of an unknown webhook: %v, want not_found", err)
		}
	}
	if err := w.Delete(context.Background(), HumanPrincipal(humanH), ""); err == nil {
		t.Error("delete with no id succeeded")
	}
}

func strPtr(s string) *string { return &s }

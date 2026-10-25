package store

// DB-backed tests for the per-webhook provider connection config (#614): the credential is sealed at
// rest and never travels on a read that builds a response, the address is canonicalized and confined by
// the operator's allowlist where it is written, the derived state tells a human whether a reconcile read
// would work, and both authorization paths (endpoint credential, human reach) are enforced in SQL.
// Skip cleanly without SWITCHBOARD_TEST_DATABASE_URL like every other store test.
//
// Governing: stump.wtf/switchboard#614, #616; ADR-0002, ADR-0037; SPEC-0035 REQ "Reach on Every Route".

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/cred"
)

func strPtr(s string) *string { return &s }

func rawProviderToken(t *testing.T, s *Store, ctx context.Context, webhookID string) string {
	t.Helper()
	var raw *string
	if err := s.pool.QueryRow(ctx,
		`SELECT provider_read_token FROM endpoint_webhooks WHERE id = $1`, webhookID,
	).Scan(&raw); err != nil {
		t.Fatalf("raw select provider_read_token: %v", err)
	}
	if raw == nil {
		return ""
	}
	return *raw
}

func providerFixture(t *testing.T, s *Store, ctx context.Context, slug string) (humanID, endpointID, webhookID string) {
	t.Helper()
	h := mustHuman(t, s, ctx, "pocket|"+slug, slug)
	ag := mustAgent(t, s, ctx, h.ID, slug+"-bot")
	minted, _ := MintSlug(ag.Name)
	ep, err := s.CreateEndpoint(ctx, ag.ID, "credhash-"+slug, "sbk_"+slug, minted, []string{"reviews"}, []string{"create_webhook", "list_webhooks"})
	if err != nil {
		t.Fatalf("vend endpoint: %v", err)
	}
	wh, err := s.CreateWebhook(ctx, ep.ID, "github", "reviews", "signed", "conn-"+slug, "whsec_conn", 3)
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	return h.ID, ep.ID, wh.ID
}

// TestProviderConnectionSealsTokenAndNeverLeaksIt is the acceptance test: the read credential is stored
// as enc:v1: ciphertext, opens only through ProviderReadConnection, and appears in no listing — while the
// derived state says the webhook is ready.
func TestProviderConnectionSealsTokenAndNeverLeaksIt(t *testing.T) {
	s, ctx, _ := testStoreWithCipher(t)
	_, endpointID, webhookID := providerFixture(t, s, ctx, "connseal")

	token := "ghp_readonly_abcdefghij"
	set, err := s.SetWebhookProviderConnection(ctx, webhookID, endpointID, ProviderConnectionEdit{
		BaseURL: "https://api.github.com/",
		Repo:    "stump-wtf/switchboard",
		Token:   &token,
	})
	if err != nil {
		t.Fatalf("set connection: %v", err)
	}
	if set.ProviderBaseURL != "https://api.github.com" {
		t.Fatalf("stored base url %q, want the canonical origin with the trailing slash gone", set.ProviderBaseURL)
	}
	if set.ProviderRepo != "stump-wtf/switchboard" || !set.ProviderReadTokenSet {
		t.Fatalf("stored connection = %+v", set)
	}
	if set.ProviderConnectionSetAt == nil {
		t.Fatal("provider_connection_set_at did not move")
	}
	if got := set.ProviderConnectionState(); got != ProviderConnectionReady {
		t.Fatalf("state = %q, want %q", got, ProviderConnectionReady)
	}

	raw := rawProviderToken(t, s, ctx, webhookID)
	if !cred.IsEncrypted(raw) || strings.Contains(raw, token) {
		t.Fatalf("stored provider_read_token %q is not sealed ciphertext", raw)
	}

	rows, err := s.ListWebhooks(ctx, endpointID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("list returned %d rows", len(rows))
	}
	if rows[0].ProviderBaseURL == "" || !rows[0].ProviderReadTokenSet {
		t.Fatalf("list row lost the connection view: %+v", rows[0])
	}
	// The listing view has no field the credential could land in: Webhook reports only that one is set.

	conn, err := s.ProviderReadConnection(ctx, webhookID)
	if err != nil {
		t.Fatalf("read connection: %v", err)
	}
	if conn.Token != token || conn.Repo != "stump-wtf/switchboard" || conn.State != ProviderConnectionReady {
		t.Fatalf("read connection = %+v, want the opened token and ready", conn)
	}
}

// TestProviderConnectionAwaitingCredential is the #615 state: an address with no credential. Nothing
// dials a provider in that state, and the state is derived, not stored.
func TestProviderConnectionAwaitingCredential(t *testing.T) {
	s, ctx, _ := testStoreWithCipher(t)
	_, endpointID, webhookID := providerFixture(t, s, ctx, "conawait")

	set, err := s.SetWebhookProviderConnection(ctx, webhookID, endpointID, ProviderConnectionEdit{
		BaseURL: "https://gitea.stump.rocks/api/v1",
		Repo:    "stump.wtf/switchboard",
	})
	if err != nil {
		t.Fatalf("set connection: %v", err)
	}
	if got := set.ProviderConnectionState(); got != ProviderConnectionAwaitingCredential {
		t.Fatalf("state = %q, want %q", got, ProviderConnectionAwaitingCredential)
	}
	if raw := rawProviderToken(t, s, ctx, webhookID); raw != "" {
		t.Fatalf("a webhook awaiting its credential holds %q", raw)
	}
	conn, err := s.ProviderReadConnection(ctx, webhookID)
	if err != nil {
		t.Fatalf("read connection: %v", err)
	}
	if conn.Token != "" || conn.State != ProviderConnectionAwaitingCredential {
		t.Fatalf("read connection = %+v, want no token and awaiting_credential", conn)
	}

	// An unconfigured webhook answers the same question with "unset" rather than an error.
	_, endpoint2, webhook2 := providerFixture(t, s, ctx, "connone")
	if _, err := s.SetWebhookProviderConnection(ctx, webhook2, endpoint2, ProviderConnectionEdit{}); err != nil {
		t.Fatalf("clear connection: %v", err)
	}
	conn2, err := s.ProviderReadConnection(ctx, webhook2)
	if err != nil {
		t.Fatalf("read unset connection: %v", err)
	}
	if conn2.State != ProviderConnectionUnset || conn2.BaseURL != "" {
		t.Fatalf("unset connection = %+v", conn2)
	}
}

// TestSetWebhookProviderConnectionRefusals covers the write-path rules: half an address, a malformed or
// disallowed address, and a credential with no at-rest key are all refused before anything is stored.
func TestSetWebhookProviderConnectionRefusals(t *testing.T) {
	s, ctx, _ := testStoreWithCipher(t)
	_, endpointID, webhookID := providerFixture(t, s, ctx, "connrefuse")
	token := "ghp_somethingsecret"

	cases := []struct {
		name string
		edit ProviderConnectionEdit
		want error
	}{
		{"repo without an address", ProviderConnectionEdit{Repo: "a/b"}, ErrInvalidArgument},
		{"address without a repo", ProviderConnectionEdit{BaseURL: "https://api.github.com"}, ErrInvalidArgument},
		{"malformed address", ProviderConnectionEdit{BaseURL: "not a url", Repo: "a/b"}, ErrInvalidArgument},
		{"credentials in the address", ProviderConnectionEdit{BaseURL: "https://token:x@api.github.com", Repo: "a/b"}, ErrInvalidArgument},
		{"plain http address", ProviderConnectionEdit{BaseURL: "http://api.github.com", Repo: "a/b"}, ErrInvalidArgument},
		{"address off the allowlist", ProviderConnectionEdit{
			BaseURL: "https://evil.example", Repo: "a/b",
			AllowedOrigins: []string{"https://api.github.com"},
		}, ErrProviderOriginRefused},
		{"repo with a path in it", ProviderConnectionEdit{BaseURL: "https://api.github.com", Repo: "a/b/c"}, ErrInvalidArgument},
	}
	for _, tc := range cases {
		tc.edit.Token = &token
		if _, err := s.SetWebhookProviderConnection(ctx, webhookID, endpointID, tc.edit); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if raw := rawProviderToken(t, s, ctx, webhookID); raw != "" {
		t.Fatalf("a refused write stored a credential: %q", raw)
	}

	// No at-rest key: the credential is refused, and the address is not written either.
	bare := New(s.pool)
	if _, err := bare.SetWebhookProviderConnection(ctx, webhookID, endpointID, ProviderConnectionEdit{
		BaseURL: "https://api.github.com", Repo: "a/b", Token: &token,
	}); !errors.Is(err, ErrProviderTokenEncryptionRequired) {
		t.Fatalf("no-cipher write err = %v, want %v", err, ErrProviderTokenEncryptionRequired)
	}
}

// TestSetWebhookProviderConnectionTokenStates is the three-way credential rule a human depends on: an
// omitted token leaves the stored one, "" clears it, and a value replaces it — while the address pair is
// always replaced whole.
func TestSetWebhookProviderConnectionTokenStates(t *testing.T) {
	s, ctx, _ := testStoreWithCipher(t)
	_, endpointID, webhookID := providerFixture(t, s, ctx, "conntoken")
	token := "ghp_first"

	if _, err := s.SetWebhookProviderConnection(ctx, webhookID, endpointID, ProviderConnectionEdit{
		BaseURL: "https://api.github.com", Repo: "a/b", Token: &token,
	}); err != nil {
		t.Fatalf("first set: %v", err)
	}
	first := rawProviderToken(t, s, ctx, webhookID)

	moved, err := s.SetWebhookProviderConnection(ctx, webhookID, endpointID, ProviderConnectionEdit{
		BaseURL: "https://api.github.com", Repo: "a/c",
	})
	if err != nil {
		t.Fatalf("address-only edit: %v", err)
	}
	if moved.ProviderRepo != "a/c" || !moved.ProviderReadTokenSet {
		t.Fatalf("address-only edit lost the credential or the new repo: %+v", moved)
	}
	if rawProviderToken(t, s, ctx, webhookID) != first {
		t.Fatal("omitting the token rewrote it; a human fixing a typo must not have to re-paste a credential")
	}

	cleared, err := s.SetWebhookProviderConnection(ctx, webhookID, endpointID, ProviderConnectionEdit{
		BaseURL: "https://api.github.com", Repo: "a/c", Token: strPtr(""),
	})
	if err != nil {
		t.Fatalf("clear token: %v", err)
	}
	if cleared.ProviderReadTokenSet {
		t.Fatal("provider_read_token survived the clear")
	}
	if got := cleared.ProviderConnectionState(); got != ProviderConnectionAwaitingCredential {
		t.Fatalf("state after clearing the token = %q", got)
	}

	clearedAll, err := s.SetWebhookProviderConnection(ctx, webhookID, endpointID, ProviderConnectionEdit{})
	if err != nil {
		t.Fatalf("clear address: %v", err)
	}
	if clearedAll.ProviderBaseURL != "" || clearedAll.ProviderRepo != "" {
		t.Fatalf("clearing the address left %+v", clearedAll)
	}
}

// TestSetWebhookProviderConnectionAuthorization covers both guards: another endpoint's webhook is not
// writable by this endpoint, and neither is another human's webhook by the human path.
func TestSetWebhookProviderConnectionAuthorization(t *testing.T) {
	s, ctx, _ := testStoreWithCipher(t)
	humanA, _, webhookA := providerFixture(t, s, ctx, "connautha")
	humanB, _, webhookB := providerFixture(t, s, ctx, "connauthb")
	token := "ghp_token"
	edit := ProviderConnectionEdit{BaseURL: "https://api.github.com", Repo: "a/b", Token: &token}

	if _, err := s.SetWebhookProviderConnection(ctx, webhookA, "00000000-0000-0000-0000-000000000000", edit); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong endpoint err = %v, want ErrNotFound", err)
	}
	if _, err := s.SetWebhookProviderConnectionForHuman(ctx, webhookB, humanA, edit); !errors.Is(err, ErrNotFound) {
		t.Fatalf("out-of-reach human err = %v, want ErrNotFound", err)
	}
	if _, err := s.SetWebhookProviderConnectionForHuman(ctx, webhookA, humanA, edit); err != nil {
		t.Fatalf("in-reach human: %v", err)
	}
	if raw := rawProviderToken(t, s, ctx, webhookA); !cred.IsEncrypted(raw) {
		t.Fatalf("human path stored %q, want sealed ciphertext", raw)
	}
	if raw := rawProviderToken(t, s, ctx, webhookB); raw != "" {
		t.Fatalf("out-of-reach write touched another human's webhook: %q", raw)
	}
	if _, err := s.SetWebhookProviderConnectionForHuman(ctx, webhookB, humanB, edit); err != nil {
		t.Fatalf("webhookB's own human: %v", err)
	}
	if raw := rawProviderToken(t, s, ctx, webhookB); !cred.IsEncrypted(raw) {
		t.Fatalf("webhookB's own human could not write it: %q", raw)
	}
}

// TestProviderConnectionRowConstraint proves migration 0035's CHECK holds at the row level, so a future
// writer that skips the store's checks still cannot leave half an address behind.
func TestProviderConnectionRowConstraint(t *testing.T) {
	s, ctx, _ := testStoreWithCipher(t)
	_, _, webhookID := providerFixture(t, s, ctx, "conncheck")
	if _, err := s.pool.Exec(ctx, `UPDATE endpoint_webhooks SET provider_base_url = 'https://api.github.com' WHERE id = $1`, webhookID); err == nil {
		t.Fatal("an address with no repository passed the row CHECK")
	} else if !strings.Contains(err.Error(), "provider_connection_address") {
		t.Fatalf("expected the provider_connection_address violation, got %v", err)
	}
}

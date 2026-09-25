package store

// DB-backed tests for the trusted_actors column (ADR-0031, SPEC-0026 REQ-5): a new github, gitea or
// cairn webhook stores its source's empty list (trusts no one), other sources store none, and the
// list is replaced only by the owning endpoint. Another endpoint's, an unknown, or a malformed id is
// not found.

import (
	"errors"
	"testing"
)

func TestWebhookTrustedActorsDefaultAndOwnership(t *testing.T) {
	s, ctx := testStore(t)
	ep := seedEndpoint(t, s, ctx, "trust-owner", "q")
	other := seedEndpoint(t, s, ctx, "trust-other", "q")

	want := map[string]string{
		"github":  `{"logins": [], "match": "sender"}`,
		"gitea":   `{"logins": [], "match": "sender"}`,
		"cairn":   `{"actor_ids": []}`,
		"generic": "",
	}
	ids := map[string]string{}
	for src, stored := range want {
		trust := "signed"
		if src == "generic" {
			trust = "token"
		}
		wh, err := s.CreateWebhook(ctx, ep, src, "q", trust, "tok-trust-"+src, "whsec_x", 10)
		if err != nil {
			t.Fatalf("create %s: %v", src, err)
		}
		ids[src] = wh.ID
		var got *string
		if err := s.pool.QueryRow(ctx, `SELECT trusted_actors::text FROM endpoint_webhooks WHERE id = $1`, wh.ID).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		if (stored == "") != (got == nil) || (got != nil && !sameJSON(t, []byte(*got), []byte(stored))) {
			t.Fatalf("%s trusted_actors = %v, want %q", src, got, stored)
		}
		if (stored == "") != (wh.TrustedActors == nil) {
			t.Fatalf("%s returned TrustedActors = %s, want present iff stored", src, wh.TrustedActors)
		}
	}

	explicit, err := s.CreateWebhookWithTrust(ctx, ep, "github", "q", "signed", "tok-trust-explicit", "whsec_x", 10,
		[]byte(`{"logins":["joestump"],"match":"sender"}`))
	if err != nil || !sameJSON(t, explicit.TrustedActors, []byte(`{"logins":["joestump"],"match":"sender"}`)) {
		t.Fatalf("explicit create = %s (%v)", explicit.TrustedActors, err)
	}

	set, err := s.SetWebhookTrustedActors(ctx, ids["github"], ep, []byte(`{"allow_all":true}`))
	if err != nil || string(set.TrustedActors) != `{"allow_all": true}` {
		t.Fatalf("set = %s (%v), want allow_all", set.TrustedActors, err)
	}
	listed, err := s.ListWebhooks(ctx, ep)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, w := range listed {
		if w.ID == ids["github"] && string(w.TrustedActors) != `{"allow_all": true}` {
			t.Fatalf("listed trusted_actors = %s, want allow_all", w.TrustedActors)
		}
	}
	for _, c := range []struct{ id, ep string }{
		{ids["github"], other}, {"00000000-0000-0000-0000-000000000000", ep}, {"not-a-uuid", ep},
	} {
		if _, err := s.SetWebhookTrustedActors(ctx, c.id, c.ep, []byte(`{"logins":[]}`)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("set(%s, %s) = %v, want ErrNotFound", c.id, c.ep, err)
		}
		if _, err := s.WebhookForEndpoint(ctx, c.id, c.ep); !errors.Is(err, ErrNotFound) {
			t.Fatalf("get(%s, %s) = %v, want ErrNotFound", c.id, c.ep, err)
		}
	}
	if w, err := s.WebhookForEndpoint(ctx, ids["cairn"], ep); err != nil || w.SourceType != "cairn" {
		t.Fatalf("get own = %+v (%v)", w, err)
	}
	// The receiver's read carries the list too.
	byToken, _, err := s.GetWebhookSecretByToken(ctx, "tok-trust-github")
	if err != nil || string(byToken.TrustedActors) != `{"allow_all": true}` {
		t.Fatalf("by token = %s (%v)", byToken.TrustedActors, err)
	}
}

package store

// Device-authorization storage tests (issue #308, ADR-0057): the two codes are stored hashed and
// each request transitions exactly once (pending → approved/denied → spent), a spend is a CAS so
// only one poll can redeem it, expiry makes a request unspendable, the polling lookup hands back
// the PREVIOUS poll stamp (that is what slow_down compares against), and user-code entry is
// rate-limited per actor so grinding the short code is refused. Skipped without
// SWITCHBOARD_TEST_DATABASE_URL. Governing: ADR-0019, ADR-0023, ADR-0057 (proposed).

import (
	"context"
	"errors"
	"testing"
	"time"
)

// newDeviceRow creates the client a device request names and persists one pending request,
// returning its row id. It reads the id straight from the table: going through the polling lookup
// would stamp an arrival and cost the caller its first poll.
func newDeviceRow(t *testing.T, s *Store, ctx context.Context, clientID, deviceHash, userHash string, ttl time.Duration) string {
	t.Helper()
	if _, err := s.CreateOAuthClient(ctx, clientID, "switchboard CLI", []string{"http://127.0.0.1:41234/callback"}); err != nil {
		t.Fatalf("create oauth client: %v", err)
	}
	if err := s.CreateOAuthDeviceCode(ctx, deviceHash, userHash, clientID, time.Now().Add(ttl), 5); err != nil {
		t.Fatalf("create device code: %v", err)
	}
	var id string
	if err := s.pool.QueryRow(ctx, `SELECT id::text FROM oauth_device_codes WHERE device_code_hash = $1`, deviceHash).Scan(&id); err != nil {
		t.Fatalf("load device row id: %v", err)
	}
	return id
}

// TestOAuthDevicePollStampIsPrevious: the polling lookup stamps the arrival it serves, so the
// value it returns must be the PREVIOUS poll — a handler that read the fresh stamp would compare
// now() against now()+interval and answer slow_down to every honest poller after the first.
func TestOAuthDevicePollStampIsPrevious(t *testing.T) {
	s, ctx := testStore(t)
	id := newDeviceRow(t, s, ctx, "cid-dev-stamp", "devhash-stamp-1", "userhash-stamp-1", 10*time.Minute)
	if id == "" {
		t.Fatal("device row has no id")
	}

	first, err := s.OAuthDeviceCodeByDeviceHash(ctx, "devhash-stamp-1")
	if err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if first.LastPolledAt != nil {
		t.Fatalf("first poll reports LastPolledAt=%v, want nil so the very first poll cannot be slow_down", *first.LastPolledAt)
	}
	time.Sleep(20 * time.Millisecond)
	between := time.Now()
	time.Sleep(20 * time.Millisecond)

	second, err := s.OAuthDeviceCodeByDeviceHash(ctx, "devhash-stamp-1")
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if second.LastPolledAt == nil {
		t.Fatal("second poll reports no previous poll, so slow_down could never fire")
	}
	if !second.LastPolledAt.Before(between) {
		t.Fatalf("second poll returned LastPolledAt=%v, want the PREVIOUS stamp (before %v): the lookup must not hand back the stamp it just wrote", *second.LastPolledAt, between)
	}
}

// TestOAuthDeviceSpendIsSingleUseCAS: approval binds the approving human and leaves the request
// unspent; exactly one poll redeems it; a denial or an expiry ends the request instead.
func TestOAuthDeviceSpendIsSingleUseCAS(t *testing.T) {
	s, ctx := testStore(t)
	h, err := s.UpsertHuman(ctx, "pocket|dev1", "Ada", "ada@example.com")
	if err != nil {
		t.Fatalf("upsert human: %v", err)
	}
	id := newDeviceRow(t, s, ctx, "cid-dev-spend", "devhash-spend-1", "userhash-spend-1", 10*time.Minute)

	if err := s.SpendOAuthDeviceCode(ctx, id); !errors.Is(err, ErrConflict) {
		t.Fatalf("spend before approval: %v, want ErrConflict", err)
	}
	if err := s.ApproveOAuthDeviceCode(ctx, id, h.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	d, err := s.OAuthDeviceCodeByDeviceHash(ctx, "devhash-spend-1")
	if err != nil {
		t.Fatalf("poll after approve: %v", err)
	}
	if d.AuthorizedAt == nil || d.UsedAt != nil || d.HumanID != h.ID {
		t.Fatalf("approved row: %+v, want authorized+human and not yet spent", d)
	}
	if err := s.ApproveOAuthDeviceCode(ctx, id, h.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second approve: %v, want ErrNotFound (a decision is final)", err)
	}
	if err := s.DenyOAuthDeviceCode(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deny after approve: %v, want ErrNotFound", err)
	}
	if err := s.SpendOAuthDeviceCode(ctx, id); err != nil {
		t.Fatalf("first spend: %v", err)
	}
	if err := s.SpendOAuthDeviceCode(ctx, id); !errors.Is(err, ErrConflict) {
		t.Fatalf("second spend: %v, want ErrConflict (one grant per request)", err)
	}
	if err := s.SpendOAuthDeviceCode(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrConflict) {
		t.Fatalf("spend of an unknown id: %v, want ErrConflict", err)
	}
}

// TestOAuthDeviceDenyIsFinal: a refusal leaves the consent record but kills the grant, and an
// expired request accepts no decision at all.
func TestOAuthDeviceDenyIsFinal(t *testing.T) {
	s, ctx := testStore(t)
	h, err := s.UpsertHuman(ctx, "pocket|dev2", "Ada", "ada@example.com")
	if err != nil {
		t.Fatalf("upsert human: %v", err)
	}
	id := newDeviceRow(t, s, ctx, "cid-dev-deny", "devhash-deny-1", "userhash-deny-1", 10*time.Minute)
	if err := s.DenyOAuthDeviceCode(ctx, id); err != nil {
		t.Fatalf("deny: %v", err)
	}
	d, err := s.OAuthDeviceCodeByDeviceHash(ctx, "devhash-deny-1")
	if err != nil {
		t.Fatalf("poll after deny: %v", err)
	}
	if d.DeniedAt == nil || d.AuthorizedAt != nil {
		t.Fatalf("denied row: %+v", d)
	}
	if err := s.SpendOAuthDeviceCode(ctx, id); !errors.Is(err, ErrConflict) {
		t.Fatalf("spend of a denied request: %v, want ErrConflict", err)
	}
	// The entry screen can no longer resolve a denied request either.
	if _, err := s.OAuthDeviceCodeByUserCode(ctx, "userhash-deny-1", h.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("entry lookup of a denied request: %v, want ErrNotFound", err)
	}

	expiredID := newDeviceRow(t, s, ctx, "cid-dev-exp", "devhash-exp-1", "userhash-exp-1", -time.Minute)
	if _, err := s.OAuthDeviceCodeByUserCode(ctx, "userhash-exp-1", h.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("entry lookup of an expired request: %v, want ErrNotFound", err)
	}
	if err := s.ApproveOAuthDeviceCode(ctx, expiredID, h.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("approve of an expired request: %v, want ErrNotFound", err)
	}
	if row, err := s.OAuthDeviceCodeByDeviceHash(ctx, "devhash-exp-1"); err != nil || !row.ExpiresAt.Before(time.Now()) {
		t.Fatalf("expired row still answers the poller as live: %+v err=%v", row, err)
	}
}

// TestOAuthDeviceEntryRateLimit: guesses are charged to the ACTOR, so the misses a per-request
// counter could never observe still close the window, another actor is untouched by it, and the
// window reopens on its own.
func TestOAuthDeviceEntryRateLimit(t *testing.T) {
	s, ctx := testStore(t)
	h, err := s.UpsertHuman(ctx, "pocket|dev3", "Ada", "ada@example.com")
	if err != nil {
		t.Fatalf("upsert human: %v", err)
	}
	other, err := s.UpsertHuman(ctx, "pocket|dev4", "Joe", "joe@example.com")
	if err != nil {
		t.Fatalf("upsert human: %v", err)
	}
	newDeviceRow(t, s, ctx, "cid-dev-rl", "devhash-rl-1", "userhash-rl-1", 10*time.Minute)

	if _, err := s.OAuthDeviceCodeByUserCode(ctx, "userhash-rl-1", h.ID); err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	// Wrong codes hash to no row at all: uniform ErrNotFound, and each one is counted.
	for i := 1; i < DeviceEntryMaxGuesses; i++ {
		if _, err := s.OAuthDeviceCodeByUserCode(ctx, "userhash-wrong", h.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("wrong guess %d: %v, want ErrNotFound", i+1, err)
		}
	}
	// The cap is now spent, and what follows is a refusal even for the RIGHT code.
	if _, err := s.OAuthDeviceCodeByUserCode(ctx, "userhash-rl-1", h.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("lookup past the window: %v, want ErrConflict", err)
	}
	if _, err := s.OAuthDeviceCodeByUserCode(ctx, "userhash-rl-1", other.ID); err != nil {
		t.Fatalf("another actor's lookup while one is rate-limited: %v", err)
	}
	// The window is a window, not a lockout: backdating the counter reopens it.
	if _, err := s.pool.Exec(ctx,
		`UPDATE oauth_device_entry_attempts SET guessed_at = now() - interval '2 minutes' WHERE gate_key = $1`, h.ID); err != nil {
		t.Fatalf("backdate counter: %v", err)
	}
	if _, err := s.OAuthDeviceCodeByUserCode(ctx, "userhash-rl-1", h.ID); err != nil {
		t.Fatalf("lookup after the window passed: %v", err)
	}
}

// TestOAuthDeviceCodeHashesAreUnique: neither code may be adopted from another request — a
// collision is ErrConflict, which the caller answers by re-minting.
func TestOAuthDeviceCodeHashesAreUnique(t *testing.T) {
	s, ctx := testStore(t)
	if _, err := s.CreateOAuthClient(ctx, "cid-dev-uniq", "switchboard CLI", []string{"http://127.0.0.1:41234/callback"}); err != nil {
		t.Fatalf("create oauth client: %v", err)
	}
	if err := s.CreateOAuthDeviceCode(ctx, "devhash-uniq", "userhash-uniq", "cid-dev-uniq", time.Now().Add(10*time.Minute), 5); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.CreateOAuthDeviceCode(ctx, "devhash-uniq", "userhash-other", "cid-dev-uniq", time.Now().Add(10*time.Minute), 5); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate device hash: %v, want ErrConflict", err)
	}
	if err := s.CreateOAuthDeviceCode(ctx, "devhash-other", "userhash-uniq", "cid-dev-uniq", time.Now().Add(10*time.Minute), 5); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate user hash: %v, want ErrConflict", err)
	}
	if _, err := s.OAuthDeviceCodeByDeviceHash(ctx, "devhash-none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown device hash: %v, want ErrNotFound", err)
	}
}

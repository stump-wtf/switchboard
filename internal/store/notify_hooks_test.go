package store

// DB-backed tests for the SPEC-0024 notify-hook store. They skip without
// SWITCHBOARD_TEST_DATABASE_URL like every other store test; CI runs them against Postgres.
//
// Governing: SPEC-0024 REQ-1 "Hook Ownership and Scope", REQ-2 "Management Verbs", REQ-4 "Signing",
// REQ-13 "Concurrency and Database Standards".

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/cred"
)

const testHookURL = "https://dispatch.example.com/sb"

// hookStore is testStore with the at-rest cipher on, which notify hooks require.
func hookStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	s, ctx := testStore(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(0x40 + i)
	}
	box, err := cred.NewSecretBox(key)
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	return New(s.pool, WithSecretCipher(box)), ctx
}

func countHooks(t *testing.T, s *Store, ctx context.Context, endpointID string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notify_hooks WHERE endpoint_id = $1`, endpointID).Scan(&n); err != nil {
		t.Fatalf("count hooks: %v", err)
	}
	return n
}

func rawHookSecrets(t *testing.T, s *Store, ctx context.Context, id string) (string, *string) {
	t.Helper()
	var cur string
	var prev *string
	if err := s.pool.QueryRow(ctx, `SELECT secret, prev_secret FROM notify_hooks WHERE id = $1`, id).Scan(&cur, &prev); err != nil {
		t.Fatalf("raw hook secrets: %v", err)
	}
	return cur, prev
}

func TestNotifyHookCreateListGetDelete(t *testing.T) {
	s, ctx := hookStore(t)
	ep := seedEndpoint(t, s, ctx, "nh-crud", "inbox", "reviews")

	h, err := s.CreateNotifyHook(ctx, ep, testHookURL, []string{"reviews"}, true, "whsec_plain-one", 5)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if h.ID == "" || h.EndpointID != ep || h.URL != testHookURL || !h.Enabled || !h.IgnorePresence ||
		!reflect.DeepEqual(h.Queues, []string{"reviews"}) || h.ConsecutiveFailures != 0 || h.DisabledReason != nil {
		t.Fatalf("created hook = %+v", h)
	}

	// At rest the secret is envelope ciphertext, never the plaintext.
	cur, prev := rawHookSecrets(t, s, ctx, h.ID)
	if !strings.HasPrefix(cur, "enc:v1:") || strings.Contains(cur, "plain-one") || prev != nil {
		t.Fatalf("stored secret is not sealed (prefix %q), prev=%v", cur[:min(len(cur), 7)], prev)
	}
	sec, err := s.NotifyHookSigningSecrets(ctx, h.ID, ep)
	if err != nil || sec.Current != "whsec_plain-one" || sec.Previous != "" {
		t.Fatalf("signing secrets = %+v, %v", sec, err)
	}

	// An empty queue list persists as empty, not NULL.
	h2, err := s.CreateNotifyHook(ctx, ep, testHookURL+"/2", nil, false, "whsec_plain-two", 5)
	if err != nil || h2.Queues == nil || len(h2.Queues) != 0 {
		t.Fatalf("create with nil queues = %+v, %v", h2, err)
	}

	list, err := s.ListNotifyHooks(ctx, ep)
	if err != nil || len(list) != 2 || list[0].ID != h.ID || list[1].ID != h2.ID {
		t.Fatalf("list = %+v, %v", list, err)
	}
	got, err := s.GetNotifyHook(ctx, h.ID, ep)
	if err != nil || got.ID != h.ID {
		t.Fatalf("get = %+v, %v", got, err)
	}

	if err := s.DeleteNotifyHook(ctx, h.ID, ep); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteNotifyHook(ctx, h.ID, ep); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
	if _, err := s.NotifyHookSigningSecrets(ctx, h.ID, ep); !errors.Is(err, ErrNotFound) {
		t.Fatalf("secrets after delete = %v, want ErrNotFound", err)
	}
	if n := countHooks(t, s, ctx, ep); n != 1 {
		t.Fatalf("hooks after delete = %d, want 1", n)
	}
}

// TestNotifyHookRequiresCipher: with no encryption key, a hook is refused outright and nothing is
// stored; notify hooks never fall back to plaintext.
func TestNotifyHookRequiresCipher(t *testing.T) {
	s, ctx := testStore(t) // no cipher
	ep := seedEndpoint(t, s, ctx, "nh-nocipher")
	if _, err := s.CreateNotifyHook(ctx, ep, testHookURL, nil, false, "whsec_x", 5); !errors.Is(err, ErrSecretCipherRequired) {
		t.Fatalf("create without cipher = %v, want ErrSecretCipherRequired", err)
	}
	if n := countHooks(t, s, ctx, ep); n != 0 {
		t.Fatalf("hooks stored without a cipher: %d", n)
	}
}

// TestNotifyHookRefusesUnsealedSecret: a row whose secret is not envelope ciphertext is refused on
// read, never passed through as legacy plaintext.
func TestNotifyHookRefusesUnsealedSecret(t *testing.T) {
	s, ctx := hookStore(t)
	ep := seedEndpoint(t, s, ctx, "nh-unsealed")
	h, err := s.CreateNotifyHook(ctx, ep, testHookURL, nil, false, "whsec_x", 5)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE notify_hooks SET secret = 'whsec_planted' WHERE id = $1`, h.ID); err != nil {
		t.Fatalf("plant plaintext: %v", err)
	}
	if _, err := s.NotifyHookSigningSecrets(ctx, h.ID, ep); !errors.Is(err, ErrSecretNotSealed) {
		t.Fatalf("read of an unsealed secret = %v, want ErrSecretNotSealed", err)
	}
}

func TestNotifyHookURLLength(t *testing.T) {
	s, ctx := hookStore(t)
	ep := seedEndpoint(t, s, ctx, "nh-url")
	long := "https://x.example/" + strings.Repeat("a", MaxNotifyHookURLLen)
	for _, u := range []string{"", long} {
		if _, err := s.CreateNotifyHook(ctx, ep, u, nil, false, "whsec_x", 5); err == nil {
			t.Fatalf("create with a %d-byte url succeeded", len(u))
		}
	}
	if n := countHooks(t, s, ctx, ep); n != 0 {
		t.Fatalf("hooks stored: %d", n)
	}
}

// TestNotifyHookCeiling: REQ-1 "Ceiling reached", and a ceiling of 0 refusing every create.
func TestNotifyHookCeiling(t *testing.T) {
	s, ctx := hookStore(t)
	ep := seedEndpoint(t, s, ctx, "nh-ceiling")
	for i := 0; i < 5; i++ {
		if _, err := s.CreateNotifyHook(ctx, ep, testHookURL, nil, false, "whsec_x", 5); err != nil {
			t.Fatalf("create %d: %v", i+1, err)
		}
	}
	if _, err := s.CreateNotifyHook(ctx, ep, testHookURL+"/6", nil, false, "whsec_sixth", 5); !errors.Is(err, ErrCeilingExceeded) {
		t.Fatalf("6th create = %v, want ErrCeilingExceeded", err)
	}
	if n := countHooks(t, s, ctx, ep); n != 5 {
		t.Fatalf("hooks after refused create = %d, want 5", n)
	}
	var leaked int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notify_hooks WHERE url = $1`, testHookURL+"/6").Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("refused hook persisted: %d, %v", leaked, err)
	}

	// SWITCHBOARD_NOTIFY_HOOK_MAX=0: an endpoint with no hooks still cannot create one.
	empty := seedEndpoint(t, s, ctx, "nh-ceiling-zero")
	if _, err := s.CreateNotifyHook(ctx, empty, testHookURL, nil, false, "whsec_x", 0); !errors.Is(err, ErrCeilingExceeded) {
		t.Fatalf("create at ceiling 0 = %v, want ErrCeilingExceeded", err)
	}
	if n := countHooks(t, s, ctx, empty); n != 0 {
		t.Fatalf("hooks stored at ceiling 0: %d", n)
	}
}

// TestNotifyHookConcurrentCreatesAtCeiling: REQ-13 — with 4 hooks and a ceiling of 5, two racing
// creates produce exactly one success.
func TestNotifyHookConcurrentCreatesAtCeiling(t *testing.T) {
	s, ctx := hookStore(t)
	for round := 0; round < 5; round++ {
		ep := seedEndpoint(t, s, ctx, "nh-race")
		for i := 0; i < 4; i++ {
			if _, err := s.CreateNotifyHook(ctx, ep, testHookURL, nil, false, "whsec_x", 5); err != nil {
				t.Fatalf("seed create: %v", err)
			}
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = s.CreateNotifyHook(ctx, ep, testHookURL, nil, false, "whsec_x", 5)
			}(i)
		}
		close(start)
		wg.Wait()
		ok, exceeded := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrCeilingExceeded):
				exceeded++
			default:
				t.Fatalf("racing create: %v", err)
			}
		}
		if ok != 1 || exceeded != 1 {
			t.Fatalf("round %d: %d succeeded and %d exceeded, want 1 and 1", round, ok, exceeded)
		}
		if n := countHooks(t, s, ctx, ep); n != 5 {
			t.Fatalf("round %d: %d hooks, want 5", round, n)
		}
	}
}

// TestNotifyHookTenancy: another endpoint's hook id is not_found on every method, for a second human
// and for a second endpoint of the SAME human, and the target hook is unchanged.
func TestNotifyHookTenancy(t *testing.T) {
	s, ctx := hookStore(t)
	owner := seedEndpoint(t, s, ctx, "nh-owner", "inbox")
	otherHuman := seedEndpoint(t, s, ctx, "nh-other-human", "inbox")

	// A second endpoint of the owner's own human: vend it on the owner's agent.
	var agentID string
	if err := s.pool.QueryRow(ctx, `SELECT agent_id::text FROM endpoints WHERE id = $1`, owner).Scan(&agentID); err != nil {
		t.Fatalf("owner agent: %v", err)
	}
	slug, err := MintSlug("nh-sibling")
	if err != nil {
		t.Fatalf("slug: %v", err)
	}
	sib, err := s.CreateEndpoint(ctx, agentID, "credhash-nh-sibling", "sbk_nhsib", slug, []string{"inbox"}, []string{"list_todos"})
	if err != nil {
		t.Fatalf("sibling endpoint: %v", err)
	}
	if ownerOf(t, s, ctx, sib.ID) != ownerOf(t, s, ctx, owner) || ownerOf(t, s, ctx, otherHuman) == ownerOf(t, s, ctx, owner) {
		t.Fatal("fixture: sibling must share the owner's human and otherHuman must not")
	}

	h, err := s.CreateNotifyHook(ctx, owner, testHookURL, []string{"inbox"}, false, "whsec_owner", 5)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for name, intruder := range map[string]string{"second human": otherHuman, "same human, other endpoint": sib.ID} {
		if _, err := s.GetNotifyHook(ctx, h.ID, intruder); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: get = %v, want ErrNotFound", name, err)
		}
		if _, err := s.RotateNotifyHookSecret(ctx, h.ID, intruder, "whsec_stolen", time.Hour); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: rotate = %v, want ErrNotFound", name, err)
		}
		if err := s.DeleteNotifyHook(ctx, h.ID, intruder); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: delete = %v, want ErrNotFound", name, err)
		}
		if _, err := s.NotifyHookSigningSecrets(ctx, h.ID, intruder); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: secrets = %v, want ErrNotFound", name, err)
		}
		if list, err := s.ListNotifyHooks(ctx, intruder); err != nil || len(list) != 0 {
			t.Errorf("%s: list = %+v, %v, want empty", name, list, err)
		}
	}
	after, err := s.GetNotifyHook(ctx, h.ID, owner)
	if err != nil || after.RotatedAt != nil {
		t.Fatalf("owner's hook changed by an intruder: %+v, %v", after, err)
	}
	if sec, err := s.NotifyHookSigningSecrets(ctx, h.ID, owner); err != nil || sec.Current != "whsec_owner" || sec.Previous != "" {
		t.Fatalf("owner's secret changed by an intruder: %+v, %v", sec, err)
	}
}

// TestNotifyHookCascadesWithEndpoint: deleting an endpoint takes its hooks (and their secrets).
func TestNotifyHookCascadesWithEndpoint(t *testing.T) {
	s, ctx := hookStore(t)
	ep := seedEndpoint(t, s, ctx, "nh-cascade")
	if _, err := s.CreateNotifyHook(ctx, ep, testHookURL, nil, false, "whsec_x", 5); err != nil {
		t.Fatalf("create: %v", err)
	}
	// The product path: revoke, then permanently delete.
	owner := ownerOf(t, s, ctx, ep)
	if err := s.RevokeEndpoint(ctx, ep, owner); err != nil {
		t.Fatalf("revoke endpoint: %v", err)
	}
	if err := s.DeleteEndpoint(ctx, ep, owner); err != nil {
		t.Fatalf("delete endpoint: %v", err)
	}
	if n := countHooks(t, s, ctx, ep); n != 0 {
		t.Fatalf("hooks survived their endpoint: %d", n)
	}
}

// TestNotifyHookRotation: rotation keeps the previous secret for the grace, re-enables a hook REQ-8
// disabled, leaves an operator disable alone, and the expiry sweep destroys the old secret.
func TestNotifyHookRotation(t *testing.T) {
	s, ctx := hookStore(t)
	ep := seedEndpoint(t, s, ctx, "nh-rotate")
	h, err := s.CreateNotifyHook(ctx, ep, testHookURL, nil, false, "whsec_first", 5)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE notify_hooks SET enabled = false, disabled_reason = 'consecutive_failures',
		disabled_at = now(), consecutive_failures = 10 WHERE id = $1`, h.ID); err != nil {
		t.Fatalf("simulate auto-disable: %v", err)
	}

	rot, err := s.RotateNotifyHookSecret(ctx, h.ID, ep, "whsec_second", 24*time.Hour)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if !rot.Enabled || rot.DisabledReason != nil || rot.DisabledAt != nil || rot.ConsecutiveFailures != 0 {
		t.Fatalf("rotate did not re-enable an auto-disabled hook: %+v", rot)
	}
	if rot.RotatedAt == nil || rot.PrevSecretExpiresAt == nil ||
		rot.PrevSecretExpiresAt.Before(time.Now().Add(23*time.Hour)) {
		t.Fatalf("rotation grace not recorded: %+v", rot)
	}
	sec, err := s.NotifyHookSigningSecrets(ctx, h.ID, ep)
	if err != nil || sec.Current != "whsec_second" || sec.Previous != "whsec_first" || sec.PreviousExpiresAt == nil {
		t.Fatalf("secrets in grace = %+v, %v", sec, err)
	}
	_, prev := rawHookSecrets(t, s, ctx, h.ID)
	if prev == nil || !strings.HasPrefix(*prev, "enc:v1:") {
		t.Fatal("previous secret not held sealed during the grace")
	}

	// An operator disable survives a rotation.
	if _, err := s.pool.Exec(ctx, `UPDATE notify_hooks SET enabled = false, disabled_reason = 'operator',
		disabled_at = now() WHERE id = $1`, h.ID); err != nil {
		t.Fatalf("simulate operator disable: %v", err)
	}
	rot2, err := s.RotateNotifyHookSecret(ctx, h.ID, ep, "whsec_third", -time.Second) // grace already over
	if err != nil {
		t.Fatalf("rotate 2: %v", err)
	}
	if rot2.Enabled || rot2.DisabledReason == nil || *rot2.DisabledReason != NotifyHookDisabledOperator {
		t.Fatalf("rotate re-enabled an operator-disabled hook: %+v", rot2)
	}

	// Past its grace the previous secret is not handed out, and the sweep destroys it.
	sec, err = s.NotifyHookSigningSecrets(ctx, h.ID, ep)
	if err != nil || sec.Current != "whsec_third" || sec.Previous != "" {
		t.Fatalf("secrets after grace = %+v, %v", sec, err)
	}
	n, err := s.DestroyExpiredNotifyHookSecrets(ctx)
	if err != nil || n != 1 {
		t.Fatalf("destroy expired = %d, %v, want 1", n, err)
	}
	if _, prev := rawHookSecrets(t, s, ctx, h.ID); prev != nil {
		t.Fatal("expired previous secret still stored after the sweep")
	}
	if n, err := s.DestroyExpiredNotifyHookSecrets(ctx); err != nil || n != 0 {
		t.Fatalf("second sweep = %d, %v, want 0", n, err)
	}
}

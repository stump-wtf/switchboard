package notifyhook

// End to end over the real store: a verified delivery creates a todo, the store's ready hook hands
// it to the dispatcher, and an httptest TLS receiver gets one signed POST that the spec-written
// reference verifier accepts, under the secret the store sealed. An unverified delivery and another
// tenant's todo reach nothing; a lease expiry re-fires with reason "requeued". Skips without
// SWITCHBOARD_TEST_DATABASE_URL; CI provides it.
//
// Governing: SPEC-0024 REQ-1 (tenancy), REQ-4, REQ-6 ("Lease expiry re-fires", "Unverified
// delivery calls no hook"), the #358 acceptance criterion for an httptest TLS end-to-end test.

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stump-wtf/switchboard/internal/cred"
	"github.com/stump-wtf/switchboard/internal/db"
	"github.com/stump-wtf/switchboard/internal/store"
)

func e2ePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("SWITCHBOARD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SWITCHBOARD_TEST_DATABASE_URL to run the notify-hook end-to-end test")
	}
	ctx := context.Background()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	const testDB = "switchboard_test_notifyhook"
	if u.Path != "/"+testDB {
		admin, err := db.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("connect (admin): %v", err)
		}
		if _, err := admin.Exec(ctx, `CREATE DATABASE `+testDB); err != nil {
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "42P04" {
				admin.Close()
				t.Fatalf("create test database: %v", err)
			}
		}
		admin.Close()
		u.Path = "/" + testDB
	}
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE humans, agents, endpoints, todos, events RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool, ctx
}

func e2eEndpoint(t *testing.T, ctx context.Context, st *store.Store, label string) string {
	t.Helper()
	h, err := st.UpsertHuman(ctx, "e2e|"+label, label, label+"@example.test")
	if err != nil {
		t.Fatalf("human: %v", err)
	}
	ag, err := st.CreateAgent(ctx, h.ID, label, "")
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	_, hash, prefix, err := cred.Mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	ep, err := st.CreateEndpoint(ctx, ag.ID, hash, prefix, label+"-12345678", []string{"inbox"}, []string{"list_todos"})
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	return ep.ID
}

func TestDispatchEndToEndOverStore(t *testing.T) {
	pool, ctx := e2ePool(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(200 - i)
	}
	box, err := cred.NewSecretBox(key)
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(pool, store.WithSecretCipher(box))
	epA := e2eEndpoint(t, ctx, st, "hook-a")
	epB := e2eEndpoint(t, ctx, st, "hook-b")

	rcv := newReceiver(t)
	secret, err := MintSecret()
	if err != nil {
		t.Fatal(err)
	}
	hook, err := st.CreateNotifyHook(ctx, epA, rcv.hookURL("/sb"), nil, false, secret, 5)
	if err != nil {
		t.Fatalf("create hook: %v", err)
	}

	h := newHarness(t, nil, rcv, func(o *Options) { o.Store = st })
	st.SetTodoReadyHook(h.d.Enqueue)
	defer st.SetTodoReadyHook(nil)

	create := func(ep, key string, verified bool) store.Todo {
		_, td, created, err := st.CreateEventTodo(ctx,
			store.EventInput{Source: "gitea", Family: "webhook", EventType: "issues", ExternalID: key,
				TrustMode: "signed", Verified: verified, Payload: []byte(`{"secret":"payload-value"}`)},
			store.CreateTodoParams{EndpointID: ep, Queue: "inbox", Source: "gitea", Kind: "issue", Title: "issue " + key,
				Payload: []byte(`{"secret":"payload-value"}`), IdempotencyKey: key})
		if err != nil || !created {
			t.Fatalf("create %s: created=%v err=%v", key, created, err)
		}
		return td
	}

	// Another tenant's verified todo and an unverified one reach nothing.
	create(epB, "e2e-b", true)
	create(epA, "e2e-unverified", false)
	h.quiet(t)

	td := create(epA, "e2e-1", true)
	dl := h.wait(t)
	if !dl.Delivered || dl.HookID != hook.ID {
		t.Fatalf("delivery = %+v", dl)
	}
	reqs := rcv.got()
	if len(reqs) != 1 {
		t.Fatalf("receiver saw %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if !referenceVerify(secret, r.header.Get("webhook-id"), r.header.Get("webhook-timestamp"), r.body, r.header.Get("webhook-signature")) {
		t.Fatal("reference verifier rejected a notification signed under the stored (sealed) secret")
	}
	var body map[string]any
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatal(err)
	}
	if body["todo_id"] != td.ID || body["reason"] != "created" || body["endpoint"] != "hook-a-12345678" ||
		strings.Contains(string(r.body), "payload-value") {
		t.Fatalf("body = %s", r.body)
	}

	// Scenario "Lease expiry re-fires": a consumer claims and dies; the reaper requeues it.
	if _, err := st.ClaimTodo(ctx, epA, td.ID, "agent:dead", time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE todos SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, td.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n, err := st.ReapExpired(ctx); err != nil || n != 1 {
		t.Fatalf("reap = %d, %v", n, err)
	}
	h.wait(t)
	reqs = rcv.got()
	if len(reqs) != 2 {
		t.Fatalf("receiver saw %d requests after the requeue, want 2", len(reqs))
	}
	body = nil
	_ = json.Unmarshal(reqs[1].body, &body)
	if body["reason"] != "requeued" || body["attempt"] != float64(1) || body["todo_id"] != td.ID {
		t.Fatalf("requeue body = %s", reqs[1].body)
	}
	if reqs[0].header.Get("webhook-id") == reqs[1].header.Get("webhook-id") {
		t.Fatal("a new transition reused the previous notification's webhook-id")
	}
}

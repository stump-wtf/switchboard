package web

// Live Attempt Refresh Against Postgres
//
// A committed transition's SSE frame carries the attempts_oob swap for an open drawer, read under
// the owning human the frame is routed to. This is the one web test that needs a store: without
// one PublishTodoTransition skips every read-backed fragment. It skips without
// SWITCHBOARD_TEST_DATABASE_URL, like the store and server suites.
//
// Governing: SPEC-0034 REQ-13, Accessibility Requirements "Dynamic Content Regions"; SPEC-0015 REQ
// "Live Fragment Architecture".
//
// @joestump-agent 09/25/2026 - Added for #329 (epic #313).

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/config"
	"github.com/stump-wtf/switchboard/internal/db"
	"github.com/stump-wtf/switchboard/internal/store"
)

// newDBHandler is newTestHandler over a real store, on a database of this package's own.
func newDBHandler(t *testing.T) (*Handler, *store.Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("SWITCHBOARD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SWITCHBOARD_TEST_DATABASE_URL to run the live attempt refresh test")
	}
	ctx := context.Background()
	const webTestDB = "switchboard_test_web"
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect (admin): %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+webTestDB); err != nil &&
		!strings.Contains(err.Error(), "42P04") { // duplicate_database: already provisioned
		admin.Close()
		t.Fatalf("create test database: %v", err)
	}
	admin.Close()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test dsn: %v", err)
	}
	u.Path = "/" + webTestDB
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
	st := store.New(pool)
	h, err := New(st, config.Config{BaseURL: "https://sb.example.com"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h, st, ctx
}

func TestLiveTransitionRefreshesTheDrawerAttempts(t *testing.T) {
	h, st, ctx := newDBHandler(t)
	human, err := st.UpsertHuman(ctx, "pocket|live-attempts", "Live Attempts", "live-attempts@example.com")
	if err != nil {
		t.Fatalf("human: %v", err)
	}
	ag, err := st.CreateAgent(ctx, human.ID, "live-attempts", "")
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	slug, err := store.MintSlug(ag.Name)
	if err != nil {
		t.Fatalf("slug: %v", err)
	}
	ep, err := st.CreateEndpoint(ctx, ag.ID, "credhash-live-attempts", "sbk_live", slug, []string{"q"}, []string{"claim"})
	if err != nil {
		t.Fatalf("endpoint: %v", err)
	}
	td, _, err := st.CreateTodo(ctx, store.CreateTodoParams{EndpointID: ep.ID, Queue: "q", Title: "live"})
	if err != nil {
		t.Fatalf("todo: %v", err)
	}
	claimed, _, err := st.ClaimTodoWith(ctx, ep.ID, td.ID, "agent:w", store.ClaimOpts{TTL: time.Hour, Claimant: "<b>fixer</b>/run-1"})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	ch, cancel, err := h.events.subscribe(human.ID, "s")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()
	h.PublishTodoTransition("claimed", claimed)

	frame := recvFrame(t, ch)
	if frame.Name != "todo_claimed" {
		t.Fatalf("frame = %q, want todo_claimed", frame.Name)
	}
	carrier := `<div hx-swap-oob="innerHTML:#sb-attempts-` + td.ID + `">`
	at := strings.Index(frame.Data, carrier)
	if at < 0 {
		t.Fatalf("claim frame has no attempts_oob swap for the open drawer: %.400q", frame.Data)
	}
	oob := frame.Data[at:]
	for _, want := range []string{`data-sb-attempt="1" data-sb-outcome="open"`, "in progress", "&lt;b&gt;fixer&lt;/b&gt;/run-1"} {
		if !strings.Contains(oob, want) {
			t.Errorf("live attempts swap missing %q", want)
		}
	}
	assertCarriersOnly(t, "live claim frame", frame.Data)
}

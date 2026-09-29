package mcp

// DB-backed tenant isolation for the SPEC-0024 notify-hook verbs, over the real store: every verb,
// called by a second human's endpoint and by a second endpoint of the SAME human, answers exactly as
// for an unknown hook, and changes nothing. Skips without SWITCHBOARD_TEST_DATABASE_URL.
//
// Governing: SPEC-0024 REQ-2 scenario "Another endpoint's hook id", REQ-1 (a hook inherits its
// endpoint's owner scope).

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stump-wtf/switchboard/internal/cred"
	"github.com/stump-wtf/switchboard/internal/push"
	"github.com/stump-wtf/switchboard/internal/store"
)

func hookDBSession(t *testing.T, ctx context.Context, st *store.Store, slug, token string) *sdk.ClientSession {
	t.Helper()
	h := New(st, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	h.SetNotifyHooks(NotifyHookConfig{
		Store:     st,
		Validator: push.New(push.WithResolver(newHookResolver())),
		Max:       5,
	})
	r := chi.NewRouter()
	r.Mount("/mcp", h.Routes())
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	t.Cleanup(h.Close)
	client := sdk.NewClient(&sdk.Implementation{Name: "hook-test-client", Version: "0.0.1"}, nil)
	cs, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:   ts.URL + "/mcp/" + slug,
		HTTPClient: &http.Client{Transport: bearerTransport{token}},
	}, nil)
	if err != nil {
		t.Fatalf("initialize handshake: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestNotifyHookVerbsTenantIsolation(t *testing.T) {
	pool, ctx := routeTestPool(t)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(0x11 * (i % 7))
	}
	box, err := cred.NewSecretBox(key)
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	st := store.New(pool, store.WithSecretCipher(box))

	humanA := mustHuman(t, ctx, st, "hooks-a", "Human A")
	humanB := mustHuman(t, ctx, st, "hooks-b", "Human B")
	agentA1 := mustAgent(t, ctx, st, humanA, "hooks-a1")
	agentA2 := mustAgent(t, ctx, st, humanA, "hooks-a2")
	agentB := mustAgent(t, ctx, st, humanB, "hooks-b")
	verbs := NotifyHookVerbs()
	_, tokenA1 := mustEndpoint(t, ctx, st, agentA1, "hooks-a1-11111111", verbs)
	_, tokenA2 := mustEndpoint(t, ctx, st, agentA2, "hooks-a2-22222222", verbs)
	_, tokenB := mustEndpoint(t, ctx, st, agentB, "hooks-b-33333333", verbs)

	owner := hookDBSession(t, ctx, st, "hooks-a1-11111111", tokenA1)
	var created createNotifyHookOut
	callOK(t, ctx, owner, "create_notify_hook", map[string]any{"url": "https://dispatch.example.com/sb", "queues": []string{"reviews"}}, &created)

	for name, cs := range map[string]*sdk.ClientSession{
		"second human":               hookDBSession(t, ctx, st, "hooks-b-33333333", tokenB),
		"same human, other endpoint": hookDBSession(t, ctx, st, "hooks-a2-22222222", tokenA2),
	} {
		t.Run(name, func(t *testing.T) {
			var list listNotifyHooksOut
			callOK(t, ctx, cs, "list_notify_hooks", map[string]any{}, &list)
			if len(list.Hooks) != 0 || list.Ceiling.Used != 0 {
				t.Fatalf("intruder sees hooks: %+v", list)
			}
			for _, verb := range []string{"rotate_notify_hook", "delete_notify_hook"} {
				msg := callErr(t, ctx, cs, verb, map[string]any{"hook_id": created.HookID}, codeNotFound)
				// Identical to an unknown id: the message never confirms the id exists.
				unknown := callErr(t, ctx, cs, verb, map[string]any{"hook_id": "00000000-0000-4000-8000-000000000000"}, codeNotFound)
				if msg != unknown {
					t.Fatalf("%s: foreign id answers %q, unknown id %q", verb, msg, unknown)
				}
			}
		})
	}

	// The owner's hook is untouched: still listed, never rotated, and its secret unchanged.
	var list listNotifyHooksOut
	callOK(t, ctx, owner, "list_notify_hooks", map[string]any{}, &list)
	if len(list.Hooks) != 1 || list.Hooks[0].RotatedAt != nil {
		t.Fatalf("owner's hook changed: %+v", list)
	}
	var epID string
	if err := pool.QueryRow(ctx, `SELECT endpoint_id::text FROM notify_hooks WHERE id = $1`, created.HookID).Scan(&epID); err != nil {
		t.Fatalf("owner endpoint: %v", err)
	}
	sec, err := st.NotifyHookSigningSecrets(ctx, created.HookID, epID)
	if err != nil || sec.Current != created.SigningSecret || sec.Previous != "" {
		t.Fatalf("owner's secret changed by an intruder (err %v)", err)
	}
	// At rest the secret is sealed.
	var raw string
	if err := pool.QueryRow(ctx, `SELECT secret FROM notify_hooks WHERE id = $1`, created.HookID).Scan(&raw); err != nil {
		t.Fatalf("raw secret: %v", err)
	}
	if !strings.HasPrefix(raw, "enc:v1:") {
		t.Fatal("hook secret is not sealed at rest")
	}
}

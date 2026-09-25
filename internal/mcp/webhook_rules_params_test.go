package mcp

// set_webhook_rules params semantics: an omitted params keeps the saved ones, an explicit {} clears
// them, and every rules verb echoes the params in force. The distinction rides on the SDK decoding
// an absent field to a nil map and {} to an empty, non-nil one, so both halves are tested through
// the real SDK decode path: in memory without a database, and end to end against the real store.
//
// Governing: SPEC-0026 REQ-4 "Params Are Never Cleared by Omission"; ADR-0031.

import (
	"context"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The SDK's typed-tool decode is what tells "omitted" from "cleared": pin that it keeps them apart.
func TestSetWebhookRulesInDecodesOmittedParamsAsNil(t *testing.T) {
	ctx := context.Background()
	srv := sdk.NewServer(&sdk.Implementation{Name: "decode-test", Version: "0.0.1"}, nil)
	var got map[string]any
	var gotNil bool
	sdk.AddTool(srv, &sdk.Tool{Name: "set_webhook_rules"},
		func(_ context.Context, _ *sdk.CallToolRequest, in setWebhookRulesIn) (*sdk.CallToolResult, webhookRulesOut, error) {
			got, gotNil = in.Params, in.Params == nil
			return nil, webhookRulesOut{Rules: []ruleIO{}, Params: map[string]any{}}, nil
		})
	st, ct := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "decode-client", Version: "0.0.1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	for _, tc := range []struct {
		name    string
		args    map[string]any
		wantNil bool
		wantLen int
	}{
		{"omitted", map[string]any{"webhook_id": "wh", "rules": []any{}}, true, 0},
		{"explicit empty", map[string]any{"webhook_id": "wh", "rules": []any{}, "params": map[string]any{}}, false, 0},
		{"populated", map[string]any{"webhook_id": "wh", "rules": []any{}, "params": map[string]any{"k": "v"}}, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, gotNil = nil, false
			var out webhookRulesOut
			callOK(t, ctx, cs, "set_webhook_rules", tc.args, &out)
			if gotNil != tc.wantNil || len(got) != tc.wantLen {
				t.Fatalf("decoded params = %#v (nil=%v), want nil=%v len=%d", got, gotNil, tc.wantNil, tc.wantLen)
			}
		})
	}
}

// callRaw returns a verb's structured content as a plain map, so a test can tell an echoed {} from
// an absent key.
func callRaw(t *testing.T, ctx context.Context, cs *sdk.ClientSession, tool string, args map[string]any) map[string]any {
	t.Helper()
	var raw map[string]any
	callOK(t, ctx, cs, tool, args, &raw)
	return raw
}

// paramsOf asserts the response carries a params object and returns it.
func paramsOf(t *testing.T, tool string, raw map[string]any) map[string]any {
	t.Helper()
	v, ok := raw["params"]
	if !ok {
		t.Fatalf("%s response has no params key: %v", tool, raw)
	}
	p, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s response params = %#v, want an object", tool, v)
	}
	return p
}

func trustedHumans(p map[string]any) []any {
	h, _ := p["trusted_humans"].([]any)
	return h
}

func TestSetWebhookRulesKeepsOmittedParamsAndClearsOnEmpty(t *testing.T) {
	ctx, f, open := ruleSessions(t)
	cs, _ := open("A")

	rules := []any{map[string]any{"id": "only", "expr": "true", "action": map[string]any{"queue": "reviews"}}}
	set := func(extra map[string]any) map[string]any {
		args := map[string]any{"webhook_id": f.webhookA, "rules": rules, "default_action": map[string]any{"drop": true}}
		for k, v := range extra {
			args[k] = v
		}
		return callRaw(t, ctx, cs, "set_webhook_rules", args)
	}

	// Before anything is saved, the echo is {} rather than an absent key.
	if p := paramsOf(t, "list_webhook_rules", callRaw(t, ctx, cs, "list_webhook_rules", map[string]any{"webhook_id": f.webhookA})); len(p) != 0 {
		t.Fatalf("fresh webhook params = %v, want {}", p)
	}

	if h := trustedHumans(paramsOf(t, "set_webhook_rules", set(map[string]any{"params": map[string]any{"trusted_humans": []string{"joestump"}}}))); len(h) != 1 || h[0] != "joestump" {
		t.Fatalf("populated set echoed trusted_humans = %v, want [joestump]", h)
	}

	// REQ-4 "Omitting params keeps them": the save still replaces the rules and the default.
	raw := set(map[string]any{})
	if h := trustedHumans(paramsOf(t, "set_webhook_rules", raw)); len(h) != 1 || h[0] != "joestump" {
		t.Fatalf("set without params echoed trusted_humans = %v, want [joestump] kept", h)
	}
	listed := callRaw(t, ctx, cs, "list_webhook_rules", map[string]any{"webhook_id": f.webhookA})
	if h := trustedHumans(paramsOf(t, "list_webhook_rules", listed)); len(h) != 1 || h[0] != "joestump" {
		t.Fatalf("stored trusted_humans after omitting params = %v, want [joestump]", h)
	}

	// Every rules verb that returns a configuration echoes the params in force.
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"add_webhook_rule", map[string]any{"webhook_id": f.webhookA, "id": "second", "expr": "false", "action": map[string]any{"drop": true}}},
		{"update_webhook_rule", map[string]any{"webhook_id": f.webhookA, "rule_id": "second", "name": "renamed"}},
		{"move_webhook_rule", map[string]any{"webhook_id": f.webhookA, "rule_id": "second", "position": 0}},
		{"remove_webhook_rule", map[string]any{"webhook_id": f.webhookA, "rule_id": "second"}},
	} {
		if h := trustedHumans(paramsOf(t, call.tool, callRaw(t, ctx, cs, call.tool, call.args))); len(h) != 1 {
			t.Fatalf("%s echoed trusted_humans = %v, want [joestump]", call.tool, h)
		}
	}

	// REQ-4 "Explicit clear": params {} empties them and the response echoes {}.
	if p := paramsOf(t, "set_webhook_rules", set(map[string]any{"params": map[string]any{}})); len(p) != 0 {
		t.Fatalf("explicit clear echoed params = %v, want {}", p)
	}
	listed = callRaw(t, ctx, cs, "list_webhook_rules", map[string]any{"webhook_id": f.webhookA})
	if p := paramsOf(t, "list_webhook_rules", listed); len(p) != 0 {
		t.Fatalf("stored params after explicit clear = %v, want {}", p)
	}
	var out webhookRulesOut
	callOK(t, ctx, cs, "list_webhook_rules", map[string]any{"webhook_id": f.webhookA}, &out)
	if len(out.Rules) != 1 || out.Rules[0].ID != "only" || out.DefaultAction == nil || !out.DefaultAction.Drop {
		t.Fatalf("rules after clear = %+v, want [only] with a drop default", out)
	}
}

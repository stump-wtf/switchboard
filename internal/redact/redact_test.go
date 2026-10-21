package redact

// Tests for SPEC-0032 REQ-3's redaction helper: the rule is keyed on the FIELD NAME, so every surface
// that dumps tool arguments or a request body loses the value without enumerating call sites.
//
// Governing: SPEC-0032 REQ-3 ("the secret MUST NOT appear in any response, page, log line, … or error
// message"), design "one helper, keyed on the field name".

import (
	"encoding/json"
	"strings"
	"testing"
)

const probe = "whsec_CanNeverBePrintedAnywhere"

// TestSecretsReplacesAtEveryDepth: a top-level signing_secret, one nested inside a tool-call argument
// object, and one inside a list all become the placeholder, and the caller's map is untouched — it is
// the same map the tool is about to act on.
func TestSecretsReplacesAtEveryDepth(t *testing.T) {
	args := map[string]any{
		"source_type":      "stripe",
		"signing_secret":   probe,
		"target_queue":     "reviews",
		"nested":           map[string]any{"signing_secret": probe, "kept": "value"},
		"list":             []any{map[string]any{"signing_secret": probe}},
		"unrelated_nested": map[string]any{"token": "keep-me"},
	}
	got := Secrets(args)

	if strings.Contains(marshal(t, got), probe) {
		t.Fatalf("a supplied value survived redaction: %s", marshal(t, got))
	}
	if v, ok := got["signing_secret"].(string); !ok || v != Placeholder {
		t.Errorf("top-level signing_secret = %#v, want %s", got["signing_secret"], Placeholder)
	}
	if v := nestedString(t, got, "nested", "signing_secret"); v != Placeholder {
		t.Errorf("nested signing_secret = %q, want %s", v, Placeholder)
	}
	if v := nestedString(t, got, "nested", "kept"); v != "value" {
		t.Errorf("sibling field changed: %q", v)
	}
	list, _ := got["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("list element lost: %#v", list)
	}
	if first, ok := list[0].(map[string]any); !ok || first["signing_secret"] != Placeholder {
		t.Fatalf("list element not redacted: %#v", list)
	}
	if v := nestedString(t, got, "unrelated_nested", "token"); v != "keep-me" {
		t.Errorf("non-sensitive field changed: %q", v)
	}
	// The input must survive: redacting the log line cannot change what the request does.
	if args["signing_secret"] != probe {
		t.Errorf("Secrets mutated the caller's map: %#v", args["signing_secret"])
	}
	if nestedString(t, args, "nested", "signing_secret") != probe {
		t.Errorf("Secrets mutated a nested map of the caller's")
	}
}

// TestBodyRedactsJSONBodiesOnly: a JSON body loses the value wherever the field name appears; a
// non-JSON body is returned untouched rather than dropped.
func TestBodyRedactsJSONBodiesOnly(t *testing.T) {
	raw := []byte(`{"name":"set_webhook_secret","arguments":{"signing_secret":"` + probe + `","max":3}}`)
	got := string(Body(raw))
	if strings.Contains(got, probe) {
		t.Fatalf("Body leaked the secret: %s", got)
	}
	if !strings.Contains(got, Placeholder) || !strings.Contains(got, `"max":3`) {
		t.Errorf("Body = %s, want the placeholder and the other fields intact", got)
	}
	if passthrough := string(Body([]byte("not json at all"))); passthrough != "not json at all" {
		t.Errorf("non-JSON body = %q, want it unchanged", passthrough)
	}
	if empty := Body(nil); len(empty) != 0 {
		t.Errorf("empty body = %q, want empty", empty)
	}
}

// TestFieldNamesCoverTheSecretFields: the list is the single place a secret-bearing field is declared,
// so a new one has to be added here and nowhere else.
func TestFieldNamesCoverTheSecretFields(t *testing.T) {
	found := false
	for _, f := range FieldNames {
		if f == "signing_secret" {
			found = true
		}
	}
	if !found {
		t.Fatalf("FieldNames = %v, must list signing_secret", FieldNames)
	}
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func nestedString(t *testing.T, from map[string]any, key, field string) string {
	t.Helper()
	if field == "" {
		return ""
	}
	m, ok := from[key].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m[field].(string)
	return s
}

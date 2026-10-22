// Package redact turns the secret-bearing fields of a value destined for a log line, a trace span or
// a debug dump into «redacted», keyed on the FIELD NAME rather than on the call site.
//
// The rule it exists to enforce is SPEC-0032 REQ-3: a signing secret must appear in no response,
// page, log line, metric label, routing trace, telemetry span, event row or error message. Call sites
// that enumerate which fields they print are how that rule gets broken by the next field someone
// adds, so this package keys on the name: any map or JSON body handed to it loses the value at
// `signing_secret`, at any depth, and a new path that forgets to ask is the only way a value gets
// through.
//
// Governing: SPEC-0032 REQ-3, design "Secret writes are audited in their own table" (one helper,
// keyed on the field name); ADR-0037.
package redact

import (
	"encoding/json"
	"strings"
)

// Placeholder replaces a secret value wherever it would otherwise be printed. It matches the token
// ingest already uses for sanitized headers, so one glance at a log line says the same thing anywhere
// in the system.
const Placeholder = "«redacted»"

// FieldNames are the argument and body field names whose VALUES must never be printed. A field is
// listed here once, and every surface that dumps tool arguments or request bodies then gets it for
// free.
var FieldNames = []string{"signing_secret"}

// Secrets returns a copy of args with every value under a sensitive field name replaced by
// Placeholder, at any depth. The input is never modified: callers hand it the map they were about to
// log, and a redaction that mutated the request would change what the tool does.
func Secrets(args map[string]any) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = SecretsValue(v, sensitive(k))
	}
	return out
}

// SecretsValue applies the same rule to an arbitrary decoded value (a slice of arguments, a nested
// object). Redacted is forced once a sensitive key is entered, so a value cannot smuggle itself back
// by nesting.
func SecretsValue(v any, redacted bool) any {
	if redacted {
		return Placeholder
	}
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = SecretsValue(val, sensitive(k))
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = SecretsValue(val, false)
		}
		return out
	default:
		return v
	}
}

// Body redacts a JSON request body for logging. Anything that is not a JSON object is returned
// unchanged: bodies are printed as-is by some handlers, and a secret can only arrive here under a
// field name this package knows.
func Body(raw []byte) []byte {
	if len(raw) == 0 {
		return raw
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return raw
	}
	red, err := json.Marshal(SecretsValue(decoded, false))
	if err != nil {
		return raw
	}
	return red
}

func sensitive(name string) bool {
	lower := strings.ToLower(name)
	for _, f := range FieldNames {
		if lower == f {
			return true
		}
	}
	return false
}

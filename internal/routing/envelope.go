package routing

// The routing envelope: the one JSON document every rule expression sees. Its shape is a contract
// with rule authors — paths here are documented in SPEC-0020 and must not move — so it is built in
// one place and used by the live receiver and the dry-run tool alike. Everything switchboard itself
// decided (source, trust_mode, verified, webhook_id) sits at the top level where a producer cannot
// forge it; everything the producer sent sits under .payload and .headers, and a source-specific
// projection (today: .artifact for cairn) gives stable names to fields that would otherwise be
// reached through a producer's nesting.
//
//	.source        webhook source type (switchboard-derived: github, gitea, cairn, generic, ...)
//	.kind          event kind: cairn's signed body `kind`, else X-GitHub-Event / X-Gitea-Event /
//	               X-Cairn-Event, else null
//	.webhook_id    the receiving webhook
//	.trust_mode    signed | token
//	.verified      true only when the delivery's signature verified
//	.content_type  request Content-Type, or null
//	.size          payload size in bytes
//	.headers       sanitized request headers, lower-cased names (secret values are «redacted»)
//	.payload       the body parsed as JSON, or null when it is not JSON
//	.artifact      cairn only (null otherwise): {event_id, kind, created_at, id, handle, url, title,
//	               share_type, channel, model, actor_id, on_behalf_of, expires_at, tags, metadata} —
//	               handle is mcp://cairn/<id>; tags is cairn's string list (handoff, lane:m, …)
//	.issue         a Gitea/GitHub `issues` event only (null otherwise, pull requests included):
//	               {provider, action, event_type, repo, number, title, url, state, author, sender,
//	               labels (names), label (GitHub's changed label, else null), body_size, label_event,
//	               key} — parsed in Go from the body (subject.go), identical across the two forges
//
// @joestump-agent 09/11/2026 - Added .issue and the cairn on_behalf_of/handle fields (ADR-0025); cairn
// handoffs are described by tags, not a label map.
//
// Governing: SPEC-0020 REQ "Deterministic Rule Evaluation" (rules evaluate the normalized event),
// REQ "Isolation and Tenant Safety" (switchboard-derived fields cannot be forged by the payload).

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"strings"
)

// SourceCairn is the cairn outbound-webhook source type (cairn ADR-0017 / SPEC-0012).
const SourceCairn = "cairn"

// EnvelopeInput carries what the envelope is built from. Headers must already be sanitized.
type EnvelopeInput struct {
	Source      string
	Kind        string
	WebhookID   string
	TrustMode   string
	Verified    bool
	ContentType string
	Headers     map[string]string
	Body        []byte
}

// Envelope builds the rule input. It uses only the value types gojq accepts (map[string]any,
// []any, string, bool, int, float64, nil).
func Envelope(in EnvelopeInput) map[string]any {
	headers := make(map[string]any, len(in.Headers))
	for k, v := range in.Headers {
		headers[strings.ToLower(k)] = v
	}
	payload := DecodePayload(in.Body)
	env := map[string]any{
		"source":       in.Source,
		"kind":         nilIfEmpty(in.Kind),
		"webhook_id":   in.WebhookID,
		"trust_mode":   in.TrustMode,
		"verified":     in.Verified,
		"content_type": nilIfEmpty(in.ContentType),
		"size":         len(in.Body),
		"headers":      headers,
		"payload":      payload,
		"artifact":     nil,
		"issue":        nil,
	}
	if in.Source == SourceCairn {
		env["artifact"] = cairnArtifact(payload)
	}
	if s := SubjectOf(in.Source, in.Headers, in.Body); s != nil && s.Type == SubjectIssue {
		env["issue"] = issueProjection(s)
	}
	return env
}

// EventKind derives .kind. For cairn the signed body's `kind` wins over the unsigned X-Cairn-Event
// header, so a tampered header cannot change what a rule sees on a verified delivery.
func EventKind(source string, header func(string) string, body []byte) string {
	if source == SourceCairn {
		if root, ok := DecodePayload(body).(map[string]any); ok {
			if k, ok := root["kind"].(string); ok && k != "" {
				return k
			}
		}
	}
	for _, name := range []string{"X-GitHub-Event", "X-Gitea-Event", "X-Cairn-Event"} {
		if v := header(name); v != "" {
			return v
		}
	}
	return ""
}

// DecodePayload parses a body as exactly one JSON value, or returns nil. Integers that fit int64
// stay exact ints rather than rounding through float64, so `.payload.id == 9007199254740993` means
// what it says.
func DecodePayload(body []byte) any {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil // trailing data: not a single JSON document
	}
	return normalizeNumbers(v)
}

func normalizeNumbers(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = normalizeNumbers(e)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = normalizeNumbers(e)
		}
		return t
	case json.Number:
		if n, err := strconv.ParseInt(string(t), 10, 64); err == nil && n >= math.MinInt && n <= math.MaxInt {
			return int(n)
		}
		if f, err := strconv.ParseFloat(string(t), 64); err == nil {
			return f
		}
		return string(t)
	default:
		return v
	}
}

// cairnArtifact projects cairn's artifact.created body onto stable names. tags and metadata are
// passed through when a cairn build sends them and are null otherwise, so a rule written against
// them today simply does not match until the producer carries them.
func cairnArtifact(payload any) map[string]any {
	root, _ := payload.(map[string]any)
	data, _ := root["data"].(map[string]any)
	out := map[string]any{
		"event_id":   root["event_id"],
		"kind":       root["kind"],
		"created_at": root["created_at"],
	}
	for _, k := range []string{"id", "url", "title", "share_type", "channel", "model", "actor_id", "on_behalf_of",
		"expires_at", "tags", "metadata"} {
		out[k] = data[k]
	}
	out["handle"] = nil
	if id, ok := data["id"].(string); ok && id != "" {
		out["handle"] = cairnHandlePrefix + id
	}
	return out
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

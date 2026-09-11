package ingest

// Unit tests for cairn's signed delivery scheme — no database. Cairn (SPEC-0012 REQ "Signed
// Delivery") signs only the raw body, so the replay defenses switchboard applies are carried by the
// signed body itself: its event_id and created_at. These cases pin each way a delivery can fail
// closed. The DB-backed round trip (dedup, routing, drop) lives in routing_selfmanaged_test.go.
//
// Governing: SPEC-0001 REQ "Signed Webhook Verification", REQ "Replay-Window Enforcement for
// Timestamped Signatures"; ADR-0024.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"
)

// cairnSig computes X-Cairn-Signature exactly as cairn's outboundhook does.
func cairnSig(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// cairnBody renders cairn's artifact.created wire format (cairn internal/outboundhook eventBody).
func cairnBody(eventID string, at time.Time, title string) string {
	return fmt.Sprintf(`{"source":"cairn","kind":"artifact.created","event_id":%q,"created_at":%q,`+
		`"data":{"id":"a1b2c3","share_type":"markdown","title":%q,"url":"https://cairn.example/a/a1b2c3","channel":"mcp","actor_id":"joe"}}`,
		eventID, at.UTC().Format(time.RFC3339Nano), title)
}

func TestVerifyCairn(t *testing.T) {
	const secret = "whsec_cairn_unit"
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	good := cairnBody("evt-1", now, "hello")
	noID := strings.Replace(good, `"event_id":"evt-1",`, "", 1)
	noCreated := strings.Replace(good, `"created_at":"2026-09-11T12:00:00Z",`, "", 1)
	badCreated := strings.Replace(good, `2026-09-11T12:00:00Z`, `yesterday`, 1)

	cases := []struct {
		name, body, sig, eventIDHeader string
		now                            time.Time
		want                           bool
	}{
		{"valid", good, cairnSig(secret, good), "", now, true},
		{"valid with matching event id header", good, cairnSig(secret, good), "evt-1", now, true},
		{"clock skew inside the window", good, cairnSig(secret, good), "", now.Add(-4 * time.Minute), true},
		{"tampered body", strings.Replace(good, "hello", "hellO", 1), cairnSig(secret, good), "", now, false},
		{"wrong secret", good, cairnSig("whsec_someone_else", good), "", now, false},
		{"missing signature", good, "", "", now, false},
		{"bare hex without the sha256= prefix", good, strings.TrimPrefix(cairnSig(secret, good), "sha256="), "", now, false},
		{"replay after the window", good, cairnSig(secret, good), "", now.Add(10 * time.Minute), false},
		{"dated in the future", good, cairnSig(secret, good), "", now.Add(-10 * time.Minute), false},
		{"unsigned event id header disagrees", good, cairnSig(secret, good), "evt-2", now, false},
		{"signed body without event_id", noID, cairnSig(secret, noID), "", now, false},
		{"signed body without created_at", noCreated, cairnSig(secret, noCreated), "", now, false},
		{"signed body with unparseable created_at", badCreated, cairnSig(secret, badCreated), "", now, false},
		{"signed non-JSON body", "not json", cairnSig(secret, "not json"), "", now, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := verifyCairn(secret, []byte(tc.body), tc.sig, tc.eventIDHeader, tc.now, defaultReplayTolerance)
			if got != tc.want {
				t.Fatalf("verifyCairn = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCairnEventIDIsTheSignedBodyID(t *testing.T) {
	if got := cairnEventID([]byte(cairnBody("evt-9", time.Now(), "x"))); got != "evt-9" {
		t.Fatalf("cairnEventID = %q, want evt-9", got)
	}
	if got := cairnEventID([]byte("not json")); got != "" {
		t.Fatalf("cairnEventID(non-JSON) = %q, want empty (body-hash fallback)", got)
	}
}

func TestSummarizeCairn(t *testing.T) {
	for body, want := range map[string]string{
		cairnBody("e", time.Now(), "Q3 audit"):                 "cairn artifact.created — Q3 audit (markdown)",
		`{"kind":"artifact.created","data":{"title":"notes"}}`: "cairn artifact.created — notes",
		`{"kind":"artifact.created","data":{"id":"zz9"}}`:      "cairn artifact.created zz9",
		`{}`: "cairn delivery",
		`{"kind":"artifact.created","data":{"title":"   ","share_type":"x"}}`: "cairn artifact.created",
	} {
		if got := summarizeCairn([]byte(body)); got != want {
			t.Fatalf("summarizeCairn(%s) = %q, want %q", body, got, want)
		}
	}
}

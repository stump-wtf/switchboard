package notifyhook

// The notification id and body. The body is a pointer, never the work: identifiers and a one-line
// summary, so a consumer has to claim the todo to learn anything else.
//
// Governing: SPEC-0024 REQ-4 (webhook-id "msg_<26-char ULID>"), REQ-5 "Notification Body".

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stump-wtf/switchboard/internal/store"
)

const (
	// MaxBodyBytes is the largest body a hook is ever sent (SPEC-0024 REQ-5).
	MaxBodyBytes = 4 << 10
	// maxSummaryRunes bounds the sender-supplied summary (SPEC-0024 REQ-5).
	maxSummaryRunes = 200
	// maxLabelRunes bounds kind and source, which also originate with the sender's delivery.
	maxLabelRunes = 128
	// TypeTodoReady is the only notification type this dispatcher sends; todos.backlog waits on
	// endpoint presence (SPEC-0024 REQ-9).
	TypeTodoReady = "todo.ready"
)

// crockford is the ULID alphabet.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newMessageID returns "msg_" + a 26-character ULID: 48 bits of millisecond time, then 80 random
// bits, in Crockford base32. One id names one notification across all of its attempts, so a
// receiver deduplicates on it.
func newMessageID(now time.Time) (string, error) {
	var b [16]byte
	ms := uint64(now.UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (8 * (5 - i)))
	}
	if _, err := rand.Read(b[6:]); err != nil {
		return "", fmt.Errorf("notifyhook: message id entropy: %w", err)
	}
	n := new(big.Int).SetBytes(b[:])
	out := make([]byte, 26)
	mask := big.NewInt(31)
	for i := 25; i >= 0; i-- {
		out[i] = crockford[new(big.Int).And(n, mask).Int64()]
		n.Rsh(n, 5)
	}
	return "msg_" + string(out), nil
}

// readyBody is exactly the SPEC-0024 REQ-5 field set. Nothing from the todo's payload, headers,
// routing trace or work order has a field here, so none of it can leave.
type readyBody struct {
	Type      string `json:"type"`
	Reason    string `json:"reason"`
	TodoID    string `json:"todo_id"`
	Queue     string `json:"queue"`
	Kind      string `json:"kind,omitempty"`
	Source    string `json:"source,omitempty"`
	Summary   string `json:"summary"`
	Endpoint  string `json:"endpoint"`
	Attempt   int    `json:"attempt"`
	CreatedAt string `json:"created_at"`
}

// buildReadyBody renders a todo.ready notification. created_at is when this notification was made
// (so a requeue carries a fresh time); the todo's own age is not something the receiver needs.
func buildReadyBody(t store.Todo, reason, endpointSlug string, now time.Time) ([]byte, error) {
	b := readyBody{
		Type: TypeTodoReady, Reason: reason, TodoID: t.ID, Queue: t.Queue,
		Kind: truncateRunes(neutralize(t.Kind), maxLabelRunes), Source: truncateRunes(neutralize(t.Source), maxLabelRunes),
		Summary:   truncateRunes(neutralize(t.Title), maxSummaryRunes),
		Endpoint:  endpointSlug,
		Attempt:   t.Attempt,
		CreatedAt: now.UTC().Format(time.RFC3339),
	}
	out, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("notifyhook: marshal body: %w", err)
	}
	if len(out) > MaxBodyBytes {
		return nil, fmt.Errorf("notifyhook: body is %d bytes, over the %d-byte limit", len(out), MaxBodyBytes)
	}
	return out, nil
}

// channelClose matches a literal </channel> in any case.
var channelClose = regexp.MustCompile(`(?i)</channel>`)

// neutralize applies the channel doorbell's neutralization (internal/mcp doorbell.go neutralize,
// duplicated because internal/mcp imports this package): a forged </channel> is defused and
// newlines collapse, so sender text stays one line of data wherever a consumer pastes it.
func neutralize(s string) string {
	s = channelClose.ReplaceAllString(s, "«/channel»")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// truncateRunes cuts s to at most n runes without splitting a UTF-8 sequence.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

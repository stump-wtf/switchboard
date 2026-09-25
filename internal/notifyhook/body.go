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

// notice is all a queued notification keeps of its todo: the fields the dispatcher matches on and
// the body carries, with the sender-supplied text already cut to its body limit. It deliberately
// holds no payload, routing trace or work order, so a full queue pins kilobytes, not the todos'
// multi-megabyte payloads. Neutralization still happens in buildReadyBody. It keeps the rune count
// (a </channel> becomes «/channel», a newline a space), so truncating first yields the same length
// of summary; a </channel> the limit cuts in half is left as an inert fragment.
type notice struct {
	todoID     string
	endpointID string
	queue      string
	kind       string
	source     string
	title      string
	attempt    int
	reason     string
}

// newNotice copies the notice out of t. The sender-supplied strings are cloned after truncation so
// the notice never shares (and so never pins) a larger backing string.
func newNotice(t store.Todo, reason string) notice {
	return notice{
		todoID: t.ID, endpointID: t.EndpointID, queue: t.Queue,
		kind:    strings.Clone(truncateRunes(t.Kind, maxLabelRunes)),
		source:  strings.Clone(truncateRunes(t.Source, maxLabelRunes)),
		title:   strings.Clone(truncateRunes(t.Title, maxSummaryRunes)),
		attempt: t.Attempt,
		reason:  reason,
	}
}

// buildReadyBody renders a todo.ready notification. attempt is the todo's attempt count, the number
// of times it has been claimed so far, so 0 on creation. created_at is when this notification was
// made (so a requeue carries a fresh time), not when the todo was created (SPEC-0024 REQ-5).
func buildReadyBody(n notice, endpointSlug string, now time.Time) ([]byte, error) {
	b := readyBody{
		Type: TypeTodoReady, Reason: n.reason, TodoID: n.todoID, Queue: n.queue,
		Kind: truncateRunes(neutralize(n.kind), maxLabelRunes), Source: truncateRunes(neutralize(n.source), maxLabelRunes),
		Summary:   truncateRunes(neutralize(n.title), maxSummaryRunes),
		Endpoint:  endpointSlug,
		Attempt:   n.attempt,
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

// truncateRunes cuts s to at most n runes without splitting a UTF-8 sequence. It walks at most n
// runes, so a huge title costs no more than a short one (this runs on the ingest path).
func truncateRunes(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

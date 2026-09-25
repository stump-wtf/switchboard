package notifyhook

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// newLockedLogger is a text logger whose writes are serialized, so a test can read the buffer while
// dispatcher workers are still logging.
func newLockedLogger(buf *bytes.Buffer, mu *sync.Mutex) *slog.Logger {
	return slog.New(slog.NewTextHandler(lockedWriter{buf: buf, mu: mu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type lockedWriter struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func TestMessageIDShape(t *testing.T) {
	a, err := newMessageID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newMessageID(time.Now().Add(time.Millisecond))
	if len(a) != 30 || a[:4] != "msg_" || a == b {
		t.Fatalf("ids %q %q", a, b)
	}
	for _, c := range a[4:] {
		if !bytes.ContainsRune([]byte(crockford), c) {
			t.Fatalf("id %q has a non-Crockford character %q", a, c)
		}
	}
	// Time-ordered: a later millisecond sorts after.
	if a[4:14] > b[4:14] {
		t.Fatalf("ULID time prefix not ordered: %q then %q", a, b)
	}
}

func TestHookLimiter(t *testing.T) {
	l := newHookLimiter(120)
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < 120; i++ {
		if !l.allow("h", now) {
			t.Fatalf("burst refused at %d", i)
		}
	}
	if l.allow("h", now) {
		t.Fatal("121st notification in the same instant allowed")
	}
	if !l.allow("other", now) {
		t.Fatal("limits leak across hooks")
	}
	if !l.allow("h", now.Add(500*time.Millisecond)) {
		t.Fatal("bucket did not refill at 2/s")
	}
}

func TestSleepCtxCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) {
		t.Fatal("sleepCtx ignored cancellation")
	}
}

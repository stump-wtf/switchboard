package mcp

// Tests for keep-alive on the standalone MCP notification stream (issue #44): an idle stream must
// put bytes on the wire often enough to survive an idle-timeout proxy, must never corrupt the SSE
// frames it shares the writer with, and must stop with the stream. Governing: SPEC-0014 REQ
// "Channels Push over the HTTP Stream".

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/store"
)

// recordingWriter is a streaming ResponseWriter that records the bytes it was handed and counts
// flushes, so a test can watch the wire without an HTTP server in the way.
type recordingWriter struct {
	mu      sync.Mutex
	buf     strings.Builder
	flushes int
	code    int
}

func (r *recordingWriter) Header() http.Header { return http.Header{} }

func (r *recordingWriter) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.code = code
}

func (r *recordingWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *recordingWriter) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushes++
}

func (r *recordingWriter) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

func (r *recordingWriter) flushCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flushes
}

// TestStreamKeepAliveTicksWithoutBreakingFrames: an open stream gets periodic comments, and a
// comment never lands in the middle of an event frame — every chunk the wire carries is either a
// whole keep-alive comment or a whole frame the SDK wrote.
func TestStreamKeepAliveTicksWithoutBreakingFrames(t *testing.T) {
	rec := &recordingWriter{}
	const frame = "event: message\ndata: {\"jsonrpc\":\"2.0\"}\n\n"
	k := newStreamKeepAliveWriter(rec, 3*time.Millisecond)
	defer k.stop()

	k.WriteHeader(http.StatusOK)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 150; i++ {
			if _, err := k.Write([]byte(frame)); err != nil {
				t.Errorf("write frame: %v", err)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("event writer did not finish")
	}

	out := rec.String()
	if !strings.Contains(out, keepAliveComment) {
		t.Fatalf("no keep-alive comment on an open idle stream:\n%s", trunc(out))
	}
	if rec.flushCount() == 0 {
		t.Fatal("keep-alive comments were never flushed, so a buffering proxy would hold them")
	}
	// Every frame boundary must be intact: splitting on the frame terminator must yield nothing
	// but complete comments and complete frames.
	parts := strings.Split(out, "\n\n")
	if parts[len(parts)-1] != "" {
		t.Fatalf("stream ends mid-frame: %q", parts[len(parts)-1])
	}
	comments, frames := 0, 0
	for _, p := range parts[:len(parts)-1] {
		switch p {
		case ": keep-alive":
			comments++
		case strings.TrimSuffix(frame, "\n\n"):
			frames++
		default:
			t.Fatalf("interleaved or corrupt frame on the wire: %q", trunc(p))
		}
	}
	if comments == 0 || frames != 150 {
		t.Fatalf("comments = %d, frames = %d, want at least one comment and all 150 frames", comments, frames)
	}
}

// TestStreamKeepAliveSilentOnNonStreamHead: a GET that never opens a stream — the SDK's 409 on a
// second GET of an already-open stream — gets no comments. A comment there would be body bytes on
// a response that is not a stream.
func TestStreamKeepAliveSilentOnNonStreamHead(t *testing.T) {
	rec := &recordingWriter{}
	k := newStreamKeepAliveWriter(rec, 2*time.Millisecond)
	defer k.stop()

	k.WriteHeader(http.StatusConflict)
	if _, err := k.Write([]byte("stream already open")); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	if out := rec.String(); strings.Contains(out, "keep-alive") {
		t.Fatalf("keep-alive written on a 409 response: %q", out)
	}
	if !strings.Contains(rec.String(), "stream already open") {
		t.Fatal("the response body must still pass through")
	}
	if rec.code != http.StatusConflict {
		t.Fatalf("status = %d, want the 409 the handler asked for", rec.code)
	}
}

// TestStreamKeepAliveStopsWithTheStream: stop() ends the ticking, and writes that arrive afterwards
// (a doorbell racing the handler's return) still reach the writer untouched.
func TestStreamKeepAliveStopsWithTheStream(t *testing.T) {
	rec := &recordingWriter{}
	k := newStreamKeepAliveWriter(rec, 2*time.Millisecond)

	k.WriteHeader(http.StatusOK)
	deadline := time.After(2 * time.Second)
	for rec.flushCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("keep-alive never ticked")
		case <-time.After(2 * time.Millisecond):
		}
	}

	k.stop()
	k.stop() // idempotent: the stream's handler path defers it, and a non-stream head can reach it too
	snapshot := rec.String()
	time.Sleep(30 * time.Millisecond)
	if out := rec.String(); out != snapshot {
		t.Fatalf("keep-alive kept ticking after stop():\n%s", trunc(strings.TrimPrefix(out, snapshot)))
	}

	if _, err := k.Write([]byte("event: message\ndata: {}\n\n")); err != nil {
		t.Fatalf("write after stop: %v", err)
	}
	if !strings.HasPrefix(rec.String(), snapshot+"event: message") {
		t.Fatalf("write after stop did not pass through: %q", trunc(rec.String()))
	}
}

// TestStandaloneStreamStaysAliveAndStillDelivers: end to end over the wire. An idle standalone
// stream emits comments often enough to survive a proxy idle cut, and the frames that share it —
// the catch-up ring on attach and a live doorbell — still arrive intact afterwards.
func TestStandaloneStreamStaysAliveAndStillDelivers(t *testing.T) {
	h, url, token, f := newDoorbellHarness(t)
	h.SetStreamKeepAlive(15 * time.Millisecond)

	f.mu.Lock()
	f.attachRings = []store.Todo{{EndpointID: harnessEndpointID, ID: "td_waiting", Queue: "reviews", Title: "waited through a restart"}}
	f.mu.Unlock()

	sess := rawInitialize(t, url, token)
	stream := openRawStream(t, sess)

	// The head comment the SDK writes (": ok") is not enough: the ticker has to keep coming.
	select {
	case <-stream.keepAlives:
	case <-time.After(5 * time.Second):
		t.Fatal("idle standalone stream emitted no keep-alive comment")
	}

	// The catch-up frame must survive the ticker: it rides the same writer.
	select {
	case n := <-stream.events:
		if n.Method != notificationChannel || n.Params.Meta["todo_id"] != "td_waiting" {
			t.Fatalf("catch-up doorbell corrupted by keep-alive: %+v", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("catch-up doorbell never arrived on a keep-alive-enabled stream")
	}

	// And so must a live push, delivered from the pump goroutine while the ticker is running.
	h.PublishTodoReady(store.Todo{EndpointID: harnessEndpointID, ID: "td_live", Queue: "reviews", Title: "pushed while ticking"})
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case n := <-stream.events:
			if n.Params.Meta["todo_id"] == "td_live" {
				return
			}
		case <-tick.C:
			h.PublishTodoReady(store.Todo{EndpointID: harnessEndpointID, ID: "td_live", Queue: "reviews", Title: "pushed while ticking"})
		case <-deadline:
			t.Fatal("doorbell never arrived while keep-alive was ticking")
		}
	}
}

// rawStream is a standalone notification stream read at the line level, which is the only way to
// see the comments: the decoding helpers in doorbell_test.go ignore them by design.
type rawStream struct {
	keepAlives <-chan struct{}
	events     <-chan notification
}

func openRawStream(t *testing.T, s *rawSession) rawStream {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.url, nil)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set(sessionIDHeader, s.id)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		t.Fatalf("stream status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	stop := make(chan struct{})
	t.Cleanup(func() { _ = resp.Body.Close() })
	t.Cleanup(func() { close(stop) })

	keepAlives := make(chan struct{}, 64)
	events := make(chan notification, 32)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var data strings.Builder
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, ":"):
				if line != strings.TrimSpace(keepAliveComment) {
					continue // the SDK's opening ": ok" comment is not a keep-alive
				}
				select {
				case keepAlives <- struct{}{}:
				case <-stop:
					return
				}
			case strings.HasPrefix(line, "data:"):
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			case line == "":
				if data.Len() == 0 {
					continue
				}
				var n notification
				if json.Unmarshal([]byte(data.String()), &n) == nil {
					select {
					case events <- n:
					case <-stop:
						return
					}
				}
				data.Reset()
			}
		}
	}()
	return rawStream{keepAlives: keepAlives, events: events}
}

// trunc keeps a failure message readable.
func trunc(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

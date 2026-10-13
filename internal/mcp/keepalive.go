package mcp

// Keep-alive for the standalone MCP notification stream.
//
// The hanging GET (session.go) is the doorbell channel, and while the queue is quiet the SDK has no
// reason to send a byte on it: it writes the head plus one ": ok" comment to force proxies to
// forward the stream, then goes silent. An idle-timeout proxy reads that silence as a dead
// connection — Cloudflare cancels roughly 125 s after connect — so a connected consumer is dropped
// on a schedule and stops hearing pushes while its queue keeps filling (stump.wtf/switchboard#44:
// "consumer attached but receiving nothing"). Periodic SSE comments hold the stream open, which is
// exactly how internal/web/sse.go holds the web UI stream open; a line beginning with ":" carries no
// protocol meaning and is ignored by every SSE client, so doorbell semantics are untouched.
//
// The wrapper is transparent: it ticks only after the stream's 200 head commits, never writes once
// the handler has returned, and takes the same lock the SDK's frame writes take, so a comment can
// never land in the middle of an event frame.
//
// Governing: SPEC-0014 REQ "Channels Push over the HTTP Stream" (the stream a doorbell is written
// to must stay open for the session's lifetime), SPEC-0011 REQ "Push Semantics".

import (
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	// keepAliveComment is one SSE comment frame: ignored by clients, but it puts a byte on the wire.
	keepAliveComment = ": keep-alive\n\n"

	// defaultStreamKeepAlive is the interval between keep-alive comments. It matches the web UI
	// stream (internal/web/sse.go defaultKeepAlive) and sits well inside the shortest idle timeout
	// seen in front of the service.
	defaultStreamKeepAlive = 15 * time.Second
)

// streamKeepAliveWriter wraps a notification stream's ResponseWriter and writes a keep-alive
// comment on a ticker for as long as the stream is open. Construct it with
// newStreamKeepAliveWriter and always call stop when the handler returns.
type streamKeepAliveWriter struct {
	http.ResponseWriter
	interval time.Duration

	mu      sync.Mutex
	started bool
	stopped bool
	non200  bool
	done    chan struct{}
}

func newStreamKeepAliveWriter(w http.ResponseWriter, interval time.Duration) *streamKeepAliveWriter {
	return &streamKeepAliveWriter{ResponseWriter: w, interval: interval, done: make(chan struct{})}
}

// WriteHeader starts the ticker on a 200 head — the moment the stream exists. Any other head (the
// SDK's 409 on a second GET of an open stream, a 400 on the GET path) suppresses keep-alive forever:
// a comment there would be a body byte on a response that is not a stream.
func (k *streamKeepAliveWriter) WriteHeader(code int) {
	if code == http.StatusOK {
		k.start()
	} else {
		k.mu.Lock()
		k.non200 = true
		k.mu.Unlock()
	}
	k.ResponseWriter.WriteHeader(code)
}

// Write serializes the SDK's frame writes with the ticker's comments, so the two never interleave.
// An unheaded Write commits an implicit 200, which is a stream opening too.
func (k *streamKeepAliveWriter) Write(p []byte) (int, error) {
	k.start()
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.ResponseWriter.Write(p)
}

// start launches the ticker goroutine, at most once, and never for a response that is not a stream
// or after the handler has stopped it. An interval of zero or less disables keep-alive.
func (k *streamKeepAliveWriter) start() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.started || k.stopped || k.non200 || k.interval <= 0 {
		return
	}
	k.started = true
	go k.tick()
}

func (k *streamKeepAliveWriter) tick() {
	t := time.NewTicker(k.interval)
	defer t.Stop()
	for {
		select {
		case <-k.done:
			return
		case <-t.C:
			if !k.writeComment() {
				return
			}
		}
	}
}

// writeComment emits one keep-alive frame under the write lock. It reports whether the ticker should
// keep running: false once the stream has been stopped, so the goroutine never touches a
// ResponseWriter whose handler has returned (net/http forbids that write).
func (k *streamKeepAliveWriter) writeComment() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stopped {
		return false
	}
	if _, err := io.WriteString(k.ResponseWriter, keepAliveComment); err != nil {
		return false
	}
	k.flushLocked() // best effort, like every other flush here
	return true
}

// stop ends the ticker. Call it when the stream handler returns; it is safe to call twice and safe
// to call on a stream that never opened.
func (k *streamKeepAliveWriter) stop() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stopped {
		return
	}
	k.stopped = true
	close(k.done)
}

// Flush and Unwrap keep the wrapper transparent, so the SDK's http.ResponseController still reaches
// the real writer. Flush takes the write lock: net/http's flush pushes the same buffered writer a
// keep-alive comment writes to, so the two must never run at once.
func (k *streamKeepAliveWriter) Flush() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.flushLocked()
}

func (k *streamKeepAliveWriter) flushLocked() {
	_ = http.NewResponseController(k.ResponseWriter).Flush()
}

func (k *streamKeepAliveWriter) Unwrap() http.ResponseWriter { return k.ResponseWriter }

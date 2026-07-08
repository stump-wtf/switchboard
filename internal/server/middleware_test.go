package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSecureHeaders(t *testing.T) {
	h := secureHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	want := map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") ||
		!strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP missing expected directives: %q", csp)
	}
}

func TestMaxBytesRejectsOversizedBody(t *testing.T) {
	// A handler that reads the whole body and reports a 413 on the MaxBytesReader overflow.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	h := maxBytes(16)(inner)

	// Under the cap: OK.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("short")))
	if rec.Code != http.StatusOK {
		t.Fatalf("small body: got %d, want 200", rec.Code)
	}
	// Over the cap: 413.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 64))))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: got %d, want 413", rec.Code)
	}
}

func TestRateLimiterTokenBucket(t *testing.T) {
	rl := newRateLimiter(1, 3) // 1 req/s, burst 3
	base := time.Unix(1_000_000, 0)

	// Burst of 3 allowed instantly, 4th denied.
	for i := 0; i < 3; i++ {
		if !rl.allowAt("1.2.3.4", base) {
			t.Fatalf("burst token %d should be allowed", i+1)
		}
	}
	if rl.allowAt("1.2.3.4", base) {
		t.Fatal("4th request in the same instant should be denied")
	}
	// A different IP has its own bucket.
	if !rl.allowAt("5.6.7.8", base) {
		t.Fatal("distinct IP should not share a bucket")
	}
	// One second later, ~1 token has refilled.
	if !rl.allowAt("1.2.3.4", base.Add(time.Second)) {
		t.Fatal("a token should refill after one second")
	}
	if rl.allowAt("1.2.3.4", base.Add(time.Second)) {
		t.Fatal("only one token should have refilled")
	}
}

func TestRateLimiterMiddleware429(t *testing.T) {
	rl := newRateLimiter(1, 1)
	h := rl.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "9.9.9.9:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: got %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 should carry a Retry-After header")
	}
}

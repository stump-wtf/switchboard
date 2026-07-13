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
	// Governing: SPEC-0012 REQ "Security Headers" (#186): base-uri 'none' (no page uses <base>;
	// an injected one would rebase every relative URL) and an EXPLICIT same-origin connect-src
	// (permits exactly the /events SSE stream and HTMX fetches, resists default-src drift).
	csp := rec.Header().Get("Content-Security-Policy")
	for _, directive := range []string{
		"default-src 'self'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"connect-src 'self'",
		"form-action 'self'",
		"script-src 'self'",
	} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP missing %q: %q", directive, csp)
		}
	}
	if strings.Contains(csp, "base-uri 'self'") {
		t.Errorf("CSP must pin base-uri 'none', not 'self': %q", csp)
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

// Governing: SPEC-0013 "Rate Limiting" (#186) — the shared human-surface limiter meters ONLY
// state-changing POSTs; reads (including the long-lived SSE GET, which carries its own per-session
// stream cap) must pass untouched even when the mutation bucket is empty.
func TestPostMiddlewareThrottlesPostsOnly(t *testing.T) {
	rl := newRateLimiter(1, 2) // burst 2
	h := rl.postMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/todos/td_1/claim", nil)
		req.RemoteAddr = "9.9.9.9:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for i := 0; i < 2; i++ {
		if rec := post(); rec.Code != http.StatusOK {
			t.Fatalf("POST %d within burst: got %d, want 200", i+1, rec.Code)
		}
	}
	rec := post()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("POST over burst: got %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 should carry a Retry-After header")
	}
	// The bucket is empty, but GETs (page loads, HTMX panel swaps, the SSE stream) stay exempt.
	for _, path := range []string{"/", "/events", "/todos"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "9.9.9.9:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s with an empty bucket: got %d, want 200 (reads are exempt)", path, rec.Code)
		}
	}
	// A different principal (distinct fallback key here; distinct human id in production) still
	// has its own tokens — one operator's burst cannot starve another.
	req := httptest.NewRequest(http.MethodPost, "/todos/td_2/claim", nil)
	req.RemoteAddr = "8.8.8.8:4321"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("distinct principal should not share a bucket: got %d", rec.Code)
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

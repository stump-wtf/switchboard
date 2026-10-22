// Inbound webhook throttle configuration (#586): the per-IP bucket on the ingest surface must be
// built from the operator's settings, not a hardcoded 10/20, because senders that never retry a 429
// (Gitea deliveries among them) permanently lose every event the bucket rejects. The router test
// drives the REAL route table so the limiter mounted on the /webhooks/w/{token} group is the one
// under test.
// Governing: SPEC-0001 REQ "Rate Limiting".
package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/stump-wtf/switchboard/internal/a2a"
	"github.com/stump-wtf/switchboard/internal/auth"
	"github.com/stump-wtf/switchboard/internal/config"
	"github.com/stump-wtf/switchboard/internal/ingest"
	mcpsrv "github.com/stump-wtf/switchboard/internal/mcp"
	"github.com/stump-wtf/switchboard/internal/oauthsrv"
	"github.com/stump-wtf/switchboard/internal/store"
	"github.com/stump-wtf/switchboard/internal/web"
)

func newWebhookRLTestRouter(t *testing.T, cfg config.Config) chi.Router {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg.BaseURL = "https://sb.example.com"
	st := store.New(nil)
	authr, err := auth.New(context.Background(), cfg, st, log)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	webh, err := web.New(st, cfg, log)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	mcph := mcpsrv.New(st, log)
	t.Cleanup(mcph.Close)
	return newRouter(routerDeps{
		st:    st,
		cfg:   cfg,
		authr: authr,
		webh:  webh,
		ing:   ingest.New(st, ingest.NewHub(), log, ingest.Config{}),
		mcp:   mcph,
		a2a:   a2a.New(st, log),
		oauth: oauthsrv.New(st, cfg.BaseURL, log),
		ping:  func(context.Context) error { return nil },
		log:   log,
	})
}

// postWebhook delivers one body to an unknown self-managed token from a fixed client IP. Below the
// bucket's burst the request reaches the ingest handler (an unknown token 404s or, with no DB,
// errors); the point is only that it was NOT a 429 — a 429 never reaches the handler at all, so
// nothing is persisted and a non-retrying sender loses the event.
func postWebhook(t *testing.T, r http.Handler) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/w/0123456789abcdef0123456789abcdef", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code
}

func TestWebhookRateLimitIsConfigurable(t *testing.T) {
	// A raised burst must be honored: 25 deliveries in one instant from one IP pass a 40-bucket,
	// while the old hardcoded burst of 20 would have 429'd the 21st and lost it. The refill rate is
	// a token per second, so bucket arithmetic alone decides the loop — no clock races.
	r := newWebhookRLTestRouter(t, config.Config{WebhookRateRPS: 1, WebhookBurst: 40})
	for i := 0; i < 25; i++ {
		if code := postWebhook(t, r); code == http.StatusTooManyRequests {
			t.Fatalf("delivery %d: 429 with WebhookBurst=40; the configured burst is not mounted on the webhook route", i+1)
		}
	}
}

func TestWebhookRateLimitRejectsAboveConfiguredBurst(t *testing.T) {
	// The other half of the setting: throttling still bounds abuse. Burst 3 from one IP: the first
	// three deliveries proceed, the next are 429 with a Retry-After.
	r := newWebhookRLTestRouter(t, config.Config{WebhookRateRPS: 1, WebhookBurst: 3})
	for i := 0; i < 3; i++ {
		if code := postWebhook(t, r); code == http.StatusTooManyRequests {
			t.Fatalf("delivery %d within burst 3: got 429", i+1)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/w/0123456789abcdef0123456789abcdef", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("delivery 4 above burst 3: got %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 carries no Retry-After")
	}
}

// TestWebhookRateLimitDefaults: a Config built without the settings (the FromEnv defaults, and
// route-table harnesses) keeps the historical 10 rps / burst 20 — 20 instant deliveries from one
// IP all reach the handler.
func TestWebhookRateLimitDefaults(t *testing.T) {
	r := newWebhookRLTestRouter(t, config.Config{})
	for i := 0; i < 20; i++ {
		if code := postWebhook(t, r); code == http.StatusTooManyRequests {
			t.Fatalf("delivery %d within the default burst 20: got 429", i+1)
		}
	}
}

// Throttled delivery reject visibility (#612, ADR-0051): the 429 a non-retrying sender loses to the
// inbound webhook throttle is counted where it happens — the limiter has to say so, because no
// receiver ever runs. The router wires that hook, so the test drives the REAL route table and reads
// the counter back off the registry the card reads.
//
// Attribution to a specific webhook needs the token to have resolved at least once (the pairing the
// receiver records), which needs a database; the overflow attribution is what is provable DB-free,
// and internal/ingest/rejects_test.go pins the resolved case.

package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/a2a"
	"github.com/stump-wtf/switchboard/internal/auth"
	"github.com/stump-wtf/switchboard/internal/config"
	"github.com/stump-wtf/switchboard/internal/ingest"
	mcpsrv "github.com/stump-wtf/switchboard/internal/mcp"
	"github.com/stump-wtf/switchboard/internal/metrics"
	"github.com/stump-wtf/switchboard/internal/oauthsrv"
	"github.com/stump-wtf/switchboard/internal/store"
	"github.com/stump-wtf/switchboard/internal/web"
)

func TestThrottledWebhookDeliveryIsCounted(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{BaseURL: "https://sb.example.com", WebhookRateRPS: 1, WebhookBurst: 1}
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
	ing := ingest.New(st, ingest.NewHub(), log, ingest.Config{})
	mtr := metrics.New(metrics.Options{Log: log})
	ing.SetMetrics(mtr)

	r := newRouter(routerDeps{
		st: st, cfg: cfg, authr: authr, webh: webh, ing: ing, mcp: mcph,
		a2a: a2a.New(st, log), oauth: oauthsrv.New(st, cfg.BaseURL, log),
		ping: func(context.Context) error { return nil }, log: log,
	})

	post := func() int {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/w/0123456789abcdef0123456789abcdef",
			strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post(); code == http.StatusTooManyRequests {
		t.Fatalf("first delivery 429d with burst 1; nothing to count")
	}
	if code := post(); code != http.StatusTooManyRequests {
		t.Fatalf("second delivery = %d, want 429 from a drained bucket", code)
	}

	if got := preIntakeRejectSeries(mtr, metrics.RejectReasonRateLimited); got != 1 {
		t.Errorf("rate-limited rejects = %v, want 1 counted under the overflow bucket "+
			"(this token never resolved, so no webhook can be named for it)", got)
	}
}

// preIntakeRejectSeries sums the pre-intake reject series carrying the given reason.
func preIntakeRejectSeries(mtr *metrics.Metrics, reason string) float64 {
	var total float64
	mfs, err := mtr.Registry().Gather()
	if err != nil {
		return -1
	}
	for _, mf := range mfs {
		if mf.GetName() != "switchboard_webhook_preintake_rejects_total" {
			continue
		}
		for _, mt := range mf.GetMetric() {
			for _, lp := range mt.GetLabel() {
				if lp.GetName() == "reason" && lp.GetValue() == reason {
					total += mt.GetCounter().GetValue()
				}
			}
		}
	}
	return total
}

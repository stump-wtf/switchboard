package web

// DB-less coverage for the SPEC-0017 Providers helpers and handler guards that runs in the
// `go test ./...` gate: the registry-row → render-model projection (paths, secret presence,
// rotatability, queue extraction), the connectable-catalog computation, the confirm-action guard
// (unknown action 404s before any store access), and the rotate secret mint. The end-to-end
// lifecycle POSTs with session + CSRF live in the DB-backed internal/server suite.
//
// Governing: SPEC-0017 REQ "Providers View" / "Provider Catalog" / "Provider Lifecycle".

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/joestump/switchboard/internal/store"
)

func TestProviderLineFromProjection(t *testing.T) {
	cases := []struct {
		name       string
		in         store.Adapter
		wantPath   string
		wantSecret string
		wantRotate bool
		wantQueue  string
		wantKind   string
	}{
		{name: "signed configured",
			in:       store.Adapter{Name: "github", Family: "webhook", Kind: "github", TrustMode: "signed", SecretConfigured: true, Config: []byte(`{"queue":"ci"}`)},
			wantPath: "/webhooks/github", wantSecret: "configured", wantRotate: true, wantQueue: "ci", wantKind: "github"},
		{name: "token missing secret",
			in:       store.Adapter{Name: "homelab", Family: "webhook", Kind: "generic", TrustMode: "token"},
			wantPath: "/webhooks/generic/homelab", wantSecret: "missing", wantRotate: true, wantQueue: "homelab", wantKind: "generic"},
		{name: "open none-by-design",
			in:       store.Adapter{Name: "lan", Family: "webhook", Kind: "generic", TrustMode: "open"},
			wantPath: "/webhooks/generic/lan", wantSecret: "none-by-design", wantRotate: false, wantQueue: "lan", wantKind: "generic"},
		{name: "queue stream config, pre-registry kind falls back to name",
			in:       store.Adapter{Name: "redis-builds", Family: "queue", TrustMode: "queue", Config: []byte(`{"stream":"builds"}`)},
			wantPath: "", wantSecret: "none-by-design", wantRotate: false, wantQueue: "builds", wantKind: "redis-builds"},
	}
	for _, tc := range cases {
		l := providerLineFrom(tc.in, nil)
		if l.Path != tc.wantPath || l.SecretStatus != tc.wantSecret || l.CanRotate != tc.wantRotate ||
			l.Queue != tc.wantQueue || l.Kind != tc.wantKind {
			t.Errorf("%s: got path=%q secret=%q rotate=%v queue=%q kind=%q, want %q/%q/%v/%q/%q",
				tc.name, l.Path, l.SecretStatus, l.CanRotate, l.Queue, l.Kind,
				tc.wantPath, tc.wantSecret, tc.wantRotate, tc.wantQueue, tc.wantKind)
		}
		if l.RowID != "sb-pr-"+tc.in.Name {
			t.Errorf("%s: row id %q", tc.name, l.RowID)
		}
	}
}

func TestProviderLineHealthJoin(t *testing.T) {
	h := store.ProviderHealth{EventsPerMin: 7}
	l := providerLineFrom(store.Adapter{Name: "github", Family: "webhook", Kind: "github", TrustMode: "signed"},
		map[string]store.ProviderHealth{"github": h})
	if l.InRate != 7 {
		t.Errorf("in-rate join: got %d, want 7", l.InRate)
	}
	if l.LastSeenAt != nil {
		t.Error("no last-seen in health → line must render never-seen")
	}
}

// SPEC-0017 REQ "Provider Catalog": connected single-instance signed kinds drop off; generic and
// redis stay connectable regardless of connections; SQS/NATS/AMQP are always (and only) available.
func TestConnectableCatalogComputation(t *testing.T) {
	lines := []providerLine{
		{Name: "github", Kind: "github", Family: "webhook"},
		{Name: "homelab", Kind: "generic", Family: "webhook"},
		{Name: "redis-builds", Kind: "redis", Family: "queue"},
	}
	kinds := map[string]bool{}
	for _, c := range connectableCatalog(lines) {
		kinds[c.Kind] = true
	}
	for _, want := range []string{"stripe", "slack", "generic", "redis"} {
		if !kinds[want] {
			t.Errorf("catalog missing connectable kind %s", want)
		}
	}
	if kinds["github"] {
		t.Error("connected github must drop off the connectable catalog")
	}
	for _, c := range availableCatalog {
		if c.State != "available" {
			t.Errorf("available tier %s must carry state=available, got %q", c.Kind, c.State)
		}
	}
}

// Unknown lifecycle actions 404 BEFORE any store access (the nil-store handler proves the guard
// runs first) — only disable/rotate/remove have confirmations.
func TestProviderConfirmModalUnknownAction404(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/providers/github/confirm/explode", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "github")
	rctx.URLParams.Add("action", "explode")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.ProviderConfirmModal(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown confirm action: got %d, want 404", rec.Code)
	}
}

// The rotate mint produces a fresh 64-char hex secret every call — paste-safe, never an sbk_ MCP
// credential, never empty.
func TestMintProviderSecret(t *testing.T) {
	a, err := mintProviderSecret()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	b, err := mintProviderSecret()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if len(a) != 64 || len(b) != 64 {
		t.Fatalf("secret length: %d/%d, want 64 hex chars", len(a), len(b))
	}
	if a == b {
		t.Fatal("two mints must differ")
	}
	for _, r := range a {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			t.Fatalf("secret must be lowercase hex, got %q", a)
		}
	}
}

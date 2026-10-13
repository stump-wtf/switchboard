package web

// Landing-page docs-link resolution (#563): a self-hosted instance has no local /docs service,
// so every relative /docs link on the public landing page 404s. The landing page now emits
// absolute links under the configured docs base (SWITCHBOARD_DOCS_BASE_URL, defaulting to the
// hosted site), and a relative /docs link in the rendered page is the regression. Exercised
// through Handler.Home — the real route — so the default applies exactly as it does in the
// server.

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stump-wtf/switchboard/internal/config"
)

func renderHome(t *testing.T, cfg config.Config) string {
	t.Helper()
	h, err := New(nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	h.Home(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 {
		t.Fatalf("Home: status %d", rec.Code)
	}
	return rec.Body.String()
}

func TestLandingDocsLinksDefaultToHostedDocs(t *testing.T) {
	body := renderHome(t, config.Config{BaseURL: "https://sb.example.com"})
	if strings.Contains(body, `href="/docs`) {
		t.Fatalf("landing page still emits relative /docs links (404 on a self-hosted instance)")
	}
	for _, want := range []string{
		`href="https://switchboard.stump.wtf/docs/"`,
		`href="https://switchboard.stump.wtf/docs/decisions/ADR-0007-todos-as-core-primitive"`,
		`href="https://switchboard.stump.wtf/docs/decisions/ADR-0014-ingestion-adapters-push-pull"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("landing page missing %s", want)
		}
	}
}

func TestLandingDocsLinksHonourConfiguredBase(t *testing.T) {
	body := renderHome(t, config.Config{
		BaseURL:     "https://sb.example.com",
		DocsBaseURL: "https://docs.internal.example/docs",
	})
	if strings.Contains(body, `href="/docs`) || strings.Contains(body, "switchboard.stump.wtf/docs") {
		t.Fatalf("landing page ignored the configured docs base")
	}
	for _, want := range []string{
		`href="https://docs.internal.example/docs/"`,
		`href="https://docs.internal.example/docs/decisions/ADR-0008-human-principal-vended-endpoints"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("landing page missing %s", want)
		}
	}
}

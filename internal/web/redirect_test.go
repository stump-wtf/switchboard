package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/joestump/switchboard/internal/config"
)

func TestSafeRedirectTarget(t *testing.T) {
	h := &Handler{cfg: config.Config{BaseURL: "https://sb.example.com"}}

	cases := []struct {
		name, referer, want string
	}{
		{"absent referer falls back", "", "/"},
		{"off-origin referer falls back", "https://evil.example/steal", "/"},
		{"same-origin absolute is stripped to path", "https://sb.example.com/agents/abc", "/agents/abc"},
		{"same-origin keeps query", "https://sb.example.com/agents/abc?tab=eps", "/agents/abc?tab=eps"},
		{"relative in-app path is honored", "/agents/xyz", "/agents/xyz"},
		{"scheme-relative off-origin falls back", "//evil.example/x", "/"},
		{"non-path referer falls back", "mailto:x@y.z", "/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/endpoints/1/revoke", nil)
			if c.referer != "" {
				r.Header.Set("Referer", c.referer)
			}
			if got := h.safeRedirectTarget(r, "/"); got != c.want {
				t.Fatalf("referer %q → %q, want %q", c.referer, got, c.want)
			}
		})
	}
}

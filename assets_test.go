package switchboard

// Static-asset conformance for the charm-web design language.
// Governing: ADR-0018 (tokens + .sb-* components, vendored fonts, no Pico, split JS modules),
// SPEC-0015 REQ "Design Token System", REQ "Typography And Vendored Fonts", REQ "Theme Toggle".

import (
	"strings"
	"testing"
)

func TestStaticAssetsEmbedded(t *testing.T) {
	for _, p := range []string{
		"static/tokens.css",
		"static/switchboard.css",
		// sb.js is split into feature modules (SPEC-0015 foundation) plus the pre-paint theme boot.
		"static/js/theme-boot.js",
		"static/js/sb-live.js",
		"static/js/sb-overlay.js",
		"static/js/sb-vend.js",
		"static/js/sb-theme.js",
		"static/js/sb-keys.js",
		// The charm-web woff2 files are documented here until vendored (fonts_test.go skips loudly).
		"static/fonts/README.md",
	} {
		b, err := StaticFS.ReadFile(p)
		if err != nil {
			t.Errorf("missing embedded asset %s: %v", p, err)
			continue
		}
		if len(b) == 0 {
			t.Errorf("embedded asset %s is empty", p)
		}
	}
	// Any woff2 that IS vendored must be a real woff2 (fonts_test.go walks the cmap in depth).
	entries, err := StaticFS.ReadDir("static/fonts")
	if err != nil {
		t.Fatalf("read static/fonts: %v", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".woff2") {
			continue
		}
		b, err := StaticFS.ReadFile("static/fonts/" + e.Name())
		if err != nil || len(b) < 4 || string(b[:4]) != "wOF2" {
			t.Errorf("static/fonts/%s is not a woff2 file", e.Name())
		}
	}
}

// TestRetiredOperatorFontsRemoved: the Zilla Slab / IBM Plex families leave with ADR-0016
// (SPEC-0015 REQ "Typography And Vendored Fonts").
func TestRetiredOperatorFontsRemoved(t *testing.T) {
	entries, err := StaticFS.ReadDir("static/fonts")
	if err != nil {
		t.Fatalf("read static/fonts: %v", err)
	}
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		if strings.Contains(name, "zilla") || strings.Contains(name, "ibm-plex") {
			t.Errorf("retired Operator font file still vendored: static/fonts/%s", e.Name())
		}
	}
	for _, css := range []string{"static/tokens.css", "static/switchboard.css"} {
		b, err := StaticFS.ReadFile(css)
		if err != nil {
			t.Fatalf("read %s: %v", css, err)
		}
		for _, family := range []string{"Zilla Slab", "IBM Plex"} {
			if strings.Contains(string(b), family) {
				t.Errorf("%s still references the retired %q family", css, family)
			}
		}
	}
}

func TestTokensArePureAndPicoFree(t *testing.T) {
	tokens, err := StaticFS.ReadFile("static/tokens.css")
	if err != nil {
		t.Fatalf("read tokens.css: %v", err)
	}
	css := string(tokens)
	if strings.Contains(css, "--pico-") {
		t.Error("tokens.css still contains --pico-* variables (ADR-0016 deletes the Pico block)")
	}
	for _, tok := range []string{
		"--sb-trust-signed", "--sb-trust-token", "--sb-trust-open", "--sb-trust-queue",
		"--sb-canvas", "--sb-primary", "--sb-accent",
		"--sb-font-mono", "--sb-font-display",
		"JetBrains Mono", "Space Mono",
		"prefers-color-scheme: dark", `[data-theme="night"]`, `[data-theme="day"]`,
		"font-display: swap",
	} {
		if !strings.Contains(css, tok) {
			t.Errorf("tokens.css missing %q", tok)
		}
	}
	// The brass-era brand tokens are retired wholesale (ADR-0018 supersedes ADR-0016).
	for _, stale := range []string{"--sb-oxblood", "--sb-brass", "--sb-copper", "--sb-font-sans"} {
		if strings.Contains(css, stale) {
			t.Errorf("tokens.css still defines retired Operator token %q", stale)
		}
	}
	if strings.Contains(css, "http://") || strings.Contains(css, "https://") {
		t.Error("tokens.css references an external origin")
	}

	comp, err := StaticFS.ReadFile("static/switchboard.css")
	if err != nil {
		t.Fatalf("read switchboard.css: %v", err)
	}
	if strings.Contains(string(comp), "http://") || strings.Contains(string(comp), "https://") {
		t.Error("switchboard.css references an external origin")
	}
	if !strings.Contains(string(comp), ".sb-") {
		t.Error("switchboard.css missing the .sb-* component layer")
	}
}

// TestThemeBootIsExternalAndTiny: the pre-paint theme set ships as a small EXTERNAL script (CSP
// script-src 'self', no inline JS) that only reads localStorage and stamps <html data-theme>.
// Governing: SPEC-0015 REQ "Theme Toggle" (no-FOUC boot compatible with the same-origin CSP).
func TestThemeBootIsExternalAndTiny(t *testing.T) {
	b, err := StaticFS.ReadFile("static/js/theme-boot.js")
	if err != nil {
		t.Fatalf("read theme-boot.js: %v", err)
	}
	js := string(b)
	if len(js) > 2048 {
		t.Errorf("theme-boot.js is %d bytes — it must stay tiny (it blocks first paint)", len(js))
	}
	for _, want := range []string{`localStorage.getItem("sb-theme")`, "data-theme"} {
		if !strings.Contains(js, want) {
			t.Errorf("theme-boot.js missing %q", want)
		}
	}
}

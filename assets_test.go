package switchboard

// Static-asset conformance for the Operator design language.
// Governing: ADR-0016 (tokens + .sb-* components, vendored fonts, no Pico),
// SPEC-0013 REQ "Design Language Conformance".

import (
	"strings"
	"testing"
)

func TestStaticAssetsEmbedded(t *testing.T) {
	for _, p := range []string{
		"static/tokens.css",
		"static/switchboard.css",
		"static/fonts/zilla-slab-500.woff2",
		"static/fonts/zilla-slab-600.woff2",
		"static/fonts/zilla-slab-700.woff2",
		"static/fonts/ibm-plex-sans-var.woff2",
		"static/fonts/ibm-plex-mono-400.woff2",
		"static/fonts/ibm-plex-mono-500.woff2",
		"static/fonts/ibm-plex-mono-600.woff2",
		"static/fonts/OFL-zilla-slab.txt",
		"static/fonts/OFL-ibm-plex-sans.txt",
		"static/fonts/OFL-ibm-plex-mono.txt",
	} {
		b, err := StaticFS.ReadFile(p)
		if err != nil {
			t.Errorf("missing embedded asset %s: %v", p, err)
			continue
		}
		if len(b) == 0 {
			t.Errorf("embedded asset %s is empty", p)
		}
		if strings.HasSuffix(p, ".woff2") && !strings.HasPrefix(string(b[:4]), "wOF2") {
			t.Errorf("%s is not a woff2 file", p)
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
		"--sb-canvas", "--sb-oxblood", "--sb-brass",
		"prefers-color-scheme: dark", `[data-theme="dark"]`,
		"font-display: swap",
	} {
		if !strings.Contains(css, tok) {
			t.Errorf("tokens.css missing %q", tok)
		}
	}
	for _, stale := range []string{"--sb-trust-unverified", "--sb-trust-redis", "--sb-trust-rejected"} {
		if strings.Contains(css, stale) {
			t.Errorf("tokens.css still defines renamed trust token %q", stale)
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

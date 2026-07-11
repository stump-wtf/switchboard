package switchboard

// WCAG 2.1 AA contrast assertions on the design tokens, in both themes. tokens.css is the single
// source of color truth (ADR-0016), so the ratios are asserted on the custom-property values
// themselves: trust badge text/background pairs, todo status text/background pairs, and
// body-text/surface pairs must hold ≥ 4.5:1 in operator-cream (light) and bakelite (dark) alike.
// Governing: ADR-0016 (Operator design language), SPEC-0013 REQ "Design Language Conformance".

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"
)

// contrastPairs are the (foreground, background) token pairs asserted at AA (4.5:1) in both themes.
var contrastPairs = [][2]string{
	// trust badges: text on tinted background (ADR-0003 trust modes)
	{"--sb-trust-signed", "--sb-trust-signed-bg"},
	{"--sb-trust-token", "--sb-trust-token-bg"},
	{"--sb-trust-open", "--sb-trust-open-bg"},
	{"--sb-trust-queue", "--sb-trust-queue-bg"},
	// todo status badges: text on tinted background (SPEC-0013 status colors)
	{"--sb-status-pending", "--sb-status-pending-bg"},
	{"--sb-status-claimed", "--sb-status-claimed-bg"},
	{"--sb-status-done", "--sb-status-done-bg"},
	{"--sb-status-failed", "--sb-status-failed-bg"},
	// body text on the two primary surfaces
	{"--sb-ink", "--sb-canvas"},
	{"--sb-text", "--sb-canvas"},
	{"--sb-body", "--sb-canvas"},
	{"--sb-ink", "--sb-panel"},
	{"--sb-text", "--sb-panel"},
	{"--sb-body", "--sb-panel"},
}

func TestTokenContrastMeetsAA(t *testing.T) {
	css := tokensCSS(t)
	themes := map[string]map[string]string{
		"light (operator-cream)": parseCustomProps(t, css, ":root {"),
		"dark (bakelite)":        parseCustomProps(t, css, `[data-theme="dark"] {`),
	}
	for theme, props := range themes {
		for _, pair := range contrastPairs {
			fg, bg := pair[0], pair[1]
			ratio := contrastRatio(t, theme, props, fg, bg)
			if ratio < 4.5 {
				t.Errorf("%s: %s on %s = %.2f:1, below WCAG 2.1 AA (4.5:1)", theme, fg, bg, ratio)
			}
		}
	}
}

// TestDarkThemeBlocksAgree pins the two dark-theme delivery mechanisms (prefers-color-scheme media
// query and the explicit data-theme attribute) to identical values, so a token edited in one block
// but not the other fails here instead of shipping a split-brain theme.
func TestDarkThemeBlocksAgree(t *testing.T) {
	css := tokensCSS(t)
	media := parseCustomProps(t, css, `:root:not([data-theme="light"]) {`)
	attr := parseCustomProps(t, css, `[data-theme="dark"] {`)
	if len(media) == 0 {
		t.Fatal("tokens.css missing the prefers-color-scheme dark block")
	}
	for k, v := range media {
		if av, ok := attr[k]; !ok {
			t.Errorf("dark token %s set in the media-query block but missing from [data-theme=\"dark\"]", k)
		} else if av != v {
			t.Errorf("dark token %s differs between blocks: media=%q attr=%q", k, v, av)
		}
	}
	for k := range attr {
		if _, ok := media[k]; !ok {
			t.Errorf("dark token %s set in [data-theme=\"dark\"] but missing from the media-query block", k)
		}
	}
}

// TestRailCollapseBreakpoint asserts the nav rail's icons-only collapse (SPEC-0013: rail collapses
// below 820px) survives in the component layer.
func TestRailCollapseBreakpoint(t *testing.T) {
	b, err := StaticFS.ReadFile("static/switchboard.css")
	if err != nil {
		t.Fatalf("read switchboard.css: %v", err)
	}
	css := string(b)
	idx := strings.Index(css, "@media (max-width: 820px)")
	if idx < 0 {
		t.Fatal("switchboard.css missing the 820px rail-collapse media query")
	}
	if !strings.Contains(css[idx:], ".sb-rail") {
		t.Error("rail-collapse media query does not restyle .sb-rail")
	}
}

func tokensCSS(t *testing.T) string {
	t.Helper()
	b, err := StaticFS.ReadFile("static/tokens.css")
	if err != nil {
		t.Fatalf("read tokens.css: %v", err)
	}
	return string(b)
}

var customPropRe = regexp.MustCompile(`(--[a-z0-9-]+)\s*:\s*([^;]+);`)

// parseCustomProps returns the custom properties declared in the block that starts at the first
// occurrence of the given selector prefix and runs to its matching closing brace.
func parseCustomProps(t *testing.T, css, selector string) map[string]string {
	t.Helper()
	start := strings.Index(css, selector)
	if start < 0 {
		t.Fatalf("tokens.css missing selector %q", selector)
	}
	body := css[start+len(selector):]
	depth := 1
	end := -1
	for i, r := range body {
		switch r {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		t.Fatalf("unbalanced braces after selector %q", selector)
	}
	props := map[string]string{}
	for _, m := range customPropRe.FindAllStringSubmatch(body[:end], -1) {
		props[m[1]] = strings.TrimSpace(m[2])
	}
	if len(props) == 0 {
		t.Fatalf("no custom properties found under %q", selector)
	}
	return props
}

// contrastRatio computes the WCAG 2.1 contrast ratio between two hex-color tokens.
func contrastRatio(t *testing.T, theme string, props map[string]string, fg, bg string) float64 {
	t.Helper()
	lf := relativeLuminance(t, theme, props, fg)
	lb := relativeLuminance(t, theme, props, bg)
	hi, lo := math.Max(lf, lb), math.Min(lf, lb)
	return (hi + 0.05) / (lo + 0.05)
}

// relativeLuminance implements WCAG 2.1 §relative luminance for an sRGB hex token.
func relativeLuminance(t *testing.T, theme string, props map[string]string, name string) float64 {
	t.Helper()
	val, ok := props[name]
	if !ok {
		t.Fatalf("%s: token %s not defined", theme, name)
	}
	r, g, b, err := parseHexColor(val)
	if err != nil {
		t.Fatalf("%s: token %s = %q: %v", theme, name, val, err)
	}
	lin := func(c float64) float64 {
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

func parseHexColor(s string) (r, g, b float64, err error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "#") || len(s) != 7 {
		return 0, 0, 0, fmt.Errorf("not a 6-digit hex color")
	}
	var ri, gi, bi int
	if _, err := fmt.Sscanf(s, "#%02x%02x%02x", &ri, &gi, &bi); err != nil {
		return 0, 0, 0, err
	}
	return float64(ri) / 255, float64(gi) / 255, float64(bi) / 255, nil
}

// Vended-credential wiring tests: the one-time reveal MUST emit Streamable-HTTP .mcp.json wiring
// (type: http + /mcp/{slug} URL + bearer header) and MUST NOT reference a local binary or stdio
// command. These are DB-less unit tests, so they run under plain `go test ./...`.
// Governing: SPEC-0014 REQ "HTTP Wiring Is the Only Wiring" (scenario "Vend reveal shows HTTP
// wiring"); SPEC-0012 REQ "Vend Flow and One-Time Credential Reveal".
package web

import (
	"encoding/json"
	"html"
	"strings"
	"testing"
)

// legacyStdioMarkers are substrings that would betray the retired stdio/binary transport. None may
// appear in the HTTP wiring block or the vended screen.
var legacyStdioMarkers = []string{
	`"command"`, `"args"`, "SWITCHBOARD_TOKEN", "SWITCHBOARD_URL",
	"dangerously-load-development-channels", "on your PATH", "switchboard channel", "server:switchboard",
}

func TestBuildMCPJSONEmitsHTTPWiring(t *testing.T) {
	out := buildMCPJSON("https://sb.example.com", "reviewer-bot-ab12cd", "sbk_secret123")

	for _, want := range []string{
		`"type": "http"`,
		`"url": "https://sb.example.com/mcp/reviewer-bot-ab12cd"`,
		`"Authorization": "Bearer sbk_secret123"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("wiring missing %q:\n%s", want, out)
		}
	}
	for _, bad := range legacyStdioMarkers {
		if strings.Contains(out, bad) {
			t.Errorf("wiring leaks retired stdio marker %q:\n%s", bad, out)
		}
	}

	// The block must be valid JSON shaped exactly as an MCP client expects.
	var parsed struct {
		MCPServers struct {
			Switchboard struct {
				Type    string            `json:"type"`
				URL     string            `json:"url"`
				Headers map[string]string `json:"headers"`
			} `json:"switchboard"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("wiring is not valid JSON: %v\n%s", err, out)
	}
	sb := parsed.MCPServers.Switchboard
	if sb.Type != "http" {
		t.Errorf("type = %q, want http", sb.Type)
	}
	if sb.URL != "https://sb.example.com/mcp/reviewer-bot-ab12cd" {
		t.Errorf("url = %q", sb.URL)
	}
	if got := sb.Headers["Authorization"]; got != "Bearer sbk_secret123" {
		t.Errorf("Authorization header = %q", got)
	}
}

// A trailing slash on the configured base URL must not double up before /mcp/.
func TestBuildMCPJSONNormalizesBaseURL(t *testing.T) {
	out := buildMCPJSON("https://sb.example.com/", "slug-1", "sbk_x")
	if !strings.Contains(out, `"url": "https://sb.example.com/mcp/slug-1"`) {
		t.Errorf("trailing slash not normalized:\n%s", out)
	}
	if strings.Contains(out, "/mcp//") {
		t.Errorf("doubled slash in URL:\n%s", out)
	}
}

// The one-time reveal fragment shows the HTTP wiring, states the immutable-scope contract, and
// carries none of the retired binary/stdio instructions.
func TestVendRevealShowsHTTPWiringOnly(t *testing.T) {
	h := newTestHandler(t)
	mcpjson := buildMCPJSON("https://sb.example.com", "reviewer-bot-ab12cd", "sbk_secret")

	// html/template escapes the JSON block's quotes; unescape so the wiring assertions read the
	// literal .mcp.json a human would copy off the reveal.
	body := html.UnescapeString(renderFrag(t, h, "vend_reveal", revealView{AgentName: "reviewer-bot",
		Slug: "reviewer-bot-ab12cd", Token: "sbk_secret", MCPJSON: mcpjson,
		Queues: []string{"reviews"}, Verbs: []string{"claim"}, CSRF: "tok"}))

	for _, want := range []string{
		"sbk_secret",               // the one-time plaintext reveal
		"it is shown once",         // one-time-reveal warning (docs/design/05-voice.md copy)
		"Credential · shown once",  // the credential field's standalone label (design canvas)
		`"type": "http"`,           // HTTP wiring in the pasted block
		"/mcp/reviewer-bot-ab12cd", // the minted endpoint URL/path
		"Bearer sbk_secret",        // bearer credential
	} {
		if !strings.Contains(body, want) {
			t.Errorf("vend reveal missing %q", want)
		}
	}
	for _, bad := range legacyStdioMarkers {
		if strings.Contains(body, bad) {
			t.Errorf("vend reveal leaks retired stdio marker %q", bad)
		}
	}
}

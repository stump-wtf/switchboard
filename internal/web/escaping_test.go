package web

// Output-encoding conformance for SPEC-0012 REQ "Server-Rendered Pages from Embedded Templates"
// ("Template output MUST rely on html/template's contextual auto-escaping; user-supplied values
// MUST NOT be emitted via any mechanism that bypasses that escaping") and REQ "Semantic HTML and
// Classless Styling" (html declares lang + responsive viewport; carried forward under SPEC-0013).

import (
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joestump/switchboard/internal/store"
)

func TestDocumentDeclaresLangAndViewport(t *testing.T) {
	h := newTestHandler(t)
	pages := map[string]view{
		"login":     {Title: "Log in"},
		"board":     {Title: "The Board", Human: testHuman(), Shell: shell{Active: "board"}},
		"endpoints": {Title: "Endpoints", Human: testHuman(), Shell: shell{Active: "endpoints"}, VerbOptions: drainVerbs},
	}
	for page, v := range pages {
		body := renderPage(t, h, page, v)
		if !strings.Contains(body, `<html lang="en">`) {
			t.Errorf("%s: <html> must declare lang", page)
		}
		if !strings.Contains(body, `name="viewport"`) || !strings.Contains(body, "width=device-width") {
			t.Errorf("%s: document must declare a responsive viewport", page)
		}
		if !strings.Contains(body, "<!doctype html>") {
			t.Errorf("%s: missing doctype", page)
		}
	}
}

// TestHostileValuesAreEscaped renders pages with attacker-shaped values in every user-supplied
// slot (display name, agent name/description, endpoint scopes, one-time token) and asserts the
// payload never survives to executable markup.
func TestHostileValuesAreEscaped(t *testing.T) {
	const payload = `"><script>alert(1)</script>`
	h := newTestHandler(t)
	human := &store.Human{ID: "h1", DisplayName: payload, Email: "x@example.com"}
	card := endpointCard{ID: "e1", AgentName: payload, PersonaName: payload, CredPrefix: payload,
		Queues: []string{payload}, Verbs: []string{payload}, State: "active"}
	sh := shell{Active: "endpoints", DBConnected: true, Initials: `"><i>`}

	bodies := map[string]string{
		"board": renderPage(t, h, "board", view{Title: "The Board", Human: human, CSRF: "tok",
			Shell: shell{Active: "board", DBConnected: true, Initials: "JS"},
			Tiles: tilesView{Stats: store.BoardStats{TodosToday: 1}, Bars: activityBars([]int{1, 2})},
			Rows: []feedRow{feedRowFromEvent(store.EventSummary{
				ID: 1, Source: payload, EventType: payload, TrustMode: "signed", ReceivedAt: time.Now(),
			}, false)},
		}),
		"endpoints": renderPage(t, h, "endpoints", view{Title: "Endpoints", Human: human, CSRF: "tok", Shell: sh,
			EndpointCards: []endpointCard{card}, PersonasEnabled: true, VerbOptions: []string{payload}}),
		"reveal": renderFrag(t, h, "vend_reveal", revealView{AgentName: payload, Slug: payload,
			Token: `</pre><script>steal()</script>`, MCPJSON: `{"x":"</pre><script>steal()</script>"}`,
			Queues: []string{payload}, Verbs: []string{payload}, CSRF: "tok"}),
	}
	for page, body := range bodies {
		if strings.Contains(body, "<script>alert(1)</script>") || strings.Contains(body, "<script>steal()</script>") {
			t.Errorf("%s: hostile value rendered unescaped", page)
		}
		// The payload must still be present, encoded — proof the value flowed through escaping
		// rather than being dropped.
		if !strings.Contains(body, "&lt;script&gt;") && !strings.Contains(body, "%3Cscript%3E") {
			t.Errorf("%s: expected an escaped form of the hostile value in output", page)
		}
	}
}

// TestNoEscapeBypassInPackageSource statically verifies no code in this package converts
// user-supplied values into html/template's trusted types (template.HTML, template.JS,
// template.URL, template.CSS, template.HTMLAttr, template.JSStr, template.Srcset), which would
// bypass contextual auto-escaping. template.HTMLEscapeString is fine — it escapes.
func TestNoEscapeBypassInPackageSource(t *testing.T) {
	banned := []string{
		"template.HTML(", "template.JS(", "template.URL(", "template.CSS(",
		"template.HTMLAttr(", "template.JSStr(", "template.Srcset(",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		// Skip this checker itself — its banned-pattern string literals are the needles, not usages.
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || e.Name() == "escaping_test.go" {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		// Strip comments so a mention in prose (like this test's own doc comment) can't trip the check.
		f, err := parser.ParseFile(fset, e.Name(), src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		f.Comments = nil
		// The banned-pattern scan matches the qualified name "template.X(", so an aliased or
		// dot import of html/template would dodge it. Require the plain import spelling.
		for _, imp := range f.Imports {
			if imp.Path.Value == `"html/template"` && imp.Name != nil {
				t.Errorf("%s: imports html/template as %q — aliasing defeats this package's escape-bypass check", e.Name(), imp.Name.Name)
			}
		}
		var b strings.Builder
		if err := printer.Fprint(&b, fset, f); err != nil {
			t.Fatalf("print %s: %v", e.Name(), err)
		}
		code := b.String()
		for _, bad := range banned {
			if strings.Contains(code, bad) {
				t.Errorf("%s: uses %q — bypasses html/template contextual auto-escaping (SPEC-0012)", e.Name(), bad)
			}
		}
	}
}

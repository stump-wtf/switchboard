package web

// Startup template-parse guarantees for SPEC-0012 REQ "Server-Rendered Pages from Embedded
// Templates": every page parses from the embedded FS, and any broken or missing template surfaces
// an error from New/parsePages so server startup fails instead of serving broken pages
// (server.Run returns the error; main exits non-zero).

import (
	"strings"
	"testing"
	"testing/fstest"
)

// validPageFS returns a minimal in-memory template FS covering the full page set, including a
// per-view fragment file each page set parses alongside the layout (SPEC-0015 REQ "Live Fragment
// Architecture": templates/fragments/*.html, one file per view).
func validPageFS() fstest.MapFS {
	fsys := fstest.MapFS{
		"templates/layout.html":          {Data: []byte(`{{define "layout"}}<html>{{template "content" .}}</html>{{end}}`)},
		"templates/fragments/board.html": {Data: []byte(`{{define "feed_row"}}{{end}}`)},
	}
	for _, p := range pageNames {
		fsys["templates/"+p+".html"] = &fstest.MapFile{Data: []byte(`{{define "content"}}` + p + `{{end}}`)}
	}
	return fsys
}

func TestParsePagesEmbeddedFS(t *testing.T) {
	// The real embedded FS must parse every page the spec ships: login, the SPEC-0013 board/todos
	// views, and the Endpoints view (which folded in the retired dashboard/agent/vended screens).
	pages, err := parsePages(tmplFS)
	if err != nil {
		t.Fatalf("parsePages(embedded): %v", err)
	}
	for _, p := range []string{"login", "board", "todos", "endpoints"} {
		if pages[p] == nil {
			t.Errorf("embedded FS missing parsed page %q", p)
		}
	}
}

func TestParsePagesFailsOnBrokenTemplate(t *testing.T) {
	fsys := validPageFS()
	fsys["templates/endpoints.html"] = &fstest.MapFile{Data: []byte(`{{define "content"}}{{.EndpointCards`)} // unclosed action

	if _, err := parsePages(fsys); err == nil {
		t.Fatal("parsePages must fail when a page template is broken")
	} else if !strings.Contains(err.Error(), `"endpoints"`) {
		t.Errorf("error should name the failing page: %v", err)
	}
}

func TestParsePagesFailsOnBrokenLayout(t *testing.T) {
	fsys := validPageFS()
	fsys["templates/layout.html"] = &fstest.MapFile{Data: []byte(`{{define "layout"}}{{if .Human}}no end{{end}`)}

	if _, err := parsePages(fsys); err == nil {
		t.Fatal("parsePages must fail when the shared layout is broken")
	}
}

func TestParsePagesFailsOnMissingTemplate(t *testing.T) {
	fsys := validPageFS()
	delete(fsys, "templates/endpoints.html")

	if _, err := parsePages(fsys); err == nil {
		t.Fatal("parsePages must fail when a page template file is missing")
	}
}

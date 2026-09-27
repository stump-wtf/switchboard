package buildinfo

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Governing: SPEC-0027 REQ-1 "Build Information" — packages other than internal/buildinfo MUST NOT
// hold a version literal for Switchboard itself, and a test MUST fail if one is assigned to a
// version-named identifier in internal/mcp or internal/web. This is that test. It exists because
// internal/mcp once shipped `serverVersion = "0.1.0"` to every client for every release.

// guardedDirs are scanned relative to this package's directory.
var guardedDirs = []string{"../mcp", "../web"}

// protocolVersions are identifiers whose literal is the version of something other than
// Switchboard, so they are exempt by name (REQ-1: protocol versions, and versions of something
// other than Switchboard, "are exempt by name").
var protocolVersions = map[string]bool{
	"a2aProtocolVersion": true, // A2A protocol version the agent card speaks (internal/web/agentcard.go)
	// The persona's own version, advertised in its A2A agent card's `version` field
	// (internal/web/agentcard.go). A persona is not Switchboard, so this is not the build version.
	"cardVersion": true,
}

var versionLiteral = regexp.MustCompile(`^v?\d+\.\d+\.\d+`)

// versionLiteralAssignments returns "file:line name = literal" for every version-named identifier
// in src assigned a string literal matching versionLiteral: const/var specs, plain assignments,
// and composite-literal keys (so `Implementation{Version: "0.1.0"}` is caught too).
func versionLiteralAssignments(fset *token.FileSet, file *ast.File) []string {
	var hits []string
	check := func(name string, value ast.Expr) {
		if !strings.Contains(strings.ToLower(name), "version") || protocolVersions[name] {
			return
		}
		lit, ok := value.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil || !versionLiteral.MatchString(s) {
			return
		}
		hits = append(hits, fset.Position(lit.Pos()).String()+" "+name+" = "+lit.Value)
	}
	nameOf := func(e ast.Expr) string {
		switch x := e.(type) {
		case *ast.Ident:
			return x.Name
		case *ast.SelectorExpr:
			return x.Sel.Name
		}
		return ""
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.ValueSpec:
			for i, name := range x.Names {
				if i < len(x.Values) {
					check(name.Name, x.Values[i])
				}
			}
		case *ast.AssignStmt:
			if len(x.Lhs) == len(x.Rhs) {
				for i := range x.Lhs {
					check(nameOf(x.Lhs[i]), x.Rhs[i])
				}
			}
		case *ast.KeyValueExpr:
			check(nameOf(x.Key), x.Value)
		}
		return true
	})
	return hits
}

func TestNoVersionLiteralsOutsideBuildinfo(t *testing.T) {
	fset := token.NewFileSet()
	var hits []string
	scanned := 0
	for _, dir := range guardedDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v (the guard must fail, not pass, when it cannot scan)", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			scanned++
			hits = append(hits, versionLiteralAssignments(fset, f)...)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no Go files: the guard would pass vacuously")
	}
	sort.Strings(hits)
	for _, h := range hits {
		t.Errorf("version literal outside internal/buildinfo (read buildinfo.Get() instead): %s", h)
	}
}

// TestVersionLiteralGuardCatchesPlants proves the guard is not vacuous: each shape the old
// `serverVersion = "0.1.0"` could come back in is flagged, and the exempt protocol names are not.
func TestVersionLiteralGuardCatchesPlants(t *testing.T) {
	const src = `package plant
const serverVersion = "0.1.0"
var buildVersion = "v1.2.3-rc1"
const a2aProtocolVersion = "0.3.0"
const serverName = "1.2.3"
func f() {
	var impl struct{ Version string }
	impl.Version = "0.2.0"
	_ = struct{ Version string }{Version: "v0.3.0"}
	_ = impl
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "plant.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	hits := versionLiteralAssignments(fset, f)
	want := []string{"serverVersion", "buildVersion", "Version = \"0.2.0\"", "Version = \"v0.3.0\""}
	if len(hits) != len(want) {
		t.Fatalf("hits = %q, want %d (one per planted literal)", hits, len(want))
	}
	for i, w := range want {
		if !strings.Contains(hits[i], w) {
			t.Errorf("hit %d = %q, want it to name %q", i, hits[i], w)
		}
	}
}

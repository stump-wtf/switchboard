package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// The quarantine / notify-hook contract, checked on the source so it binds at merge time.
//
// SPEC-0026 REQ-6 forbids any agent-facing signal for a held todo, and REQ-7 says a release "MUST
// ring doorbells and fire hooks exactly as a freshly routed todo would". The SPEC-0024 notify-hook
// dispatcher (#358) adds a second after-commit signal, fireReady, beside every fireDoorbell call.
// The two land on separate branches, so this test holds whichever merges second to both halves:
//
//   - fireDoorbell refuses the quarantine queue, and ApplyQuarantineRelease rings it (always).
//   - once fireReady exists: it refuses the quarantine queue too, and every function that rings a
//     doorbell also fires the ready hook (so a release fires one per new row, and a held intake
//     none).
//
// Governing: ADR-0031, SPEC-0026 REQ-6, REQ-7; SPEC-0024 REQ-6 "Trigger and Sender Gate".
func TestReadyHookFollowsTheDoorbellAndRefusesQuarantine(t *testing.T) {
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list store sources: %v", err)
	}
	funcs := map[string]*ast.FuncDecl{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				funcs[fd.Name.Name] = fd
			}
		}
	}
	calls := func(fd *ast.FuncDecl, name string) bool {
		found := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
					found = true
				}
			}
			return !found
		})
		return found
	}
	guardsQuarantine := func(fd *ast.FuncDecl) bool {
		found := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "QueueQuarantine" {
				found = true
			}
			return !found
		})
		return found
	}

	door, ok := funcs["fireDoorbell"]
	if !ok {
		t.Fatal("fireDoorbell is gone: re-check the quarantine signal guard (SPEC-0026 REQ-6)")
	}
	if !guardsQuarantine(door) {
		t.Error("fireDoorbell no longer refuses the quarantine queue (SPEC-0026 REQ-6)")
	}
	if rel, ok := funcs["ApplyQuarantineRelease"]; !ok || !calls(rel, "fireDoorbell") {
		t.Error("ApplyQuarantineRelease must ring doorbells as a freshly routed todo would (SPEC-0026 REQ-7)")
	}

	ready, ok := funcs["fireReady"]
	if !ok {
		t.Log("fireReady (SPEC-0024 #358) is not on this branch yet; its half of the contract is checked once it is")
		return
	}
	if !guardsQuarantine(ready) {
		t.Error("fireReady must refuse the quarantine queue, as fireDoorbell does (SPEC-0026 REQ-6)")
	}
	for name, fd := range funcs {
		if name == "fireDoorbell" || !calls(fd, "fireDoorbell") {
			continue
		}
		if !calls(fd, "fireReady") {
			t.Errorf("%s rings a doorbell but fires no ready hook: SPEC-0026 REQ-7 and SPEC-0024 REQ-6 need both", name)
		}
	}
}

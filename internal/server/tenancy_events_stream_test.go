// Tenancy-suite additions (issue #177, part of #176): the two gaps the main suite
// (tenancy_isolation_test.go) deliberately left open, filed as separate tests so they can be
// reviewed on their own merits.
//
//  1. The SSE stream is a tenant surface too. "GET /events" was in the coverage table but never
//     deep-driven; live.go's own comment said every authenticated human "operates the same
//     single-tenant board", which made the stream the worst leak in #176 — it needed no
//     navigation at all. This test subscribes as bob (bounded read: the stream never ends on its
//     own) and asserts alice's identifiers never arrive in the bytes bob receives.
//
//  2. The store-level guard issue #177 asked for: every exported Store method in the operator
//     board's read files must take a human-scoping parameter, AST-parsed rather than string-
//     matched. An unscoped operator read — the original shape of the #176 bug — is now
//     un-writable without this suite failing at review time.
//
// Governing: SPEC-0007 REQ "Human as Accountable Principal"; SPEC-0013; ADR-0022.
package server

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// TestTenancyEventsStreamLeaksNothing: bob subscribes to the live-event stream and must receive
// nothing attributable to alice — no todo ids, titles, or her endpoint slug — in the frames that
// arrive within the window. The stream never terminates on its own, so the read is bounded and
// the assertion runs on whatever bytes arrived before the deadline.
//
// HONEST LIMITATION: with no in-flight delivery, the hub sends no frames and this passes
// vacuously. It still guards the regression where any seeded row or count frame leaks into a
// subscriber's stream, but the delivery-driven variant (subscribe as bob, push a signed webhook
// for alice's endpoint, assert her title never arrives) should reuse the board_live_test harness
// and is tracked as a follow-up. Do not mistake green here for the stream being scoped.
func TestTenancyEventsStreamLeaksNothing(t *testing.T) {
	f := newTenancyFixture(t)

	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: f.bobTok})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, req)

	body := rec.Body.String()
	assertNotIn(t, "GET /events stream", body,
		f.todoAPending.ID, f.todoAClaimed.ID,
		f.todoAPending.Title, f.todoAClaimed.Title,
		f.epA.ID, "sbk_alice",
	)
}

// TestStoreOperatorReadsTakeAHumanScope — the store-level guard issue #177 asked for. Every
// exported Store method in the operator board's read files (queue_view.go, board.go) must take a
// parameter whose name carries the human scope (humanID / ownerHumanID / forHuman …), AST-parsed
// via go/parser so the check survives formatting and comment drift.
//
// RED today: TodoCounts, ListTodoItems, GetTodoItem, BoardStats, RecentEvents, EventByID and
// EventBuckets take no scope. Goes green as #176's scoping renames them onto the tenant chain.
func TestStoreOperatorReadsTakeAHumanScope(t *testing.T) {
	files := []string{
		"../store/queue_view.go",
		"../store/board.go",
	}
	// scopeExempt: methods that legitimately take no tenant — health probes, not operator
	// reads. Adding to this list requires a reason in the review.
	scopeExempt := map[string]bool{"Ping": true}

	for _, f := range files {
		fset := token.NewFileSet()
		node, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, decl := range node.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || !fn.Name.IsExported() {
				continue
			}
			if scopeExempt[fn.Name.Name] {
				continue
			}
			if fn.Type.Params == nil || len(fn.Type.Params.List) == 0 {
				t.Errorf("%s: %s takes no parameters — operator reads must take the session human's scope", f, fn.Name.Name)
				continue
			}
			scoped := false
			for _, p := range fn.Type.Params.List {
				for _, name := range p.Names {
					if strings.Contains(strings.ToLower(name.Name), "human") {
						scoped = true
					}
				}
			}
			if !scoped {
				t.Errorf("%s: %s takes no human-scoping parameter — an unscoped operator read is how #176 happened", f, fn.Name.Name)
			}
		}
	}
}

// Compile-time guard that the chi import stays used: the coverage walk lives in
// tenancy_isolation_test.go; this file's SSE test needs the router only transitively, but the
// chi reference here documents that these tests drive the real router, not handlers directly.
var _ chi.Router = (chi.Router)(nil)

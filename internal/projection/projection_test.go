package projection

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const giteaIssueBody = `{"action":"opened","repository":{"id":11,"full_name":"acme/widget","owner":{"login":"acme","id":7}},
"issue":{"number":4812,"title":"slim responses","labels":[{"id":1,"name":"bug"},{"id":2,"name":"size/m"}],
"user":{"login":"squidney-j","id":99}},"sender":{"login":"acme-bot","id":5},"installations":[{"id":3},{"id":4}]}`

func project(t *testing.T, body string, fields ...string) string {
	t.Helper()
	got, ok, why := Apply([]byte(body), fields)
	if !ok {
		t.Fatalf("Apply(%v) not applied: %s", fields, why)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func TestApplyKeepsOnlyDeclaredFields(t *testing.T) {
	got := project(t, giteaIssueBody, "issue.number", "issue.labels[].name", "repository.full_name")
	want := `{"issue":{"labels":[{"name":"bug"},{"name":"size/m"}],"number":4812},"repository":{"full_name":"acme/widget"}}`
	if got != want {
		t.Fatalf("projection =\n%s\nwant\n%s", got, want)
	}
}

func TestApplyOmitsMissingPaths(t *testing.T) {
	// A path the document does not carry is left out rather than written as null: a projection that
	// says "absent" and one that says "null" are not the same claim, and absence is cheaper.
	got := project(t, giteaIssueBody, "issue.number", "pull_request.number", "issue.labels[].url")
	if want := `{"issue":{"number":4812}}`; got != want {
		t.Fatalf("projection = %s, want %s", got, want)
	}
}

func TestApplyKeepsNumbersVerbatim(t *testing.T) {
	got := project(t, `{"issue":{"number":1234567890123},"ratio":1.50}`, "issue.number", "ratio")
	if !strings.Contains(got, "1234567890123") {
		t.Fatalf("projection = %s, want the id in source notation", got)
	}
	if !strings.Contains(got, "1.50") {
		t.Fatalf("projection = %s, want the number as written", got)
	}
}

func TestApplyOverlappingFieldsShareTheirPrefix(t *testing.T) {
	got := project(t, giteaIssueBody, "issue.user.login", "issue.number")
	if want := `{"issue":{"number":4812,"user":{"login":"squidney-j"}}}`; got != want {
		t.Fatalf("projection = %s, want %s", got, want)
	}
}

func TestApplyTrailingEachIsTheArrayItself(t *testing.T) {
	got := project(t, `{"a":[{"x":1},{"x":2}]}`, "a[]")
	if want := `{"a":[{"x":1},{"x":2}]}`; got != want {
		t.Fatalf("projection = %s, want %s", got, want)
	}
}

func TestApplyNotAppliedReportsWhy(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		fields     []string
		contains   string
	}{
		{"empty payload", "", []string{"a"}, "the payload is empty"},
		{"not json", "not json at all", []string{"a"}, "the payload is not JSON"},
		{"array root", `[1,2]`, []string{"a"}, "not a JSON object"},
		{"no field matches", giteaIssueBody, []string{"nothing.here"}, "not_applied"},
		{"bad selector", giteaIssueBody, []string{"a..b"}, "empty path segment"},
	} {
		got, ok, why := Apply([]byte(tc.body), tc.fields)
		if ok || got != nil || !strings.Contains(why, tc.contains) {
			t.Errorf("%s: Apply = (%v, %v, %q), want not applied mentioning %q", tc.name, got, ok, why, tc.contains)
		}
	}
}

func TestApplyNothingAskedIsNotAProjection(t *testing.T) {
	if got, ok, why := Apply([]byte(giteaIssueBody), nil); ok || got != nil || why != "" {
		t.Fatalf("no selector list = (%v, %v, %q), want the caller to keep its own document", got, ok, why)
	}
}

func TestApplyRespectsTheOutputCap(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"issue":{"labels":[`)
	pad := strings.Repeat("x", 40)
	for i := range MaxProjectedBytes / 50 {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"name":"%s-%d"}`, pad, i)
	}
	sb.WriteString(`]}}`)
	got, ok, why := Apply([]byte(sb.String()), []string{"issue.labels[]"})
	if ok || got != nil || !strings.Contains(why, "the limit is") {
		t.Fatalf("oversized projection = (ok=%v, why=%q), want refusal naming the cap", ok, why)
	}
}

func TestValidateAcceptsRealSelectorLists(t *testing.T) {
	for _, fields := range [][]string{
		{"issue.number"},
		{"issue.labels[].name", "repository.full_name", "action"},
		{"pull_request.head.sha", "sender.login"},
		{"a[].b", "a[].b[].c"},
		{"a.b.c.d.e.f.g.h.i.j.k.l.m.n.o.p"},
		{"x_y", "x-y", "x@y", "x:y", "x$y", "1"},
	} {
		if err := Validate(fields); err != nil {
			t.Errorf("Validate(%v) = %v, want nil", fields, err)
		}
	}
}

func TestValidateRefusesUnusableLists(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fields   []string
		contains string
	}{
		{"empty field", []string{""}, "empty field"},
		{"leading dot", []string{".a"}, "empty path segment"},
		{"trailing dot", []string{"a."}, "empty path segment"},
		{"double dot", []string{"a..b"}, "empty path segment"},
		{"each with no name before it", []string{"[].a"}, "no field name before it"},
		{"each repeated", []string{"a[][]"}, "is not a field name"},
		{"brackets inside a key", []string{"a[b]"}, "not a field name"},
		{"space in a key", []string{"a b"}, "not a field name"},
		{"dotted key cannot be written", []string{"a.b.c.d.e.f.g.h.i.j.k.l.m.n.o.p.q"}, "path segments"},
		{"too many fields", make([]string, MaxFields+1), "the limit is"},
		{"field too long", []string{strings.Repeat("a", MaxFieldBytes+1)}, "longer than"},
	} {
		err := Validate(tc.fields)
		if err == nil || !strings.Contains(err.Error(), tc.contains) {
			t.Errorf("%s: Validate = %v, want it to mention %q", tc.name, err, tc.contains)
		}
	}
}

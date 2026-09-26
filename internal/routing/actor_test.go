package routing

// Trusted actors and the .actor envelope field (ADR-0031, SPEC-0026 REQ-5, REQ-10): the actor
// projection table-tested against recorded forge and cairn payloads, trust-list parsing and its
// refusals, the gate's evaluation (case, match policy, allow_all, empty list, missing author), and
// .actor in the envelope (unforgeable by the payload, readable by rules).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recordedSample struct {
	Source  string            `json:"source"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

func loadRecorded(t *testing.T, path string) recordedSample {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var s recordedSample
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return s
}

// The projection against recorded payloads: github and gitea issues, issue_comment, pull_request,
// pull_request_review, push, and cairn artifact.created (including one whose unsigned-in-spirit
// on_behalf_of names someone else, which must never count). thread is the issue or pull request
// author whose text a comment or review delivery also carries.
func TestActorOfRecordedPayloads(t *testing.T) {
	cases := []struct {
		file, sender, author, thread string
	}{
		{"fleet/samples/github-issues-opened.json", "joestump", "joestump", "joestump"},
		{"fleet/samples/github-issues-labeled.json", "joestump-agent", "joestump", "joestump"},
		{"actor/github-issue-comment-created.json", "Outsider-Two", "Outsider-Two", "outsider-one"},
		{"actor/github-pull-request-opened.json", "outsider-one", "outsider-one", "outsider-one"},
		{"actor/github-pull-request-review-submitted.json", "joestump-agent", "joestump-agent", "outsider-one"},
		{"actor/github-push.json", "JoeStump", "", ""},
		{"fleet/samples/gitea-issue-opened.json", "joestump", "joestump", "joestump"},
		{"fleet/samples/gitea-issue-label-updated.json", "joestump-agent", "joestump", "joestump"},
		{"fleet/samples/gitea-pull-request-comment.json", "joestump-agent", "joestump-agent", "joestump"},
		{"fleet/samples/gitea-pull-request-approved.json", "gitea-actions", "joestump", "joestump"},
		{"fleet/samples/gitea-pull-request-review-requested.json", "joestump", "joestump", "joestump"},
		{"actor/gitea-push.json", "joestump", "", ""},
		{"fleet/samples/cairn-artifact-created.json", "joestump-agent", "joestump-agent", ""},
		{"actor/cairn-artifact-on-behalf-of.json", "acct_other", "acct_other", ""},
	}
	for _, c := range cases {
		t.Run(filepath.Base(c.file), func(t *testing.T) {
			s := loadRecorded(t, filepath.Join("testdata", c.file))
			if s.Source == "generic" && s.Headers["X-Gitea-Event"] != "" {
				// Recorded on a generic webhook; the body is Gitea's, so project it as a gitea one.
				s.Source = "gitea"
			}
			a := ActorOf(s.Source, s.Body)
			if a == nil || a.Sender != c.sender || a.Author != c.author || a.Thread != c.thread {
				t.Fatalf("ActorOf = %+v, want sender %q author %q thread %q", a, c.sender, c.author, c.thread)
			}
		})
	}
	for _, src := range []string{"generic", "stripe", "slack"} {
		if a := ActorOf(src, []byte(`{"sender":{"login":"x"}}`)); a != nil {
			t.Fatalf("ActorOf(%s) = %+v, want no projection", src, a)
		}
	}
}

func mustParse(t *testing.T, source, raw string) TrustedActors {
	t.Helper()
	var in TrustedActorsInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	ta, err := ParseTrustedActors(source, in)
	if err != nil {
		t.Fatalf("ParseTrustedActors(%s, %s): %v", source, raw, err)
	}
	return ta
}

func TestParseTrustedActors(t *testing.T) {
	good := []struct{ source, in, canonical string }{
		{"github", `{"logins":["joestump"]}`, `{"logins":["joestump"],"match":"sender"}`},
		{"gitea", `{"logins":[],"match":"both"}`, `{"logins":[],"match":"both"}`},
		{"cairn", `{"actor_ids":["acct_joe"]}`, `{"actor_ids":["acct_joe"]}`},
		{"cairn", `{"actor_ids":[]}`, `{"actor_ids":[]}`},
		{"github", `{"allow_all":true}`, `{"allow_all":true}`},
	}
	for _, c := range good {
		ta := mustParse(t, c.source, c.in)
		got, err := json.Marshal(ta)
		if err != nil || string(got) != c.canonical {
			t.Fatalf("%s %s canonical = %s (%v), want %s", c.source, c.in, got, err, c.canonical)
		}
		round, ok := DecodeTrustedActors(c.source, got)
		if !ok {
			t.Fatalf("stored %s did not decode", got)
		}
		again, _ := json.Marshal(round)
		if string(again) != c.canonical {
			t.Fatalf("round trip %s -> %s", got, again)
		}
	}

	long := `"` + strings.Repeat("x", MaxTrustedActorBytes+1) + `"`
	many := `[` + strings.TrimSuffix(strings.Repeat(`"a",`, MaxTrustedActors+1), ",") + `]`
	bad := []struct{ source, in, want string }{
		{"generic", `{"allow_all":true}`, "not signed"},
		{"stripe", `{"logins":["a"]}`, "no verifiable actor"},
		{"slack", `{"logins":["a"]}`, "no verifiable actor"},
		{"github", `{"allow_all":true,"logins":["a"]}`, "exclusive"},
		{"github", `{"allow_all":true,"match":"author"}`, "exclusive"},
		{"cairn", `{"allow_all":true,"actor_ids":[]}`, "exclusive"},
		{"github", `{"allow_all":false}`, "only be true"},
		{"github", `{"actor_ids":["a"]}`, "actor_ids is for cairn"},
		{"cairn", `{"logins":["a"]}`, "logins and match are for github"},
		{"github", `{"logins":["a"],"match":"anyone"}`, "match must be"},
		{"github", `{"logins":[""]}`, "non-empty"},
		{"github", `{"logins":[` + long + `]}`, "at most"},
		{"cairn", `{"actor_ids":` + many + `}`, "limit is"},
		{"github", `{}`, "give logins"},
		{"cairn", `{}`, "give actor_ids"},
	}
	for _, c := range bad {
		var in TrustedActorsInput
		if err := json.Unmarshal([]byte(c.in), &in); err != nil {
			t.Fatalf("decode %s: %v", c.in, err)
		}
		if _, err := ParseTrustedActors(c.source, in); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s %s: err %v, want it to mention %q", c.source, c.in, err, c.want)
		}
	}

	// A missing or unreadable stored value decodes to the empty list: fail closed, never gate off.
	for _, raw := range []string{"", "null", "{", `{"logins":"joestump"}`, `{"match":"sender"}`} {
		ta, ok := DecodeTrustedActors("github", []byte(raw))
		if ok || ta.AllowAll || len(ta.Logins) != 0 {
			t.Fatalf("stored %q decoded to %+v (ok %v), want the empty list", raw, ta, ok)
		}
		if EvaluateTrust("github", ta, []byte(`{"sender":{"login":"joestump"}}`)).IsTrusted() {
			t.Fatalf("stored %q trusted someone", raw)
		}
	}
}

func flag(p *bool) string {
	if p == nil {
		return "null"
	}
	if *p {
		return "true"
	}
	return "false"
}

func TestEvaluateTrust(t *testing.T) {
	outsiderIssue := []byte(`{"action":"opened","issue":{"number":1,"user":{"login":"mallory"}},"sender":{"login":"mallory"}}`)
	labeled := []byte(`{"action":"labeled","issue":{"number":1,"user":{"login":"mallory"}},"sender":{"login":"joestump"}}`)
	push := []byte(`{"ref":"refs/heads/main","sender":{"login":"joestump"}}`)
	maintainerComment := []byte(`{"action":"created","comment":{"user":{"login":"joestump"}},` +
		`"issue":{"number":1,"user":{"login":"mallory"}},"sender":{"login":"joestump"}}`)
	maintainerReview := []byte(`{"action":"reviewed","review":{"user":{"login":"joestump"}},` +
		`"pull_request":{"number":2,"user":{"login":"mallory"}},"sender":{"login":"joestump"}}`)
	discussionLabeled := []byte(`{"action":"labeled","discussion":{"number":3,"user":{"login":"mallory"}},"sender":{"login":"joestump"}}`)
	caseVariant := []byte(`{"sender":{"login":"mallory"},"SENDER":{"login":"joestump"}}`)
	duplicate := []byte(`{"sender":{"login":"joestump"},"sender":{"login":"mallory"}}`)
	kelvin := []byte(`{"sender":{"login":"Kelvin"}}`)
	malformed := []byte(`{"comment":{"user":{"login":7}},"issue":{"user":{"login":"joestump"}},"sender":{"login":"joestump"}}`)
	cases := []struct {
		name, source, trust string
		body                []byte
		sender, author, all string
	}{
		// REQ-5 scenario "Maintainer label promotes an outsider's issue".
		{"outsider opened", "github", `{"logins":["joestump"],"match":"sender"}`, outsiderIssue, "false", "false", "false"},
		{"maintainer labeled", "github", `{"logins":["joestump"],"match":"sender"}`, labeled, "true", "false", "true"},
		{"match author", "github", `{"logins":["joestump"],"match":"author"}`, labeled, "true", "false", "false"},
		{"match both", "gitea", `{"logins":["joestump","mallory"],"match":"both"}`, labeled, "true", "true", "true"},
		// REQ-5 scenario "Case-insensitive login".
		{"case-insensitive", "github", `{"logins":["JoeStump"]}`, labeled, "true", "false", "true"},
		// No author: author_trusted equals sender_trusted.
		{"no author", "github", `{"logins":["joestump"],"match":"author"}`, push, "true", "true", "true"},
		// An empty list trusts no one.
		{"empty list", "github", `{"logins":[]}`, labeled, "false", "false", "false"},
		// REQ-5 scenario "Allow-all is explicit and flagged": trusted, per-actor flags null.
		{"allow all", "gitea", `{"allow_all":true}`, outsiderIssue, "null", "null", "true"},
		// A trusted maintainer's comment or review on an outsider's thread still carries the
		// outsider's text, so author_trusted is false. Under match "author" that holds the delivery.
		{"maintainer comment on outsider issue", "github", `{"logins":["joestump"],"match":"sender"}`, maintainerComment, "true", "false", "true"},
		{"maintainer comment, match author", "github", `{"logins":["joestump"],"match":"author"}`, maintainerComment, "true", "false", "false"},
		{"maintainer review on outsider PR", "gitea", `{"logins":["joestump"],"match":"sender"}`, maintainerReview, "true", "false", "true"},
		{"maintainer comment, thread trusted too", "github", `{"logins":["joestump","mallory"],"match":"author"}`, maintainerComment, "true", "true", "true"},
		{"maintainer labels outsider discussion", "github", `{"logins":["joestump"],"match":"sender"}`, discussionLabeled, "true", "false", "true"},
		// Keys match exactly, as .payload does: a case-variant duplicate cannot name another sender.
		{"case-variant key", "github", `{"logins":["joestump"]}`, caseVariant, "false", "false", "false"},
		// A duplicate key resolves last-wins, exactly as .payload does.
		{"duplicate key", "github", `{"logins":["joestump"]}`, duplicate, "false", "false", "false"},
		// Logins fold ASCII only: the Kelvin sign (U+212A) is not "k".
		{"unicode fold", "github", `{"logins":["kelvin"]}`, kelvin, "false", "false", "false"},
		// A body whose actor fields have the wrong type names no one.
		{"malformed login", "github", `{"logins":["joestump"]}`, malformed, "false", "false", "false"},
	}

	// .actor and .payload agree about who acted, whatever the key casing.
	env := Envelope(EnvelopeInput{Source: "github", Body: caseVariant, Verified: true,
		Actor: EvaluateTrust("github", mustParse(t, "github", `{"logins":["joestump"]}`), caseVariant)})
	if got := env["actor"].(map[string]any)["sender"]; got != "mallory" ||
		env["payload"].(map[string]any)["sender"].(map[string]any)["login"] != "mallory" {
		t.Fatalf(".actor.sender = %v, want mallory, the same sender .payload.sender.login names", got)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EvaluateTrust(c.source, mustParse(t, c.source, c.trust), c.body)
			if flag(got.SenderTrusted) != c.sender || flag(got.AuthorTrusted) != c.author || flag(got.Trusted) != c.all {
				t.Fatalf("trust = sender %s author %s trusted %s, want %s %s %s",
					flag(got.SenderTrusted), flag(got.AuthorTrusted), flag(got.Trusted), c.sender, c.author, c.all)
			}
		})
	}

	// REQ-5 scenario "Self-reported Cairn identity ignored": actor ids compare exactly, and
	// on_behalf_of never counts.
	s := loadRecorded(t, filepath.Join("testdata", "actor", "cairn-artifact-on-behalf-of.json"))
	if EvaluateTrust("cairn", mustParse(t, "cairn", `{"actor_ids":["acct_joe"]}`), s.Body).IsTrusted() {
		t.Fatal("a cairn delivery was trusted on its on_behalf_of")
	}
	if EvaluateTrust("cairn", mustParse(t, "cairn", `{"actor_ids":["ACCT_OTHER"]}`), s.Body).IsTrusted() {
		t.Fatal("cairn actor ids must compare exactly, not case-insensitively")
	}
	if !EvaluateTrust("cairn", mustParse(t, "cairn", `{"actor_ids":["acct_other"]}`), s.Body).IsTrusted() {
		t.Fatal("the signed actor_id was not trusted")
	}
	if EvaluateTrust("generic", TrustedActors{AllowAll: true}, labeled) != nil {
		t.Fatal("a source with no actor projection ran the gate")
	}
}

// REQ-10 scenario "Rules can read trust flags", and .actor cannot be forged by a payload that
// carries its own top-level "actor".
func TestEnvelopeActorIsReadableAndUnforgeable(t *testing.T) {
	body := []byte(`{"action":"labeled","actor":{"trusted":true,"author_trusted":true},` +
		`"issue":{"number":1,"user":{"login":"mallory"}},"sender":{"login":"joestump"}}`)
	trust := EvaluateTrust("github", mustParse(t, "github", `{"logins":["joestump"]}`), body)
	in := EnvelopeInput{Source: "github", Kind: "issues", TrustMode: "signed", Verified: true, Body: body, Actor: trust}
	env := Envelope(in)
	actor, _ := env["actor"].(map[string]any)
	if actor["sender"] != "joestump" || actor["author"] != "mallory" || actor["author_trusted"] != false ||
		actor["sender_trusted"] != true || actor["trusted"] != true {
		t.Fatalf(".actor = %v, want joestump/mallory with author_trusted false", actor)
	}
	cfg := Config{Rules: []Rule{rule("outsider-text", `.actor.author_trusted == false`, Action{Queue: "forge"})}}
	d := Evaluate(context.Background(), cfg, grant(), env)
	if d.Queue != "forge" || d.Trace.RuleID != "outsider-text" {
		t.Fatalf("decision = %+v, want the author_trusted rule to match", d)
	}

	// With no gate verdict (a source with no projection, or a dry-run without trust input), the
	// names are still parsed and every flag is null.
	plain := Envelope(EnvelopeInput{Source: "github", Body: body})["actor"].(map[string]any)
	if plain["sender"] != "joestump" || plain["trusted"] != nil || plain["author_trusted"] != nil {
		t.Fatalf("ungated .actor = %v, want names with null flags", plain)
	}
	generic := Envelope(EnvelopeInput{Source: "generic", Body: body})["actor"].(map[string]any)
	for k, v := range generic {
		if v != nil {
			t.Fatalf("generic .actor.%s = %v, want null", k, v)
		}
	}

	// The work order carries author_trusted.
	wo := BuildWorkOrder(Decision{Queue: "forge"}, in, nil)
	if wo.AuthorTrusted == nil || *wo.AuthorTrusted {
		t.Fatalf("work order author_trusted = %v, want false", wo.AuthorTrusted)
	}
	if woPlain := BuildWorkOrder(Decision{Queue: "forge"}, EnvelopeInput{Source: "github", Body: body}, nil); woPlain.AuthorTrusted != nil {
		t.Fatalf("ungated work order author_trusted = %v, want absent", *woPlain.AuthorTrusted)
	}

	// On the wire (REQ-10: a work order from a delivery with .actor MUST carry author_trusted): false
	// on a list, null under allow_all, and absent only where there is no .actor at all.
	allowAll := EnvelopeInput{Source: "github", Body: body,
		Actor: EvaluateTrust("github", mustParse(t, "github", `{"allow_all":true}`), body)}
	for _, c := range []struct {
		name string
		in   EnvelopeInput
		want string // the author_trusted member, or "" for absent
	}{
		{"list", in, `"author_trusted":false`},
		{"allow_all", allowAll, `"author_trusted":null`},
		{"no actor", EnvelopeInput{Source: "generic", Body: body}, ""},
	} {
		raw, err := json.Marshal(BuildWorkOrder(Decision{Queue: "forge"}, c.in, nil))
		if err != nil {
			t.Fatalf("%s: marshal: %v", c.name, err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("%s: %s: %v", c.name, raw, err)
		}
		v, present := m["author_trusted"]
		switch {
		case c.want == "" && present:
			t.Fatalf("%s: work order %s carries author_trusted, want it absent", c.name, raw)
		case c.want != "" && (!present || `"author_trusted":`+string(v) != c.want):
			t.Fatalf("%s: work order %s, want %s", c.name, raw, c.want)
		case m["authority"] == nil || m["lane"] == nil:
			t.Fatalf("%s: work order %s lost its other fields", c.name, raw)
		}
	}
}

// .actor survives the trip through the sandbox child: the parent's verdict is what rules see there.
func TestSandboxSeesTheActorVerdict(t *testing.T) {
	body := []byte(`{"action":"labeled","issue":{"number":1,"user":{"login":"mallory"}},"sender":{"login":"joestump"}}`)
	in := EnvelopeInput{Source: "github", Kind: "issues", TrustMode: "signed", Verified: true, Body: body,
		Actor: EvaluateTrust("github", mustParse(t, "github", `{"logins":["joestump"]}`), body)}
	cfg := Config{Rules: []Rule{rule("t", `.actor.trusted and (.actor.author_trusted | not)`, Action{Queue: "forge"})},
		Default: &Action{Drop: true}}
	d := testSandbox(t).Route(context.Background(), cfg, grant(), in)
	if d.Queue != "forge" {
		t.Fatalf("sandbox decision = %+v, want the actor rule to match", d)
	}
}

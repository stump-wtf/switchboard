package main

// The operator CLI's contract, verb by verb: dispatch and help, flag parsing with flags on either
// side of the positional arguments, the credentials file (location override, 0600, idempotent
// logout), and each verb's output against the fake deployment in fake_test.go.

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/switchboard/internal/buildinfo"
)

// testCLI is a cli with every edge captured: buffered output, a private environment, a stubbed
// serve, no browser, and a fixed clock.
type testCLI struct {
	*cli
	stdout, stderr *bytes.Buffer
	env            map[string]string
	serveCalls     [][]string
	clock          time.Time
}

func newTestCLI(t *testing.T) *testCLI {
	t.Helper()
	tc := &testCLI{
		cli:    newCLI(),
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
		env:    map[string]string{"SWITCHBOARD_CREDENTIALS": filepath.Join(t.TempDir(), "creds", "credentials.json")},
		clock:  time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
	}
	tc.cli.stdout, tc.cli.stderr = tc.stdout, tc.stderr
	tc.getenv = func(k string) string { return tc.env[k] }
	tc.openBrowser = func(string) error { return errors.New("no browser in tests") }
	tc.serve = func(_ *cli, args []string) int { tc.serveCalls = append(tc.serveCalls, args); return exitOK }
	tc.now = func() time.Time { return tc.clock }
	tc.loginTimeout = 5 * time.Second
	return tc
}

// loggedIn seeds a credentials file that matches the fake deployment's current pair.
func (tc *testCLI) loggedIn(t *testing.T, f *fakeDeployment, expiresAt time.Time) {
	t.Helper()
	f.mu.Lock()
	if f.access == "" {
		f.issue()
	}
	creds := &credentials{BaseURL: f.base(), ClientID: "cid-test", AccessToken: f.access, RefreshToken: f.refresh, ExpiresAt: expiresAt}
	f.mu.Unlock()
	if err := tc.saveCredentials(creds); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}
}

func (tc *testCLI) run(t *testing.T, args ...string) int {
	t.Helper()
	tc.stdout.Reset()
	tc.stderr.Reset()
	return tc.cli.run(args)
}

func mustContain(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("%s missing %q:\n%s", what, want, got)
		}
	}
}

// --- dispatch ---

func TestDispatchServeIsTheDefault(t *testing.T) {
	tc := newTestCLI(t)
	if code := tc.run(t); code != exitOK || len(tc.serveCalls) != 1 {
		t.Fatalf("bare invocation: code %d, serve calls %v", code, tc.serveCalls)
	}
	if code := tc.run(t, "serve"); code != exitOK || len(tc.serveCalls) != 2 {
		t.Fatalf("explicit serve: code %d, serve calls %v", code, tc.serveCalls)
	}
}

func TestServeAcceptsOnlyHelp(t *testing.T) {
	tc := newTestCLI(t)
	if code := runServe(tc.cli, []string{"-h"}); code != exitOK {
		t.Fatalf("serve -h: code %d", code)
	}
	mustContain(t, "serve help", tc.stdout.String(), "usage: switchboard serve", "SWITCHBOARD_DATABASE_URL")
	if code := runServe(tc.cli, []string{"bogus"}); code != exitUsage {
		t.Fatalf("serve bogus: code %d, want %d", code, exitUsage)
	}
	mustContain(t, "serve usage error", tc.stderr.String(), `unexpected argument "bogus"`)
}

func TestDispatchHelpAndVersion(t *testing.T) {
	tc := newTestCLI(t)
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}} {
		if code := tc.run(t, args...); code != exitOK {
			t.Fatalf("%v: code %d", args, code)
		}
		for _, cmd := range commands() {
			if cmd.hidden {
				// Pre-grouping aliases still work but are deliberately unlisted.
				continue
			}
			mustContain(t, "usage", tc.stdout.String(), "switchboard "+cmd.name)
			for _, sub := range cmd.subs {
				mustContain(t, "usage", tc.stdout.String(), "switchboard "+cmd.name+" "+sub.name)
			}
		}
		if tc.stderr.Len() != 0 {
			t.Errorf("%v wrote to stderr: %q", args, tc.stderr.String())
		}
	}
	for _, args := range [][]string{
		{"help", "endpoint", "vend"}, {"endpoint", "vend", "-h"}, {"endpoint", "vend", "--help"},
	} {
		if code := tc.run(t, args...); code != exitOK {
			t.Fatalf("%v: code %d", args, code)
		}
		mustContain(t, "vend help", tc.stdout.String(), "usage: switchboard endpoint vend [flags] NAME", "-queue", "-json")
	}
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		if code := tc.run(t, args...); code != exitOK || strings.TrimSpace(tc.stdout.String()) != "switchboard "+buildinfo.Get().Version {
			t.Fatalf("%v: code %d, stdout %q", args, code, tc.stdout.String())
		}
	}
}

func TestDispatchRejectsUnknownVerbsAndFlags(t *testing.T) {
	tc := newTestCLI(t)
	for _, args := range [][]string{{"nope"}, {"--nope"}, {"help", "nope"}} {
		if code := tc.run(t, args...); code != exitUsage {
			t.Fatalf("%v: code %d, want %d", args, code, exitUsage)
		}
		mustContain(t, "usage error", tc.stderr.String(), "switchboard: unknown", "usage:")
		if tc.stdout.Len() != 0 {
			t.Errorf("%v wrote usage to stdout: %q", args, tc.stdout.String())
		}
	}
	if len(tc.serveCalls) != 0 {
		t.Fatalf("a usage mistake must never fall through to serve: %v", tc.serveCalls)
	}
}

// --- flag parsing ---

func TestParseArgsInterspersed(t *testing.T) {
	cases := []struct {
		args []string
		pos  []string
		q    string
	}{
		{[]string{"name", "-q", "ops"}, []string{"name"}, "ops"},
		{[]string{"-q", "ops", "name"}, []string{"name"}, "ops"},
		{[]string{"name", "--q=ops"}, []string{"name"}, "ops"},
		{[]string{"a", "-q", "ops", "b"}, []string{"a", "b"}, "ops"},
		{[]string{"--", "-q", "literal"}, []string{"-q", "literal"}, "inbox"},
		{[]string{"-q", "ops", "--", "--name"}, []string{"--name"}, "ops"},
	}
	for _, tt := range cases {
		tc := newTestCLI(t)
		fs := tc.flagSet("t", "NAME", "test")
		q := fs.String("q", "inbox", "queue")
		pos, code, ok := tc.parseArgs(fs, tt.args)
		if !ok || code != exitOK {
			t.Fatalf("%v: ok=%v code=%d stderr=%q", tt.args, ok, code, tc.stderr.String())
		}
		if strings.Join(pos, "|") != strings.Join(tt.pos, "|") || *q != tt.q {
			t.Errorf("%v: positional %v q=%q, want %v q=%q", tt.args, pos, *q, tt.pos, tt.q)
		}
	}
}

func TestParseArgsHelpAndErrors(t *testing.T) {
	tc := newTestCLI(t)
	fs := tc.flagSet("t", "NAME", "the summary")
	fs.String("q", "inbox", "queue")
	if _, code, ok := tc.parseArgs(fs, []string{"name", "-h"}); ok || code != exitOK {
		t.Fatalf("-h after a positional: ok=%v code=%d", ok, code)
	}
	mustContain(t, "help", tc.stdout.String(), "usage: switchboard t [flags] NAME", "the summary", "-q string")
	if _, code, ok := tc.parseArgs(fs, []string{"-bogus"}); ok || code != exitUsage {
		t.Fatalf("unknown flag: ok=%v code=%d", ok, code)
	}
	mustContain(t, "unknown flag", tc.stderr.String(), "flag provided but not defined: -bogus", "usage: switchboard t")
}

func TestNormalizeBaseURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://sb.example.com/":       "https://sb.example.com",
		"sb.example.com":                "https://sb.example.com",
		"http://localhost:8080/":        "http://localhost:8080",
		"https://sb.example.com/?x=1#f": "https://sb.example.com",
		" https://sb.example.com ":      "https://sb.example.com",
	} {
		got, err := normalizeBaseURL(in)
		if err != nil || got != want {
			t.Errorf("normalizeBaseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://x", "://nope", "https://"} {
		if _, err := normalizeBaseURL(bad); err == nil {
			t.Errorf("normalizeBaseURL(%q) accepted", bad)
		}
	}
}

// --- credentials file ---

func TestCredentialsFileIsPrivateAndRemovable(t *testing.T) {
	tc := newTestCLI(t)
	creds := &credentials{BaseURL: "https://sb.example.com", ClientID: "cid", AccessToken: "at", RefreshToken: "rt", ExpiresAt: tc.clock.Add(time.Hour)}
	if err := tc.saveCredentials(creds); err != nil {
		t.Fatalf("save: %v", err)
	}
	path := tc.env["SWITCHBOARD_CREDENTIALS"]
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("credentials file not written at the overridden path: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("credentials mode = %o, want 0600", info.Mode().Perm())
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp file left behind: %v", err)
	}
	got, err := tc.loadCredentials()
	if err != nil || got.AccessToken != "at" || !got.ExpiresAt.Equal(creds.ExpiresAt) {
		t.Fatalf("load = %+v, %v", got, err)
	}
	if err := tc.removeCredentials(); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := tc.loadCredentials(); !errors.Is(err, errNotLoggedIn) {
		t.Fatalf("after remove: err = %v, want errNotLoggedIn", err)
	}
	if err := tc.removeCredentials(); err != nil {
		t.Fatalf("second remove must be a no-op: %v", err)
	}
}

func TestCredentialsCorruptFileIsReported(t *testing.T) {
	tc := newTestCLI(t)
	path := tc.env["SWITCHBOARD_CREDENTIALS"]
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := tc.run(t, "agents"); code != exitFailure {
		t.Fatalf("agents with a corrupt file: code %d", code)
	}
	mustContain(t, "corrupt file error", tc.stderr.String(), "is corrupt", "switchboard logout")
}

// --- status / logout ---

func TestStatusAndLogoutLifecycle(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)

	if code := tc.run(t, "status"); code != exitFailure {
		t.Fatalf("status while logged out: code %d, want 1", code)
	}
	mustContain(t, "status (logged out)", tc.stdout.String(), "Not logged in", "switchboard login", tc.env["SWITCHBOARD_CREDENTIALS"])
	if code := tc.run(t, "logout"); code != exitOK || !strings.Contains(tc.stdout.String(), "Not logged in.") {
		t.Fatalf("logout while logged out: code %d, stdout %q", code, tc.stdout.String())
	}

	tc.loggedIn(t, f, tc.clock.Add(42*time.Minute))
	if code := tc.run(t, "status"); code != exitOK {
		t.Fatalf("status: code %d, stderr %q", code, tc.stderr.String())
	}
	mustContain(t, "status", tc.stdout.String(), "Logged in to "+f.base(), "expires in 42m0s", "refreshes automatically")

	tc.clock = tc.clock.Add(time.Hour)
	if code := tc.run(t, "status"); code != exitOK {
		t.Fatalf("status (expired): code %d", code)
	}
	mustContain(t, "status (expired)", tc.stdout.String(), "expired 18m0s ago")

	if code := tc.run(t, "logout"); code != exitOK {
		t.Fatalf("logout: code %d, stderr %q", code, tc.stderr.String())
	}
	mustContain(t, "logout", tc.stdout.String(), "Logged out of "+f.base())
	if _, err := os.Stat(tc.env["SWITCHBOARD_CREDENTIALS"]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("logout must remove the credentials file: %v", err)
	}
	for _, verb := range []string{"agents", "endpoints", "vend"} {
		args := []string{verb}
		if verb == "vend" {
			args = append(args, "x")
		}
		if code := tc.run(t, args...); code != exitFailure || !strings.Contains(tc.stderr.String(), "not logged in") {
			t.Errorf("%s after logout: code %d, stderr %q", verb, code, tc.stderr.String())
		}
	}
}

// --- vend / endpoints / agents ---

func TestVendPrintsTheOneTimeReveal(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))

	if code := tc.run(t, "vend", "release-bot", "--queue", "reviews"); code != exitOK {
		t.Fatalf("vend: code %d, stderr %q", code, tc.stderr.String())
	}
	out := tc.stdout.String()
	mustContain(t, "vend reveal", out,
		"Vended release-bot (slug release-bot-ab12cd34) on queue reviews.",
		"shown ONCE",
		"MCP endpoint  "+f.base()+"/mcp/release-bot-ab12cd34",
		"Bearer token  sbk_"+strings.Repeat("x", 40),
		"Ingest URL    "+f.base()+"/webhooks/w/deadbeef",
		"Verbs         list_todos claim complete",
		"Expires       never",
		"Client wiring", `"mcpServers"`, `"Authorization": "Bearer sbk_`,
	)
	if len(f.vends) != 1 || f.vends[0]["name"] != "release-bot" || f.vends[0]["queue"] != "reviews" {
		t.Fatalf("vend bodies = %v", f.vends)
	}

	// Flags may precede the name; -q is the shorthand; --json prints the API document verbatim.
	if code := tc.run(t, "vend", "-q", "ops", "--json", "ops-bot"); code != exitOK {
		t.Fatalf("vend --json: code %d, stderr %q", code, tc.stderr.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(tc.stdout.Bytes(), &doc); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, tc.stdout.String())
	}
	if doc["agent_name"] != "ops-bot" || doc["queue"] != "ops" || f.vends[1]["queue"] != "ops" {
		t.Fatalf("--json doc = %v, vend body = %v", doc, f.vends[1])
	}
}

func TestVendUsageMistakes(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	for _, args := range [][]string{
		{"endpoint", "vend"}, {"endpoint", "vend", " "}, {"endpoint", "vend", "a", "b"},
		{"endpoint", "vend", "--bogus", "a"}, {"endpoint", "vend", "-q", "", "a"},
	} {
		if code := tc.run(t, args...); code != exitUsage {
			t.Errorf("%v: code %d, want %d (stderr %q)", args, code, exitUsage, tc.stderr.String())
		}
		mustContain(t, "vend usage", tc.stderr.String(), "usage: switchboard endpoint vend")
	}
	if len(f.vends) != 0 {
		t.Fatalf("a usage mistake reached the API: %v", f.vends)
	}
}

func TestVendSurfacesTheServerError(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	// The fake's vend rejects a blank name the way the real API does (400 + plain-text reason);
	// the CLI's own guard trims first, so drive the client directly to reach the server's answer.
	api, err := tc.apiClient(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.post("/api/v1/endpoints", map[string]string{"name": ""}); err == nil ||
		!strings.Contains(err.Error(), "POST /api/v1/endpoints: 400 Bad Request: name is required") {
		t.Fatalf("server error not surfaced: %v", err)
	}
}

func TestEndpointsAndAgentsTables(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))

	if code := tc.run(t, "endpoints"); code != exitOK || !strings.Contains(tc.stdout.String(), "No endpoints vended yet") {
		t.Fatalf("empty endpoints: code %d, stdout %q", code, tc.stdout.String())
	}
	f.mu.Lock()
	f.endpointRows = []map[string]any{
		{"slug": "release-bot-ab12cd34", "agent_name": "release-bot", "state": "active", "queues": []string{"reviews", "ci"}, "verbs": []string{"claim"}, "expires_at": "2026-12-01T00:00:00Z"},
		{"slug": "old-bot-ffffffff", "agent_name": "old-bot", "state": "revoked", "queues": []string{"inbox"}, "verbs": []string{"claim"}, "expires_at": nil},
	}
	f.agentRows = []map[string]any{{"id": "ag-1", "name": "release-bot", "description": "cuts releases"}}
	f.mu.Unlock()

	if code := tc.run(t, "endpoints"); code != exitOK {
		t.Fatalf("endpoints: code %d, stderr %q", code, tc.stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(tc.stdout.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "SLUG") {
		t.Fatalf("endpoints table:\n%s", tc.stdout.String())
	}
	mustContain(t, "endpoints table", tc.stdout.String(), "SLUG", "AGENT", "STATE", "QUEUES", "EXPIRES",
		"release-bot-ab12cd34", "active", "reviews,ci", "2026-12-01T00:00:00Z", "old-bot-ffffffff", "revoked", "never")
	if strings.Contains(tc.stdout.String(), "sbk_") {
		t.Fatal("endpoints must never print a credential")
	}

	if code := tc.run(t, "agents"); code != exitOK {
		t.Fatalf("agents: code %d, stderr %q", code, tc.stderr.String())
	}
	mustContain(t, "agents table", tc.stdout.String(), "ID", "NAME", "DESCRIPTION", "ag-1", "release-bot", "cuts releases")

	if code := tc.run(t, "agents", "--json"); code != exitOK {
		t.Fatalf("agents --json: code %d", code)
	}
	var rows []map[string]any
	if err := json.Unmarshal(tc.stdout.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0]["id"] != "ag-1" {
		t.Fatalf("agents --json = %q, %v", tc.stdout.String(), err)
	}
	if code := tc.run(t, "agents", "extra"); code != exitUsage {
		t.Fatalf("agents extra: code %d, want %d", code, exitUsage)
	}
}

// --- resource-grouped verbs ---

// The grouping is the point: a resource named without a verb, or with one it does not have, must
// say what it does support rather than dumping the whole top-level usage.
func TestResourceGroupUsage(t *testing.T) {
	tc := newTestCLI(t)
	for _, args := range [][]string{{"endpoint"}, {"agent"}} {
		if code := tc.run(t, args...); code != exitUsage {
			t.Errorf("%v: code %d, want %d", args, code, exitUsage)
		}
		mustContain(t, "group usage", tc.stderr.String(), "usage: switchboard "+args[0]+" <verb>", "verbs:")
	}
	if code := tc.run(t, "endpoint", "bogus"); code != exitUsage {
		t.Errorf("unknown verb: code %d, want %d", code, exitUsage)
	}
	mustContain(t, "unknown verb", tc.stderr.String(), `unknown verb "bogus"`, "switchboard endpoint revoke")

	// -h on the group is help, not an error, and lands on stdout.
	for _, args := range [][]string{{"endpoint", "-h"}, {"help", "endpoint"}} {
		if code := tc.run(t, args...); code != exitOK {
			t.Errorf("%v: code %d, want %d", args, code, exitOK)
		}
		mustContain(t, "group help", tc.stdout.String(), "switchboard endpoint list", "switchboard endpoint revoke")
		if tc.stderr.Len() != 0 {
			t.Errorf("%v wrote to stderr: %q", args, tc.stderr.String())
		}
	}
}

// The pre-grouping spellings still reach the same handlers, so anything already scripted against
// them keeps working even though help no longer teaches them.
func TestLegacyAliasesStillWork(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	f.endpointRows = []map[string]any{{"slug": "a-1", "agent_name": "a", "state": "active", "queues": []string{"inbox"}}}

	if code := tc.run(t, "endpoints"); code != exitOK {
		t.Fatalf("legacy `endpoints`: code %d (stderr %q)", code, tc.stderr.String())
	}
	mustContain(t, "legacy endpoints", tc.stdout.String(), "a-1")

	if code := tc.run(t, "vend", "svc", "--queue", "q"); code != exitOK {
		t.Fatalf("legacy `vend`: code %d (stderr %q)", code, tc.stderr.String())
	}
	if len(f.vends) != 1 || f.vends[0]["name"] != "svc" {
		t.Fatalf("legacy vend did not reach the API: %v", f.vends)
	}
}

// --- endpoint revoke ---

func TestEndpointRevoke(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))

	// -y skips the prompt; the ref reaches the API path verbatim.
	if code := tc.run(t, "endpoint", "revoke", "some-slug-ab12cd34", "-y"); code != exitOK {
		t.Fatalf("revoke: code %d (stderr %q)", code, tc.stderr.String())
	}
	if len(f.revokes) != 1 || f.revokes[0] != "some-slug-ab12cd34" {
		t.Fatalf("revokes = %v, want the slug once", f.revokes)
	}
	mustContain(t, "revoke output", tc.stdout.String(), "Revoked some-slug-ab12cd34", "endpoint vend")
}

// Revocation is terminal, so the interactive path must not act on anything but an explicit yes —
// and "no" must leave the endpoint alone rather than falling through to the API.
func TestEndpointRevokeConfirmation(t *testing.T) {
	for _, tc2 := range []struct {
		name, answer string
		wantCall     bool
	}{
		{"yes", "y\n", true},
		{"long yes", "yes\n", true},
		{"no", "n\n", false},
		{"empty", "\n", false},
		{"eof", "", false},
		{"garbage", "sure why not\n", false},
	} {
		t.Run(tc2.name, func(t *testing.T) {
			tc := newTestCLI(t)
			f := newFakeDeployment(t)
			tc.loggedIn(t, f, tc.clock.Add(time.Hour))
			tc.stdin = strings.NewReader(tc2.answer)

			if code := tc.run(t, "endpoint", "revoke", "ep-slug"); code != exitOK {
				t.Fatalf("code %d (stderr %q)", code, tc.stderr.String())
			}
			if got := len(f.revokes) > 0; got != tc2.wantCall {
				t.Fatalf("reached the API = %v, want %v (answer %q)", got, tc2.wantCall, tc2.answer)
			}
			if !tc2.wantCall {
				mustContain(t, "declined", tc.stdout.String(), "Not revoked.")
			}
		})
	}
}

func TestEndpointRevokeUsageAndErrors(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))

	for _, args := range [][]string{
		{"endpoint", "revoke"}, {"endpoint", "revoke", "a", "b"}, {"endpoint", "revoke", "--bogus", "a"},
	} {
		if code := tc.run(t, args...); code != exitUsage {
			t.Errorf("%v: code %d, want %d", args, code, exitUsage)
		}
		mustContain(t, "revoke usage", tc.stderr.String(), "usage: switchboard endpoint revoke")
	}
	if len(f.revokes) != 0 {
		t.Fatalf("a usage mistake reached the API: %v", f.revokes)
	}

	// An already-revoked endpoint is a conflict, and the CLI must surface it as a failure rather
	// than reporting a kill that did not happen.
	f.revokeStatus = 409
	if code := tc.run(t, "endpoint", "revoke", "gone", "-y"); code != exitFailure {
		t.Fatalf("409: code %d, want %d", code, exitFailure)
	}
	mustContain(t, "conflict", tc.stderr.String(), "already revoked")
}

func TestTodoPushPostsAndPrints(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))

	if code := tc.run(t, "todo", "push", "my-agent-k3x9", "look at PR 7",
		"--queue", "reviews", "--payload", `{"pr":7}`, "--key", "pr-7", "--kind", "review"); code != exitOK {
		t.Fatalf("todo push: code %d, stderr %q", code, tc.stderr.String())
	}
	mustContain(t, "push output", tc.stdout.String(), "Pushed td_1 to my-agent-k3x9 on queue reviews", "doorbell rung")
	if len(f.pushes) != 1 {
		t.Fatalf("pushes = %v, want one", f.pushes)
	}
	p := f.pushes[0]
	if p["ref"] != "my-agent-k3x9" || p["title"] != "look at PR 7" || p["queue"] != "reviews" || p["key"] != "pr-7" || p["kind"] != "review" {
		t.Fatalf("push body = %v", p)
	}
	if pl, _ := p["payload"].(map[string]any); pl["pr"] != float64(7) {
		t.Fatalf("payload = %v, want the inline JSON", p["payload"])
	}

	// @file payloads go through the injected reader; a matched key is reported as such.
	tc.readFile = func(name string) ([]byte, error) {
		if name != "order.json" {
			t.Fatalf("readFile(%q), want order.json", name)
		}
		return []byte(`{"pr": 8}`), nil
	}
	f.pushExists = true
	tc.stdout.Reset()
	if code := tc.run(t, "todo", "push", "--payload", "@order.json", "my-agent-k3x9", "look at PR 8"); code != exitOK {
		t.Fatalf("todo push @file: code %d, stderr %q", code, tc.stderr.String())
	}
	mustContain(t, "repeat output", tc.stdout.String(), "Already there: td_1 on my-agent-k3x9", "nothing new minted")
	if pl, _ := f.pushes[1]["payload"].(map[string]any); pl["pr"] != float64(8) {
		t.Fatalf("@file payload = %v", f.pushes[1]["payload"])
	}
	if _, has := f.pushes[1]["queue"]; has {
		t.Fatalf("no --queue given, yet one was sent: %v", f.pushes[1])
	}

	// @- reads stdin; --json prints the API document verbatim.
	tc.stdin = strings.NewReader(`{"pr": 9}`)
	tc.stdout.Reset()
	if code := tc.run(t, "todo", "push", "--json", "--payload", "@-", "my-agent-k3x9", "look at PR 9"); code != exitOK {
		t.Fatalf("todo push --json: code %d, stderr %q", code, tc.stderr.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(tc.stdout.Bytes(), &doc); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, tc.stdout.String())
	}
	if doc["id"] != "td_1" || doc["created"] != false {
		t.Fatalf("--json doc = %v", doc)
	}
	if pl, _ := f.pushes[2]["payload"].(map[string]any); pl["pr"] != float64(9) {
		t.Fatalf("stdin payload = %v", f.pushes[2]["payload"])
	}
}

func TestTodoPushUsageMistakes(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	for _, args := range [][]string{
		{"todo"},
		{"todo", "push"},
		{"todo", "push", "my-agent-k3x9"},
		{"todo", "push", "my-agent-k3x9", "   "},
		{"todo", "push", "my-agent-k3x9", "title", "extra"},
		{"todo", "push", "my-agent-k3x9", "title", "--payload", "not json"},
		{"todo", "push", "my-agent-k3x9", "title", "--bogus"},
	} {
		tc.stderr.Reset()
		if code := tc.run(t, args...); code != exitUsage {
			t.Fatalf("%v: code %d, want %d (stderr %q)", args, code, exitUsage, tc.stderr.String())
		}
	}
	if len(f.pushes) != 0 {
		t.Fatalf("usage mistakes must never reach the API: %v", f.pushes)
	}
}

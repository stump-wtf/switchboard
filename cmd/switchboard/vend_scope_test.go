package main

// Scripted vend scope against the fake deployment: the flags and the scope file must send the scope the
// operator named, a scope the wizard would refuse must stop the command before the deployment mints
// anything, and a named credential destination must keep the minted bearer token off stdout, including
// out of the .mcp.json wiring that embeds it.
//
// Governing: SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling", SPEC-0007 (one-time
// reveal), SPEC-0016 REQ "Credential Lifetime".
//
// @joestump-agent 10/22/2026 - Added for stump.wtf/switchboard#584.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var mintedToken = "sbk_" + strings.Repeat("x", 40)

func TestVendScopeFlagsTravel(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))

	code := tc.run(t, "endpoint", "vend", "router", "--source-types", "github, gitea",
		"--allowed-queues", "inbox,reviews", "--webhook-max", "3", "--ttl", "7d")
	if code != exitOK {
		t.Fatalf("endpoint vend: code %d, stderr %q", code, tc.stderr.String())
	}
	got := firstVend(t, f)
	wantSources := []string{"github", "gitea"}
	if !slices.Equal(stringSlice(got["source_types"]), wantSources) {
		t.Errorf("source_types = %v, want %v", got["source_types"], wantSources)
	}
	wantQueues := []string{"inbox", "reviews"}
	if !slices.Equal(stringSlice(got["allowed_queues"]), wantQueues) {
		t.Errorf("allowed_queues = %v, want %v", got["allowed_queues"], wantQueues)
	}
	if got["webhook_max"] != float64(3) {
		t.Errorf("webhook_max = %v, want 3", got["webhook_max"])
	}
	if got["ttl"] != "7d" {
		t.Errorf("ttl = %v, want 7d", got["ttl"])
	}
}

func TestVendFromFileWithFlagOverrides(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	path := writeScopeFile(t, `{"name":"from-file","queue":"router-in","verbs":["list_todos","claim_next"],
		"source_types":["gitea"],"allowed_queues":["router-in"],"webhook_max":2,"ttl":"24h",
		"default_lease_ttl_seconds":900,"replay_targets":["https://example.test/replay"]}`)

	if code := tc.run(t, "endpoint", "vend", "--from-file", path, "--webhook-max", "1"); code != exitOK {
		t.Fatalf("endpoint vend --from-file: code %d, stderr %q", code, tc.stderr.String())
	}
	got := firstVend(t, f)
	if got["name"] != "from-file" || got["queue"] != "router-in" {
		t.Errorf("name/queue = %v/%v, want the file's", got["name"], got["queue"])
	}
	if got["webhook_max"] != float64(1) {
		t.Errorf("webhook_max = %v, want the flag's 1 over the file's 2", got["webhook_max"])
	}
	if got["ttl"] != "24h" || got["default_lease_ttl_seconds"] != float64(900) {
		t.Errorf("ttl/lease = %v/%v, want the file's 24h and 900", got["ttl"], got["default_lease_ttl_seconds"])
	}
	verbs := stringSlice(got["verbs"])
	if !slices.Contains(verbs, "list_todos") || !slices.Contains(verbs, "claim_next") {
		t.Errorf("verbs = %v, want the file's grant", verbs)
	}
	if slices.Contains(verbs, "create_webhook") {
		t.Errorf("verbs = %v, want only what the file named", verbs)
	}
	if !slices.Equal(stringSlice(got["replay_targets"]), []string{"https://example.test/replay"}) {
		t.Errorf("replay_targets = %v, want the file's", got["replay_targets"])
	}

	// The positional name beats the file, which is how a checked-in scope gets reused per lane.
	f.mu.Lock()
	f.vends = nil
	f.mu.Unlock()
	if code := tc.run(t, "endpoint", "vend", "second-lane", "--from-file", path); code != exitOK {
		t.Fatalf("endpoint vend NAME --from-file: code %d, stderr %q", code, tc.stderr.String())
	}
	if got := firstVend(t, f); got["name"] != "second-lane" {
		t.Errorf("name = %v, want the positional argument over the file's", got["name"])
	}
}

func TestVendScopeRefusedBeforeAnythingIsMinted(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))

	for _, tcase := range []struct {
		args []string
		want string
	}{
		{[]string{"--source-types", "carrier-pigeon"}, "not supported"},
		{[]string{"--allowed-queues", "quarantine"}, "quarantine"},
		{[]string{"--webhook-max", "-1"}, "not be negative"},
		{[]string{"--ttl", "1.5d"}, "lifetime"},
		{[]string{"--ttl", "0"}, "positive"},
	} {
		args := append([]string{"endpoint", "vend", "router"}, tcase.args...)
		if code := tc.run(t, args...); code == exitOK {
			t.Errorf("%v: exit OK, want a usage error", tcase.args)
		}
		if !strings.Contains(tc.stderr.String(), tcase.want) {
			t.Errorf("%v: stderr %q, want it to say %q", tcase.args, tc.stderr.String(), tcase.want)
		}
	}
	f.mu.Lock()
	mints := len(f.vends)
	f.mu.Unlock()
	if mints != 0 {
		t.Errorf("%d vends reached the API for scopes the wizard would refuse", mints)
	}
}

func TestVendFromFileRefusesAnUnknownKey(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	path := writeScopeFile(t, `{"name":"router","sourse_types":["github"]}`)

	if code := tc.run(t, "endpoint", "vend", "--from-file", path); code == exitOK {
		t.Fatal("a scope file with an unknown key vended an endpoint")
	}
	if !strings.Contains(tc.stderr.String(), "sourse_types") {
		t.Errorf("stderr %q, want the unknown key named", tc.stderr.String())
	}
	f.mu.Lock()
	mints := len(f.vends)
	f.mu.Unlock()
	if mints != 0 {
		t.Errorf("%d vends reached the API for a file with a typo in it", mints)
	}
}

func TestVendSecretFileKeepsTheTokenOffStdout(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	path := filepath.Join(t.TempDir(), "private", "endpoint.token")

	if code := tc.run(t, "endpoint", "vend", "router", "--secret-file", path); code != exitOK {
		t.Fatalf("endpoint vend --secret-file: code %d, stderr %q", code, tc.stderr.String())
	}
	if strings.Contains(tc.stdout.String(), mintedToken) {
		t.Errorf("stdout carries the bearer token:\n%s", tc.stdout.String())
	}
	mustContain(t, "vend with a file", tc.stdout.String(), "written to "+path, "mode 0600")
	assertSecretFile(t, path, []string{mintedToken})
}

func TestVendEnvFileCarriesURLAndToken(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	path := filepath.Join(t.TempDir(), "endpoint.env")

	if code := tc.run(t, "endpoint", "vend", "router", "--env-file", path); code != exitOK {
		t.Fatalf("endpoint vend --env-file: code %d, stderr %q", code, tc.stderr.String())
	}
	if strings.Contains(tc.stdout.String(), mintedToken) {
		t.Errorf("stdout carries the bearer token:\n%s", tc.stdout.String())
	}
	got := strings.Join(assertSecretFile(t, path, nil), "\n")
	if !strings.Contains(got, "SWITCHBOARD_ENDPOINT_TOKEN="+mintedToken) || !strings.Contains(got, "SWITCHBOARD_MCP_URL=http") {
		t.Errorf("env file = %q, want both the MCP URL and the token as KEY=VALUE lines", got)
	}
}

func TestVendJSONWithSecretFileRedactsBothCopies(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	path := filepath.Join(t.TempDir(), "endpoint.token")

	if code := tc.run(t, "endpoint", "vend", "router", "--json", "--secret-file", path); code != exitOK {
		t.Fatalf("endpoint vend --json: code %d, stderr %q", code, tc.stderr.String())
	}
	out := tc.stdout.String()
	if strings.Contains(out, mintedToken) {
		t.Errorf("--json printed the token it was told to put in a file:\n%s", out)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, out)
	}
	if doc["slug"] == nil || doc["slug"] == "" {
		t.Errorf("--json body = %v, want the vend document with the token removed", doc)
	}
	assertSecretFile(t, path, []string{mintedToken})
}

func TestVendSecretFlagsAreMutuallyExclusive(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	dir := t.TempDir()

	if code := tc.run(t, "endpoint", "vend", "router", "--secret-file", filepath.Join(dir, "a"), "--env-file", filepath.Join(dir, "b")); code == exitOK {
		t.Fatal("both destination flags vended an endpoint")
	}
	if !strings.Contains(tc.stderr.String(), "mutually exclusive") {
		t.Errorf("stderr %q, want the mutual-exclusion refusal", tc.stderr.String())
	}
	f.mu.Lock()
	mints := len(f.vends)
	f.mu.Unlock()
	if mints != 0 {
		t.Errorf("%d vends reached the API for a refused flag combination", mints)
	}
}

// writeScopeFile puts a scope document in a temp file and returns its path.
func writeScopeFile(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scope.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write scope file: %v", err)
	}
	return path
}

// firstVend returns the body of the only vend the fake saw.
func firstVend(t *testing.T, f *fakeDeployment) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.vends) != 1 {
		t.Fatalf("vends = %d, want exactly 1", len(f.vends))
	}
	return f.vends[0]
}

func stringSlice(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}

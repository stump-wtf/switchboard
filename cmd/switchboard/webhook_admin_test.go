package main

// The Webhook Lifecycle Verbs against the fake deployment: what each verb sends, where the one-time
// secret ends up, and the usage mistakes that must never reach the API. The secret tests are the point
// of the file: a create or rotate that names a destination file must leave no secret on stdout, in the
// text reveal or in the --json body, and the file that holds it must be readable only by its owner.
//
// Governing: SPEC-0035 REQ "CLI Parity", REQ "Webhook Management"; SPEC-0007 (one-time reveal).
//
// @joestump-agent 10/22/2026 - Added with webhook create/rotate/delete (#584).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const mintedSecret = "whsec_mintedonce"

// seedRouter gives the fake one endpoint of the operator's own, so a slug resolves on the lifecycle
// routes and an id can be sent as given.
func seedRouter(f *fakeDeployment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpointRows = []map[string]any{
		{"id": routeOwnerID, "slug": "cairn-handoff-router-1363fe15", "agent_name": "router", "state": "active"},
	}
}

func TestWebhookCreateBySlugRevealsSecretOnce(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	seedRouter(f)

	if code := tc.run(t, "webhook", "create", "cairn-handoff-router-1363fe15", "--source", "github", "-q", "inbox"); code != exitOK {
		t.Fatalf("webhook create: code %d, stderr %q", code, tc.stderr.String())
	}
	mustContain(t, "create reveal", tc.stdout.String(), "Created github webhook wh-new, delivering to queue inbox",
		"Signing secret: "+mintedSecret, "shown ONCE")

	f.mu.Lock()
	creates, ref := slices.Clone(f.webhookCreates), f.createdRef
	f.mu.Unlock()
	if len(creates) != 1 {
		t.Fatalf("create calls = %d, want 1", len(creates))
	}
	if ref != routeOwnerID {
		t.Errorf("create targeted %q, want the id the slug resolves to", ref)
	}
	if creates[0]["source_type"] != "github" || creates[0]["target_queue"] != "inbox" {
		t.Errorf("create body = %v, want source_type github and target_queue inbox", creates[0])
	}
}

func TestWebhookCreateSecretFileIsPrivateAndSilent(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	seedRouter(f)
	path := filepath.Join(t.TempDir(), "private", "webhook.secret")

	if code := tc.run(t, "webhook", "create", routeOwnerID, "--source", "gitea", "--queue", "inbox", "--secret-file", path); code != exitOK {
		t.Fatalf("webhook create: code %d, stderr %q", code, tc.stderr.String())
	}
	if strings.Contains(tc.stdout.String(), mintedSecret) {
		t.Errorf("stdout carries the secret:\n%s", tc.stdout.String())
	}
	mustContain(t, "create with a file", tc.stdout.String(), "Signing secret written to "+path, "mode 0600")
	assertSecretFile(t, path, []string{mintedSecret})
}

func TestWebhookCreateEnvFileCarriesURLAndSecret(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	seedRouter(f)
	path := filepath.Join(t.TempDir(), "webhook.env")

	if code := tc.run(t, "webhook", "create", routeOwnerID, "--source", "gitea", "--queue", "inbox", "--env-file", path); code != exitOK {
		t.Fatalf("webhook create: code %d, stderr %q", code, tc.stderr.String())
	}
	if strings.Contains(tc.stdout.String(), mintedSecret) {
		t.Errorf("stdout carries the secret:\n%s", tc.stdout.String())
	}
	got := strings.Join(assertSecretFile(t, path, nil), "\n")
	if !strings.Contains(got, "SWITCHBOARD_WEBHOOK_SECRET="+mintedSecret) || !strings.Contains(got, "SWITCHBOARD_WEBHOOK_URL=http") {
		t.Errorf("env file = %q, want both the ingest URL and the secret as KEY=VALUE lines", got)
	}
}

// TestWebhookCreateProviderOriginSecretFileNamesItsContents pins the kindness nit: a --secret-file on a
// provider-origin source (stripe) can only ever hold the ingest URL, because the producer signs with a
// secret only it issued. The file is still written (the URL is what the producer setup needs next), but
// stderr says what the file actually holds, and stdout never claims a secret went into it.
func TestWebhookCreateProviderOriginSecretFileNamesItsContents(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	seedRouter(f)
	path := filepath.Join(t.TempDir(), "webhook.secret")

	if code := tc.run(t, "webhook", "create", routeOwnerID, "--source", "stripe", "--queue", "inbox", "--secret-file", path); code != exitOK {
		t.Fatalf("webhook create: code %d, stderr %q", code, tc.stderr.String())
	}
	if strings.Contains(tc.stdout.String(), "Signing secret written to") {
		t.Errorf("stdout claims a secret was written:\n%s", tc.stdout.String())
	}
	mustContain(t, "provider-origin note", tc.stderr.String(),
		"holds the ingest URL, not a signing secret", "stripe")
	got := strings.Join(assertSecretFile(t, path, nil), "\n")
	if !strings.Contains(got, "/webhooks/w/token123") || strings.Contains(got, mintedSecret) {
		t.Errorf("secret file = %q, want the ingest URL and no secret", got)
	}
}

func TestWebhookSecretFlagsAreMutuallyExclusive(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	seedRouter(f)
	dir := t.TempDir()

	code := tc.run(t, "webhook", "create", routeOwnerID, "--source", "gitea", "--queue", "inbox",
		"--secret-file", filepath.Join(dir, "a.secret"), "--env-file", filepath.Join(dir, "b.env"))
	if code == exitOK {
		t.Fatal("both destination flags accepted")
	}
	if !strings.Contains(tc.stderr.String(), "mutually exclusive") {
		t.Errorf("stderr = %q, want the mutual-exclusion refusal", tc.stderr.String())
	}
	f.mu.Lock()
	calls := len(f.webhookCreates)
	f.mu.Unlock()
	if calls != 0 {
		t.Errorf("the API was called %d times for a refused flag combination", calls)
	}
}

func TestWebhookCreateJSONWithSecretFileRedacts(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	seedRouter(f)
	path := filepath.Join(t.TempDir(), "webhook.secret")

	if code := tc.run(t, "webhook", "create", routeOwnerID, "--source", "gitea", "--queue", "inbox", "--json", "--secret-file", path); code != exitOK {
		t.Fatalf("webhook create --json: code %d, stderr %q", code, tc.stderr.String())
	}
	out := tc.stdout.String()
	if strings.Contains(out, mintedSecret) {
		t.Errorf("--json printed the secret it was told to put in a file:\n%s", out)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("--json output is not JSON: %v\n%s", err, out)
	}
	if doc["webhook_id"] != "wh-new" {
		t.Errorf("--json body = %v, want the webhook document with the secret removed", doc)
	}
	assertSecretFile(t, path, []string{mintedSecret})
}

func TestWebhookCreateCeilingRefusalTravels(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	seedRouter(f)
	f.mu.Lock()
	f.createStatus = 409
	f.mu.Unlock()

	if code := tc.run(t, "webhook", "create", routeOwnerID, "--source", "github", "--queue", "inbox"); code == exitOK {
		t.Fatal("a ceiling refusal reported success")
	}
	if !strings.Contains(tc.stderr.String(), "ceiling") {
		t.Errorf("stderr = %q, want the API's ceiling refusal", tc.stderr.String())
	}
}

func TestWebhookRotatePrintsNewSecretOrWritesIt(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))

	if code := tc.run(t, "webhook", "rotate", hookID); code != exitOK {
		t.Fatalf("webhook rotate: code %d, stderr %q", code, tc.stderr.String())
	}
	mustContain(t, "rotate reveal", tc.stdout.String(), "Rotated github webhook "+hookID, "Signing secret: whsec_rotated")
	f.mu.Lock()
	rotated := slices.Clone(f.rotateCalls)
	f.mu.Unlock()
	if !slices.Contains(rotated, hookID) {
		t.Errorf("rotate calls = %v, want %s", rotated, hookID)
	}

	path := filepath.Join(t.TempDir(), "rotated.secret")
	if code := tc.run(t, "webhook", "rotate", hookID, "--secret-file", path); code != exitOK {
		t.Fatalf("webhook rotate --secret-file: code %d, stderr %q", code, tc.stderr.String())
	}
	if strings.Contains(tc.stdout.String(), "whsec_rotated") {
		t.Errorf("stdout carries the rotated secret:\n%s", tc.stdout.String())
	}
	assertSecretFile(t, path, []string{"whsec_rotated"})
}

func TestWebhookDeleteAsksUnlessForced(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))

	if code := tc.run(t, "webhook", "delete", hookID); code != exitOK {
		t.Fatalf("webhook delete without an answer: code %d, stderr %q", code, tc.stderr.String())
	}
	mustContain(t, "delete prompt", tc.stderr.String(), "Delete webhook "+hookID+"?", "[y/N]")
	f.mu.Lock()
	deletes := slices.Clone(f.deleteCalls)
	f.mu.Unlock()
	if len(deletes) != 0 {
		t.Fatalf("an unanswered delete reached the API: %v", deletes)
	}

	if code := tc.run(t, "webhook", "delete", hookID, "--force"); code != exitOK {
		t.Fatalf("webhook delete --force: code %d, stderr %q", code, tc.stderr.String())
	}
	mustContain(t, "delete output", tc.stdout.String(), "Deleted webhook "+hookID, "webhook create")
	f.mu.Lock()
	deletes = slices.Clone(f.deleteCalls)
	f.mu.Unlock()
	if !slices.Contains(deletes, hookID) {
		t.Errorf("delete calls = %v, want %s", deletes, hookID)
	}
}

func TestWebhookLifecycleUsageErrors(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	seedRouter(f)

	for _, tcase := range []struct {
		name string
		args []string
		want string
	}{
		{"create without an endpoint", []string{"webhook", "create"}, "an endpoint is required"},
		{"create without a source", []string{"webhook", "create", routeOwnerID, "--queue", "inbox"}, "--source is required"},
		{"create without a queue", []string{"webhook", "create", routeOwnerID, "--source", "github"}, "--queue is required"},
		{"create with an unknown endpoint", []string{"webhook", "create", "no-such-slug", "--source", "github", "--queue", "inbox"}, "no endpoint of yours"},
		{"rotate without an id", []string{"webhook", "rotate"}, "a webhook id is required"},
		{"delete without an id", []string{"webhook", "delete"}, "a webhook id is required"},
	} {
		if code := tc.run(t, tcase.args...); code == exitOK {
			t.Errorf("%s: exit OK, want a usage error", tcase.name)
		}
		if !strings.Contains(tc.stderr.String(), tcase.want) {
			t.Errorf("%s: stderr %q, want it to say %q", tcase.name, tc.stderr.String(), tcase.want)
		}
	}
	f.mu.Lock()
	calls := len(f.webhookCreates) + len(f.rotateCalls) + len(f.deleteCalls)
	f.mu.Unlock()
	if calls != 0 {
		t.Errorf("%d lifecycle writes reached the API for refused arguments", calls)
	}
}

// assertSecretFile checks the mode and contents of a file the CLI was told to put a secret in, and
// returns its lines. The mode is the assertion that matters: a secret file the group or other can read
// has leaked the credential whatever the command printed.
func assertSecretFile(t *testing.T, path string, wantLines []string) []string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("secret file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("secret file %s mode is %o, want 600", path, perm)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read secret file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, want := range wantLines {
		if !slices.Contains(lines, want) {
			t.Errorf("secret file lines = %q, want %q", lines, want)
		}
	}
	return lines
}

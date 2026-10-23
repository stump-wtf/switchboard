package main

// The scope verbs against the fake deployment: the preview is shown before anything is applied, the
// confirmation is a second call rather than a guess, --yes skips the prompt without skipping the
// preview, and a widen the operator declines sends no write at all. Issue #590.
//
// Governing: ADR-0049 (proposed), SPEC-0035 REQ "CLI Parity".
//
// @joestump-agent 10/21/2026 - Added for stump.wtf/switchboard#590.

import (
	"strings"
	"testing"
	"time"
)

func TestEndpointScopeWidenPreviewsThenApplies(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	f.mu.Lock()
	f.widenQueues = []string{"lane-a"}
	f.mu.Unlock()

	if code := tc.run(t, "endpoint", "scope", "widen", "lane-a-ep", "--add-queue", "merge"); code != exitOK {
		t.Fatalf("widen: code %d, stdout %q, stderr %q", code, tc.stdout.String(), tc.stderr.String())
	}
	out := tc.stdout.String()
	for _, want := range []string{"webhook_queues", "was", "now", "lane-a", "merge", "Apply this widen?"} {
		if !strings.Contains(out, want) {
			t.Errorf("confirmation output is missing %q: %q", want, out)
		}
	}
	// The prompt was declined (EOF on a closed stdin), so nothing was applied.
	f.mu.Lock()
	calls := len(f.widens)
	queues := strings.Join(f.widenQueues, ",")
	f.mu.Unlock()
	if calls != 1 {
		t.Fatalf("a declined widen sent %d calls, want the preview only", calls)
	}
	if queues != "lane-a" {
		t.Fatalf("a declined widen changed the grant: %q", queues)
	}
	if !strings.Contains(tc.stdout.String(), "Nothing changed") {
		t.Errorf("a declined widen does not say so: %q", tc.stdout.String())
	}
}

func TestEndpointScopeWidenYesAppliesAfterShowing(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	f.mu.Lock()
	f.widenQueues = []string{"lane-a"}
	f.mu.Unlock()

	if code := tc.run(t, "endpoint", "scope", "widen", "lane-a-ep", "--add-queue", "merge,lane-b", "--yes"); code != exitOK {
		t.Fatalf("widen --yes: code %d, stderr %q", code, tc.stderr.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.widens) != 2 {
		t.Fatalf("widen --yes sent %d calls, want a preview and an apply", len(f.widens))
	}
	if confirm, _ := f.widens[0]["confirm"].(bool); confirm {
		t.Errorf("the first call confirmed: %v", f.widens[0])
	}
	if confirm, _ := f.widens[1]["confirm"].(bool); !confirm {
		t.Errorf("the second call did not confirm: %v", f.widens[1])
	}
	got := strings.Join(f.widenQueues, " ")
	if !strings.Contains(got, "merge") || !strings.Contains(got, "lane-b") || !strings.Contains(got, "lane-a") {
		t.Fatalf("grant after widen --yes = %q, want lane-a kept and both queues added", got)
	}
	if !strings.Contains(tc.stdout.String(), "credential, ingest URLs and signing secrets are unchanged") {
		t.Errorf("the apply does not say the credential is untouched: %q", tc.stdout.String())
	}
}

// add-queue is the one-flag shape of the same call, and "y" at the prompt is the second call.
func TestEndpointScopeAddQueueConfirmsInteractively(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	f.mu.Lock()
	f.widenQueues = []string{"lane-a"}
	f.mu.Unlock()
	tc.stdin = strings.NewReader("y\n")

	if code := tc.run(t, "endpoint", "scope", "add-queue", "lane-a-ep", "merge"); code != exitOK {
		t.Fatalf("add-queue: code %d, stderr %q", code, tc.stderr.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.widens) != 2 {
		t.Fatalf("add-queue sent %d calls, want a preview and an apply", len(f.widens))
	}
	if ref, _ := f.widens[0]["ref"].(string); ref != "lane-a-ep" {
		t.Errorf("widen targeted %q, want lane-a-ep", ref)
	}
	if queues, ok := f.widens[0]["add_webhook_queues"].([]any); !ok || len(queues) != 1 || queues[0] != "merge" {
		t.Errorf("add_webhook_queues = %v, want [merge]", f.widens[0]["add_webhook_queues"])
	}
}

// A usage mistake never reaches the deployment.
func TestEndpointScopeUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"endpoint", "scope", "widen"},
		{"endpoint", "scope", "widen", "lane-a-ep"},
		{"endpoint", "scope", "widen", "lane-a-ep", "extra", "--add-queue", "merge"},
		{"endpoint", "scope", "add-queue", "lane-a-ep"},
		{"endpoint", "scope", "add-queue", "lane-a-ep", "merge", "too-many"},
		{"endpoint", "scope", "history"},
		{"endpoint", "scope", "history", "lane-a-ep", "too-many"},
	} {
		tc := newTestCLI(t)
		f := newFakeDeployment(t)
		tc.loggedIn(t, f, tc.clock.Add(time.Hour))
		if code := tc.run(t, args...); code == exitOK {
			t.Errorf("%v exited fine: %q", args, tc.stdout.String())
		}
		f.mu.Lock()
		calls := len(f.widens)
		f.mu.Unlock()
		if calls != 0 {
			t.Errorf("%v reached the API %d times", args, calls)
		}
	}
}

// The server's refusal travels: a widen that would narrow the ceiling fails with the server's own
// message rather than looking like it worked.
func TestEndpointScopeWidenSurfacesRefusal(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	f.mu.Lock()
	f.widenStatus = 400
	f.mu.Unlock()

	if code := tc.run(t, "endpoint", "scope", "widen", "lane-a-ep", "--webhook-max", "1", "--yes"); code == exitOK {
		t.Fatalf("a refused widen exited fine: %q / %q", tc.stdout.String(), tc.stderr.String())
	}
	if !strings.Contains(tc.stderr.String(), "widening only") {
		t.Errorf("the server's refusal is missing: %q", tc.stderr.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.widens) != 1 {
		t.Fatalf("a refused preview was followed by an apply: %d calls", len(f.widens))
	}
}

// The trail is readable: history prints what each widening replaced, so "when did this lane get that
// route" is one command rather than a query.
func TestEndpointScopeHistoryPrintsTheTrail(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))

	if code := tc.run(t, "endpoint", "scope", "add-queue", "lane-a-ep", "merge", "--yes"); code != exitOK {
		t.Fatalf("add-queue exited %d: %q", code, tc.stderr.String())
	}
	tc.stdout.Reset()
	if code := tc.run(t, "endpoint", "scope", "history", "lane-a-ep"); code != exitOK {
		t.Fatalf("history exited %d: %q", code, tc.stderr.String())
	}
	out := tc.stdout.String()
	for _, want := range []string{"webhook_queues", "was", "merge", "2026-10-21T12:00:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("history is missing %q: %q", want, out)
		}
	}
}

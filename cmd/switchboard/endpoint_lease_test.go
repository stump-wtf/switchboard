package main

// The CLI's default-lease surface (ADR-0043): `endpoint vend --lease-ttl`, `endpoint edit
// --lease-ttl DUR|default`, and the LEASE column of `endpoint list`. DUR is a Go duration or bare
// seconds, converted to seconds here; the bounds are the server's, so an out-of-range value is sent
// and the server's refusal is printed.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestVendSendsTheLeaseTTLInSeconds(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))

	for i, c := range []struct {
		flag string
		want float64
	}{{"1h", 3600}, {"45m", 2700}, {"3600", 3600}, {"90000", 90000}} {
		if code := tc.run(t, "endpoint", "vend", "slow-bot", "--lease-ttl", c.flag); code != exitOK {
			t.Fatalf("vend --lease-ttl %s: code %d, stderr %q", c.flag, code, tc.stderr.String())
		}
		if got := f.vends[i]["default_lease_ttl_seconds"]; got != c.want {
			t.Fatalf("vend --lease-ttl %s sent %v, want %v", c.flag, got, c.want)
		}
	}
	mustContain(t, "vend reveal", tc.stdout.String(), "Lease", "25h (90000s)")

	// Without the flag nothing is sent: the server default applies.
	if code := tc.run(t, "endpoint", "vend", "quick-bot"); code != exitOK {
		t.Fatalf("vend: code %d", code)
	}
	if _, ok := f.vends[len(f.vends)-1]["default_lease_ttl_seconds"]; ok {
		t.Fatalf("vend without --lease-ttl sent one: %v", f.vends[len(f.vends)-1])
	}

	// An unparseable value is a usage mistake that never reaches the API.
	before := len(f.vends)
	for _, bad := range []string{"soon", "1.5s", "1d"} {
		if code := tc.run(t, "endpoint", "vend", "x", "--lease-ttl", bad); code != exitUsage {
			t.Errorf("--lease-ttl %s: code %d, want %d", bad, code, exitUsage)
		}
		mustContain(t, "vend usage", tc.stderr.String(), "--lease-ttl", "usage: switchboard endpoint vend")
	}
	if len(f.vends) != before {
		t.Fatalf("a usage mistake reached the API: %v", f.vends[before:])
	}
}

func TestEndpointEditSetsAndResetsTheLease(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))

	if code := tc.run(t, "endpoint", "edit", "slow-bot-ab12cd34", "--lease-ttl", "1h"); code != exitOK {
		t.Fatalf("edit: code %d, stderr %q", code, tc.stderr.String())
	}
	if len(f.edits) != 1 || f.edits[0]["ref"] != "slow-bot-ab12cd34" || f.edits[0]["default_lease_ttl_seconds"] != float64(3600) {
		t.Fatalf("edit bodies = %v", f.edits)
	}
	mustContain(t, "edit output", tc.stdout.String(), "Updated slow-bot-ab12cd34", "default lease 1h (3600s)",
		"leases already granted keep their expiry")

	// "default" sends an explicit null, the reset; flags may precede the slug.
	if code := tc.run(t, "endpoint", "edit", "--lease-ttl", "default", "slow-bot-ab12cd34"); code != exitOK {
		t.Fatalf("edit default: code %d, stderr %q", code, tc.stderr.String())
	}
	if v, ok := f.edits[1]["default_lease_ttl_seconds"]; !ok || v != nil {
		t.Fatalf("--lease-ttl default sent %v (present %v), want an explicit null", v, ok)
	}
	mustContain(t, "edit default output", tc.stdout.String(), "5m (300s) (server default)")

	// --json prints the API document verbatim.
	if code := tc.run(t, "endpoint", "edit", "slow-bot-ab12cd34", "--lease-ttl", "2700", "--json"); code != exitOK {
		t.Fatalf("edit --json: code %d", code)
	}
	var doc map[string]any
	if err := json.Unmarshal(tc.stdout.Bytes(), &doc); err != nil || doc["default_lease_ttl_seconds"] != float64(2700) {
		t.Fatalf("--json output = %q (%v)", tc.stdout.String(), err)
	}
}

func TestEndpointEditUsageMistakesAndServerRefusal(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	for _, args := range [][]string{
		{"endpoint", "edit"},                                   // no endpoint
		{"endpoint", "edit", "a"},                              // nothing to change
		{"endpoint", "edit", "a", "b", "--lease-ttl", "1h"},    // two endpoints
		{"endpoint", "edit", "a", "--lease-ttl", "soon"},       // not a duration
		{"endpoint", "edit", "a", "--queue", "q"},              // scope is not editable
		{"endpoint", "edit", "a", "--lease-ttl", "1h", "--no"}, // unknown flag
	} {
		if code := tc.run(t, args...); code != exitUsage {
			t.Errorf("%v: code %d, want %d (stderr %q)", args, code, exitUsage, tc.stderr.String())
		}
		mustContain(t, "edit usage", tc.stderr.String(), "usage: switchboard endpoint edit")
	}
	if len(f.edits) != 0 {
		t.Fatalf("a usage mistake reached the API: %v", f.edits)
	}

	// The bounds are the server's: an out-of-range value is sent, and its refusal printed.
	f.mu.Lock()
	f.editStatus = 400
	f.mu.Unlock()
	if code := tc.run(t, "endpoint", "edit", "a", "--lease-ttl", "59"); code != exitFailure {
		t.Fatalf("server refusal: code %d, want %d", code, exitFailure)
	}
	if len(f.edits) != 1 || f.edits[0]["default_lease_ttl_seconds"] != float64(59) {
		t.Fatalf("edit bodies = %v", f.edits)
	}
	mustContain(t, "edit refusal", tc.stderr.String(), "400 Bad Request", "from 60 to 86400", "(invalid_argument)")
}

func TestEndpointListShowsTheLease(t *testing.T) {
	tc := newTestCLI(t)
	f := newFakeDeployment(t)
	tc.loggedIn(t, f, tc.clock.Add(time.Hour))
	f.mu.Lock()
	f.endpointRows = []map[string]any{
		{"slug": "slow-bot-ab12cd34", "agent_name": "slow-bot", "state": "active", "queues": []string{"reviews"},
			"default_lease_ttl_seconds": 3600, "effective_lease_ttl_seconds": 3600},
		{"slug": "quick-bot-ffffffff", "agent_name": "quick-bot", "state": "active", "queues": []string{"inbox"},
			"default_lease_ttl_seconds": nil, "effective_lease_ttl_seconds": 300},
		{"slug": "old-server-00000000", "agent_name": "old", "state": "active", "queues": []string{"inbox"}},
	}
	f.mu.Unlock()
	if code := tc.run(t, "endpoint", "list"); code != exitOK {
		t.Fatalf("list: code %d, stderr %q", code, tc.stderr.String())
	}
	out := tc.stdout.String()
	mustContain(t, "endpoint list", out, "LEASE", "1h (3600s)", "5m (300s) (server default)")
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "old-server-00000000") && !strings.HasSuffix(strings.TrimSpace(line), "-") {
			t.Errorf("a row from a server without lease fields should show -, got %q", line)
		}
	}
}

func TestHelpListsEndpointEdit(t *testing.T) {
	tc := newTestCLI(t)
	if code := tc.run(t, "help"); code != exitOK {
		t.Fatalf("help: code %d", code)
	}
	mustContain(t, "help", tc.stdout.String(), "endpoint edit SLUG|ID", "default claim lease")
	if code := tc.run(t, "endpoint", "edit", "-h"); code != exitOK {
		t.Fatalf("edit -h: code %d", code)
	}
	mustContain(t, "edit -h", tc.stdout.String(), "usage: switchboard endpoint edit [flags] SLUG|ID", "-lease-ttl", `"default" resets it`)
	if code := tc.run(t, "endpoint", "vend", "-h"); code != exitOK {
		t.Fatalf("vend -h: code %d", code)
	}
	mustContain(t, "vend -h", tc.stdout.String(), "-lease-ttl", "60 to 86400 seconds")
}

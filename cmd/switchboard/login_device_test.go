package main

// The device login (--device, RFC 8628) against the fake deployment: discovery → registration →
// device_authorization → poll through authorization_pending → the same operator-bound pair the
// loopback flow stores, usable on /api/v1; denial surfaces as a failed login with nothing saved;
// and a deployment that advertises no device endpoint says so plainly instead of hanging.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestLoginDeviceApprovesAndSavesCredentials(t *testing.T) {
	f := newFakeDeployment(t)
	tc := newTestCLI(t)
	tc.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

	if code := tc.run(t, "login", "--device", f.base()); code != exitOK {
		t.Fatalf("login --device exit=%d stderr=%s stdout=%s", code, tc.stderr, tc.stdout)
	}
	mustContain(t, "login output", tc.stdout.String(), "VKTR-JWPF", f.base()+"/oauth/device")

	f.mu.Lock()
	if len(f.deviceForms) != 1 || f.deviceForms[0].Get("client_id") != "cid-test" ||
		f.deviceForms[0].Get("resource") != f.base()+"/api" {
		t.Fatalf("device_authorization forms: %+v", f.deviceForms)
	}
	if len(f.tokenCalls) < 2 {
		t.Fatalf("want at least one pending poll plus the issuing one, got %d", len(f.tokenCalls))
	}
	for _, form := range f.tokenCalls {
		if form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
			t.Fatalf("poll carried grant_type %q", form.Get("grant_type"))
		}
	}
	if got := f.tokenCalls[len(f.tokenCalls)-1].Get("device_code"); got == "" || f.deviceCodes[got] != "spent" {
		t.Fatalf("winning poll device_code %q not marked spent", got)
	}
	f.mu.Unlock()

	// The stored pair works on the API without another exchange.
	if code := tc.run(t, "endpoint", "list"); code != exitOK {
		t.Fatalf("endpoint list after device login exit=%d stderr=%s", code, tc.stderr)
	}
}

func TestLoginDeviceDenied(t *testing.T) {
	f := newFakeDeployment(t)
	f.mu.Lock()
	f.deviceDeny = true
	f.mu.Unlock()
	tc := newTestCLI(t)
	tc.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

	if code := tc.run(t, "login", "--device", f.base()); code == exitOK {
		t.Fatalf("denied device login must fail, stdout=%s", tc.stdout)
	}
	if !strings.Contains(tc.stderr.String(), "denied") {
		t.Fatalf("want a denial the user can read, got %s", tc.stderr)
	}
	if _, err := tc.loadCredentials(); err == nil {
		t.Fatal("a denied login saved credentials")
	}
}

func TestLoginDeviceServerWithoutDeviceEndpoint(t *testing.T) {
	f := newFakeDeployment(t)
	f.mu.Lock()
	f.noDeviceEndpoint = true
	f.mu.Unlock()
	tc := newTestCLI(t)
	tc.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

	if code := tc.run(t, "login", "--device", f.base()); code == exitOK {
		t.Fatal("a deployment with no device_authorization_endpoint must fail the login")
	}
	if !strings.Contains(tc.stderr.String(), "device_authorization_endpoint") {
		t.Fatalf("want the missing advertisement named, got %s", tc.stderr)
	}
}

// The device poll loop answers slow_down by widening the interval, per RFC 8628 — here: the fake
// reports slow_down for the first poll and the CLI must still land the pair.
func TestLoginDeviceSlowDownWidensInterval(t *testing.T) {
	f := newFakeDeployment(t)
	f.mu.Lock()
	f.deviceSlowDown = 1
	f.devicePending = 2
	f.mu.Unlock()
	tc := newTestCLI(t)
	tc.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

	if code := tc.run(t, "login", "--device", f.base()); code != exitOK {
		t.Fatalf("login --device exit=%d stderr=%s", code, tc.stderr)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tokenCalls) < 3 {
		t.Fatalf("slow_down poll expected, got %d calls", len(f.tokenCalls))
	}
}

// discovery tolerates a deployment without the device advertisement: the field is optional in the
// RFC 8414 document, so the loopback login must keep working unchanged.
func TestDiscoveryWithoutDeviceEndpointIsFine(t *testing.T) {
	f := newFakeDeployment(t)
	f.mu.Lock()
	f.noDeviceEndpoint = true
	f.mu.Unlock()
	tc := newTestCLI(t)

	as, err := tc.discover(context.Background(), f.base())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if as.DeviceAuthorizationEndpoint != "" {
		t.Fatalf("unexpected device endpoint %q", as.DeviceAuthorizationEndpoint)
	}
	b, err := json.Marshal(as)
	if err != nil || !strings.Contains(string(b), "token_endpoint") {
		t.Fatalf("metadata decode lost fields: %s %v", b, err)
	}
}

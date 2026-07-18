package switchboard

// The JS quality gate: the shipped sb.js modules (static/js/) carry real behavior — the keymap,
// the theme contract, the wizard step gating — so they are tested BEHAVIORALLY, not just source-
// parsed (keymap_test.go). The suite is plain node:test files under jstest/ against a hand-rolled
// DOM stub (jstest/dom.js): zero JS dependencies, zero build step for shipped assets, run here so
// `go test ./...` — and therefore `make ci` — carries the gate. Without node the gate cannot run;
// that is a LOUD skip, never a silent pass.
// Governing: SPEC-0015 REQ "Global Keyboard Map", REQ "Theme Toggle", REQ "Wizard Interaction
// Pattern"; ADR-0018 (keyboard-first interaction language); ADR-0001 (hand-rolled, no framework).

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestJSModules runs node's built-in test runner over jstest/*.test.js, which load the static/js
// modules unmodified. The test files are enumerated here (rather than a glob or directory arg) so
// the invocation works on every node with `--test` and an empty suite is a failure, not a pass.
// go test runs with the package directory (the repo root) as the working directory, so the
// relative paths hold.
func TestJSModules(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("SKIPPED — node not found in PATH: the sb.js behavioral suite (jstest/) DID NOT RUN. " +
			"Install Node.js 20+ so the JS quality gate executes; this skip must never be treated as a pass.")
	}
	files, err := filepath.Glob("jstest/*.test.js")
	if err != nil {
		t.Fatalf("glob jstest/*.test.js: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no jstest/*.test.js files found — the JS suite has vanished, which must fail the gate")
	}
	out, err := exec.Command(node, append([]string{"--test"}, files...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node --test %v failed: %v\n%s", files, err, out)
	}
	t.Logf("node --test %v passed:\n%s", files, out)
}

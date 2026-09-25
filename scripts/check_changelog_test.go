// Package scripts holds tests for the repository's shell scripts. check-changelog.sh is exercised
// against throwaway git repositories, one per case, so the rules are proven by `go test ./...` in
// CI and locally. Governing: SPEC-0027 REQ-9 "CHANGELOG", REQ-10 "Breaking Changes and Upgrade
// Notes", REQ-11 (the failure message).
package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const baseChangelog = `# Changelog

## [Unreleased]

## [0.3.0] - 2026-09-22

### Added

- The first release under the contract.
`

// fixture is one pull request: files written on top of the base commit, plus its title and labels.
type fixture struct {
	title  string
	labels string
	files  map[string]string
}

// run builds a repository whose main branch holds the base CHANGELOG and upgrade guide, commits the
// fixture's files on a branch, and runs the script's check against main. It returns the exit code
// and the combined output.
func run(t *testing.T, check string, f fixture) (int, string) {
	t.Helper()
	script, err := filepath.Abs("check-changelog.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "test")
	git("config", "commit.gpgsign", "false")
	write("CHANGELOG.md", baseChangelog)
	write("docs/guides/15-upgrading.md", "# Upgrading\n")
	write("internal/db/migrations/0001_init.sql", "CREATE TABLE t (id int);\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	git("checkout", "-q", "-b", "pr")
	for name, body := range f.files {
		write(name, body)
	}
	git("add", "-A")
	git("commit", "-q", "--allow-empty", "-m", f.title)

	cmd := exec.Command("bash", script, check)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PR_TITLE="+f.title, "PR_LABELS="+f.labels, "BASE_REF=main",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &exit):
		return exit.ExitCode(), string(out)
	default:
		t.Fatalf("running the script: %v", err)
		return -1, ""
	}
}

// withUnreleased returns the base CHANGELOG with lines inserted under ## [Unreleased].
func withUnreleased(lines string) string {
	return strings.Replace(baseChangelog, "## [Unreleased]\n", "## [Unreleased]\n\n"+lines, 1)
}

func TestCheckChangelog(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	upgrading := "# Upgrading\n\n## Upgrading to Unreleased\n\nWhat breaks, who, steps, verify.\n"
	breakingEntry := withUnreleased("### Breaking\n\n- Removed X; use Y.\n")
	cases := []struct {
		name  string
		check string
		f     fixture
		want  int
		say   string
	}{
		// REQ-9 scenario "Feature without an entry".
		{"feature without an entry fails", "changelog",
			fixture{title: "feat(mcp): add ack_doorbell"}, 1, "needs a CHANGELOG entry"},
		{"feature with an Unreleased entry passes", "changelog",
			fixture{title: "feat(mcp): add ack_doorbell", files: map[string]string{
				"CHANGELOG.md": withUnreleased("### Added\n\n- `ack_doorbell`.\n")}}, 0, "found a new line"},
		{"an entry under an old release does not count", "changelog",
			fixture{title: "fix: a bug", files: map[string]string{
				"CHANGELOG.md": strings.Replace(baseChangelog, "- The first release", "- Late note.\n- The first release", 1)}}, 1, "needs a CHANGELOG entry"},
		{"a blank line under Unreleased does not count", "changelog",
			fixture{title: "sec: tighten X", files: map[string]string{
				"CHANGELOG.md": withUnreleased("\n")}}, 1, "needs a CHANGELOG entry"},
		{"perf needs an entry", "changelog", fixture{title: "perf(store): faster claim"}, 1, "needs a CHANGELOG entry"},
		// REQ-9 scenario "Dependency bump".
		{"dependency bump passes", "changelog",
			fixture{title: "chore(deps): update module golang.org/x/net to v0.40.0"}, 0, "needs no CHANGELOG entry"},
		{"docs passes", "changelog", fixture{title: "docs: fix a typo"}, 0, "needs no CHANGELOG entry"},
		{"toil passes", "changelog", fixture{title: "toil(web): tidy"}, 0, "needs no CHANGELOG entry"},
		{"test passes", "changelog", fixture{title: "test: cover X"}, 0, "needs no CHANGELOG entry"},
		{"no-changelog label waives", "changelog",
			fixture{title: "fix: a bug", labels: "bug,no-changelog"}, 0, "waived"},
		{"a breaking title needs an entry whatever its type", "changelog",
			fixture{title: "refactor!: rename X"}, 1, "needs a CHANGELOG entry"},
		// REQ-10 scenario "Breaking PR without a note".
		{"breaking PR without a note fails", "upgrade-note",
			fixture{title: "sec!: remove X"}, 1, "15-upgrading.md"},
		{"no-changelog does not waive the upgrade note", "upgrade-note",
			fixture{title: "sec!: remove X", labels: "no-changelog"}, 1, "no label waives this"},
		{"breaking PR with only the upgrade guide fails", "upgrade-note",
			fixture{title: "sec!: remove X", files: map[string]string{"docs/guides/15-upgrading.md": upgrading}}, 1, "### Breaking"},
		{"a Breaking entry outside Unreleased does not count", "upgrade-note",
			fixture{title: "sec!: remove X", files: map[string]string{
				"docs/guides/15-upgrading.md": upgrading,
				"CHANGELOG.md":                withUnreleased("### Added\n\n- X was removed.\n")}}, 1, "### Breaking"},
		{"breaking PR with the guide and a Breaking entry passes", "all",
			fixture{title: "sec!: remove X", files: map[string]string{
				"docs/guides/15-upgrading.md": upgrading, "CHANGELOG.md": breakingEntry}}, 0, "upgrade-note: ok"},
		{"the failure message states REQ-11", "upgrade-note",
			fixture{title: "feat(api)!: drop the v0 route"}, 1, "no alias, shim or"},
		// REQ-10 scenario "Irreversible migration".
		{"irreversible migration without a note fails", "upgrade-note",
			fixture{title: "toil(db): drop adapters", files: map[string]string{
				"internal/db/migrations/0021_drop_adapters.sql": "DROP TABLE adapters;\n"}}, 1, "0021_drop_adapters.sql"},
		{"irreversible migration also needs a CHANGELOG entry", "changelog",
			fixture{title: "toil(db): drop adapters", files: map[string]string{
				"internal/db/migrations/0021_drop_adapters.sql": "delete from providers;\n"}}, 1, "needs a CHANGELOG entry"},
		{"irreversible migration with the guide and a Breaking entry passes", "all",
			fixture{title: "toil(db): drop adapters", files: map[string]string{
				"internal/db/migrations/0021_drop_adapters.sql": "ALTER TABLE t DROP COLUMN x;\n",
				"docs/guides/15-upgrading.md":                   upgrading, "CHANGELOG.md": breakingEntry}}, 0, "upgrade-note: ok"},
		{"an additive migration is not breaking", "all",
			fixture{title: "toil(db): add an index", files: map[string]string{
				"internal/db/migrations/0022_index.sql": "CREATE INDEX i ON t (id);\n"}}, 0, "not a breaking change"},
		{"editing an old migration is not a new destructive one", "upgrade-note",
			fixture{title: "toil(db): comment", files: map[string]string{
				"internal/db/migrations/0001_init.sql": "CREATE TABLE t (id int); -- DROP TABLE t later\n"}}, 0, "not a breaking change"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := run(t, tc.check, tc.f)
			if code != tc.want {
				t.Fatalf("exit %d, want %d\n%s", code, tc.want, out)
			}
			if !strings.Contains(out, tc.say) {
				t.Fatalf("output lacks %q:\n%s", tc.say, out)
			}
		})
	}
}

func TestCheckChangelogUsage(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	cmd := exec.Command("bash", "check-changelog.sh", "bogus")
	cmd.Env = append(os.Environ(), "PR_TITLE=feat: x")
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("want exit 2 on a bad check name, got %v", err)
	}
}

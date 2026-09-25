package buildinfo

import (
	"runtime/debug"
	"testing"
)

// Governing: SPEC-0027 REQ-1 "Build Information", REQ-15 "Error Handling Standards".

func readFrom(bi *debug.BuildInfo, ok bool) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) { return bi, ok }
}

func vcsInfo(mainVersion, rev, when string) *debug.BuildInfo {
	bi := &debug.BuildInfo{Main: debug.Module{Path: "github.com/stump-wtf/switchboard", Version: mainVersion}}
	if rev != "" {
		bi.Settings = append(bi.Settings, debug.BuildSetting{Key: "vcs.revision", Value: rev})
	}
	if when != "" {
		bi.Settings = append(bi.Settings, debug.BuildSetting{Key: "vcs.time", Value: when})
	}
	return bi
}

func TestResolve(t *testing.T) {
	const (
		rev  = "0123456789abcdef0123456789abcdef01234567"
		when = "2026-09-24T10:11:12Z"
	)
	cases := []struct {
		name                  string
		version, commit, date string
		read                  func() (*debug.BuildInfo, bool)
		want                  Info
	}{
		{
			// Scenario "Release build": goreleaser stamps all three; the build info is not consulted.
			name:    "release build: ldflags win",
			version: "v0.3.0", commit: rev, date: when,
			read: readFrom(vcsInfo("v9.9.9", "ffff", "2000-01-01T00:00:00Z"), true),
			want: Info{Version: "v0.3.0", Commit: rev, Date: when},
		},
		{
			// Scenario "go install": no ldflags, the module version is the tag.
			name: "go install @vX: module version",
			read: readFrom(vcsInfo("v0.3.0", "", ""), true),
			want: Info{Version: "v0.3.0"},
		},
		{
			// Scenario "Plain local build": since Go 1.24 a build off a release tag stamps a
			// pseudo-version, which is reported as it is.
			name: "plain local build: vcs pseudo-version plus revision and time",
			read: readFrom(vcsInfo("v0.3.1-0.20260923185201-4369412033b4", rev, when), true),
			want: Info{Version: "v0.3.1-0.20260923185201-4369412033b4", Commit: rev, Date: when},
		},
		{
			// A modified tree adds +dirty; that is reported too, never hidden.
			name: "plain local build: dirty tree",
			read: readFrom(vcsInfo("v0.3.1-0.20260923185201-4369412033b4+dirty", rev, when), true),
			want: Info{Version: "v0.3.1-0.20260923185201-4369412033b4+dirty", Commit: rev, Date: when},
		},
		{
			// A (devel) module with VCS settings: a build where Go could not derive a version.
			name: "(devel) module: dev plus vcs revision and time",
			read: readFrom(vcsInfo("(devel)", rev, when), true),
			want: Info{Version: "dev", Commit: rev, Date: when},
		},
		{
			// Scenario "Build without VCS information": -buildvcs=false leaves (devel) and no
			// settings, so a missed -X stamp reports dev (what release.yaml's assertion relies on).
			name: "-buildvcs=false: dev, no commit or date",
			read: readFrom(vcsInfo("(devel)", "", ""), true),
			want: Info{Version: "dev"},
		},
		{
			name:    "partial stamp: missing fields fall back per field",
			version: "v0.3.0",
			read:    readFrom(vcsInfo("(devel)", rev, when), true),
			want:    Info{Version: "v0.3.0", Commit: rev, Date: when},
		},
		{
			// REQ-15: no build info at all never fails; it reports dev.
			name: "no build info",
			read: readFrom(nil, false),
			want: Info{Version: "dev"},
		},
		{
			name: "nil reader",
			want: Info{Version: "dev"},
		},
		{
			name: "empty module version",
			read: readFrom(vcsInfo("", "", ""), true),
			want: Info{Version: "dev"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolve(tc.version, tc.commit, tc.date, tc.read); got != tc.want {
				t.Fatalf("resolve = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestBuildDayAndBanner(t *testing.T) {
	cases := []struct {
		info       Info
		day        string
		wantBanner string
	}{
		{Info{Version: "v0.3.0", Date: "2026-09-24T10:11:12Z"}, "2026-09-24", "switchboard v0.3.0 (built 2026-09-24)"},
		// git %cI carries the committer's offset; the day is reported in UTC.
		{Info{Version: "v0.3.0", Date: "2026-09-24T22:30:00-07:00"}, "2026-09-25", "switchboard v0.3.0 (built 2026-09-25)"},
		// REQ-2: the parenthetical is omitted when the date is unknown or unparseable.
		{Info{Version: "dev"}, "", "switchboard dev"},
		{Info{Version: "dev", Date: "yesterday"}, "", "switchboard dev"},
	}
	for _, tc := range cases {
		if got := tc.info.BuildDay(); got != tc.day {
			t.Errorf("BuildDay(%q) = %q, want %q", tc.info.Date, got, tc.day)
		}
		if got := tc.info.Banner(); got != tc.wantBanner {
			t.Errorf("Banner(%+v) = %q, want %q", tc.info, got, tc.wantBanner)
		}
	}
}

// TestGetNeverEmpty: whatever this test binary was built with, Get resolves a non-empty Version
// and is stable across calls.
func TestGetNeverEmpty(t *testing.T) {
	a, b := Get(), Get()
	if a.Version == "" {
		t.Fatal("Get().Version is empty")
	}
	if a != b {
		t.Fatalf("Get is not stable: %+v then %+v", a, b)
	}
}

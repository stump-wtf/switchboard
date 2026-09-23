// Package buildinfo is the one place Switchboard learns its own version. Every surface that reports
// the build — MCP serverInfo and session instructions, the CLI, and later /healthz, the web footer
// and the build-info metric — calls Get(), so no other package needs a version threaded through it
// and none may hold a version literal (literal_guard_test.go enforces that for internal/mcp and
// internal/web).
//
// Governing: ADR-0032 (release version reporting and upgrade contract), SPEC-0027 REQ-1 "Build
// Information", REQ-15 "Error Handling Standards"; design.md "internal/buildinfo replaces
// main.version".
package buildinfo

import (
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// The stamp targets. Release builds set them with
//
//	-ldflags "-X github.com/stump-wtf/switchboard/internal/buildinfo.Version=… \
//	          -X github.com/stump-wtf/switchboard/internal/buildinfo.Commit=… \
//	          -X github.com/stump-wtf/switchboard/internal/buildinfo.Date=…"
//
// from the Makefile, deploy/docker/Dockerfile and .goreleaser.yaml. Left empty, Get() fills each
// one from runtime/debug.ReadBuildInfo instead. Read them through Get(), never directly: a
// directly-read Version is "" on a `go install` build that Get() reports correctly.
var (
	Version string
	Commit  string
	Date    string
)

// devVersion is what Version reads when neither a stamp nor the module version is available.
const devVersion = "dev"

// Info is the resolved build identity.
type Info struct {
	// Version is the release tag (v0.3.0), the module version of a `go install …@vX` build, or
	// "dev". Never empty.
	Version string
	// Commit is the full revision, or "" when unknown.
	Commit string
	// Date is the commit date as RFC 3339, or "" when unknown.
	Date string
}

var (
	once     sync.Once
	resolved Info
)

// Get returns the build identity, applying the ReadBuildInfo fallback once. It never fails: a
// binary with no stamps and no embedded build info reports Version "dev" with Commit and Date
// empty (REQ-15: missing version information never fails startup or a request).
func Get() Info {
	once.Do(func() { resolved = resolve(Version, Commit, Date, debug.ReadBuildInfo) })
	return resolved
}

// resolve is Get's pure core: ldflags win field by field, and each empty field falls back to the
// embedded build info. It is separate so the fallback is testable without a stamped binary.
func resolve(version, commit, date string, read func() (*debug.BuildInfo, bool)) Info {
	info := Info{Version: version, Commit: commit, Date: date}
	if info.Version != "" && info.Commit != "" && info.Date != "" {
		return info
	}
	if read != nil {
		if bi, ok := read(); ok && bi != nil {
			if info.Version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
				info.Version = bi.Main.Version
			}
			for _, s := range bi.Settings {
				switch s.Key {
				case "vcs.revision":
					if info.Commit == "" {
						info.Commit = s.Value
					}
				case "vcs.time":
					if info.Date == "" {
						info.Date = s.Value
					}
				}
			}
		}
	}
	if info.Version == "" {
		info.Version = devVersion
	}
	return info
}

// BuildDay is Date as YYYY-MM-DD in UTC, or "" when Date is empty or not RFC 3339. Git's %cI and
// Go's vcs.time are both RFC 3339, so "" means the date is genuinely unknown.
func (i Info) BuildDay() string {
	d := strings.TrimSpace(i.Date)
	if d == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, d)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.DateOnly)
}

// Banner is the one-line build identity: "switchboard v0.3.0 (built 2026-09-24)", with the
// parenthetical omitted when the date is unknown. It is the first line of every MCP session's
// instructions (SPEC-0027 REQ-2).
func (i Info) Banner() string {
	if day := i.BuildDay(); day != "" {
		return "switchboard " + i.Version + " (built " + day + ")"
	}
	return "switchboard " + i.Version
}

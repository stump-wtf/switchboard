package mcp

// Attempt report inputs
//
// The summary and artifact a lease-ending verb (release today; complete and fail with #321) closes
// its attempt with. attemptReport is the one place they are checked, so every verb accepts and
// refuses exactly the same values: a malformed artifact is an invalid call naming the argument,
// refused before the store is touched, while a long summary is only long and the store cuts it
// (store.ClipSummary, in reportArgs) and sets summary_truncated on the attempt. The MCP layer does
// not pre-clip, because the store can only report the cut it makes itself. Neither value is ever
// logged or interpreted; Switchboard never fetches an artifact.
//
// Governing: SPEC-0034 REQ-5 "Summary, Artifact and Claimant Inputs", REQ-19 "Error Handling
// Standards"; design.md "Summaries are truncated, never rejected".
//
// @joestump-agent 09/25/2026 - Added for #328 (epic #313).

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/stump-wtf/switchboard/internal/store"
)

// cairnHandle is the only non-https artifact form (SPEC-0034 REQ-5).
var cairnHandle = regexp.MustCompile(`^mcp://cairn/[A-Za-z0-9_-]{1,64}$`)

// errInvalidArtifact is the client-visible refusal. It names the argument and never echoes the
// value, so a caller's own text cannot come back in an error that might be logged downstream.
var errInvalidArtifact = &toolError{codeInvalid,
	"artifact must be an mcp://cairn/<id> handle or an absolute https URL of at most 512 bytes"}

// attemptReport builds the store report for a lease-ending verb from its summary, artifact and
// presented lease token, or returns errInvalidArtifact when the artifact is malformed. An empty
// artifact is "none" and always valid.
func attemptReport(summary, artifact, leaseToken string) (store.Report, error) {
	if artifact != "" && !validArtifact(artifact) {
		return store.Report{}, errInvalidArtifact
	}
	return store.Report{Summary: summary, Artifact: artifact, TokenHash: leaseTokenHash(leaseToken)}, nil
}

// validArtifact reports whether s is an mcp://cairn handle or an absolute https URL with a host,
// no userinfo, and no surrounding whitespace, within AttemptArtifactMax bytes.
//
// The userinfo rule is not cosmetic: an artifact is stored on the attempt and handed to later
// claimers, and the Board renders it as a link when it is an https URL (SPEC-0034 REQ-13). A URL
// like https://user:token@host/ would publish a credential to every subsequent claimer. Switchboard
// never dials an artifact, so no receiver needs userinfo here (SPEC-0024 REQ-3 refuses it for the
// URLs Switchboard does dial).
func validArtifact(s string) bool {
	if len(s) > store.AttemptArtifactMax {
		return false
	}
	if strings.TrimSpace(s) != s {
		return false
	}
	if cairnHandle.MatchString(s) {
		return true
	}
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.Hostname() != "" && u.User == nil
}

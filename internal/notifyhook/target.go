package notifyhook

// Hook URL checks shared by create time (the MCP verb) and dial time (the dispatcher), so "what we
// accepted" and "what we will connect to" cannot drift, plus the one redaction every surface uses
// to show a hook URL.
//
// Governing: SPEC-0024 REQ-3 "Target Validation (SSRF Guard)", REQ-10 (query redacted), REQ-11 (no
// response or log carries the URL's query string).

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/stump-wtf/switchboard/internal/push"
)

const (
	// MaxURLLen is the longest hook URL accepted (SPEC-0024 REQ-3), matching the column CHECK.
	MaxURLLen = 2048
	// RotationGrace is how long the previous secret keeps signing after rotate_notify_hook
	// (SPEC-0024 REQ-4, design.md "Fixed values").
	RotationGrace = 24 * time.Hour
)

// ErrInvalidURL wraps every hook-URL rejection that is not an address-class rejection from the
// shared push.Validator (those wrap push.ErrValidation). Messages never echo the URL's query.
var ErrInvalidURL = errors.New("notifyhook: invalid hook url")

// ValidateURL checks a hook URL the way SPEC-0024 REQ-3 requires and returns the one resolution that
// passed, for a pinned dial: at most MaxURLLen bytes, no userinfo, and then the shared SSRF guard
// (scheme, every resolved address, the operator allowlist, switchboard's own address and port).
// An error wraps ErrInvalidURL or push.ErrValidation.
func ValidateURL(ctx context.Context, v *push.Validator, raw string) (push.Target, error) {
	if v == nil {
		return push.Target{}, fmt.Errorf("%w: no validator configured", ErrInvalidURL)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return push.Target{}, fmt.Errorf("%w: url is required", ErrInvalidURL)
	}
	if len(raw) > MaxURLLen {
		return push.Target{}, fmt.Errorf("%w: url is %d bytes; the limit is %d", ErrInvalidURL, len(raw), MaxURLLen)
	}
	// Parse here first so a parse failure is reported generically: url.Parse's own error quotes the
	// whole URL, query string included.
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return push.Target{}, fmt.Errorf("%w: url is not an absolute http(s) URL", ErrInvalidURL)
	}
	if u.User != nil {
		return push.Target{}, fmt.Errorf("%w: url must not carry userinfo (user:pass@)", ErrInvalidURL)
	}
	return v.Resolve(ctx, raw)
}

// RedactURL renders a hook URL for display, API responses and logs: scheme, host (with port) and
// path, with any query replaced by "?redacted" and any userinfo or fragment dropped. A query string
// is where receivers most often put a token, so it never leaves the store unredacted.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(unparseable url)"
	}
	out := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath}
	s := out.String()
	if u.RawQuery != "" || u.ForceQuery {
		s += "?redacted"
	}
	return s
}

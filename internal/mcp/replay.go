package mcp

// This file is the SPEC-0005 `replay_webhook_event` delivery path — the one side-effecting tool on
// the MCP surface. It resolves the outbound target (explicit arg or the configured
// `replay_default_target`, hard error if neither), SSRF-hardens it (scheme + resolved-IP checks,
// with a DNS-rebinding guard at dial time), performs the bounded outbound POST replaying the stored
// raw payload and a replay-safe subset of headers, and reports what happened. Every attempt is
// logged with structured context and never any payload/secret material.
//
// Governing: ADR-0005 (replay is the single, SSRF-conscious side-effecting verb; optional target
// with a configured default and a hard error if neither is set), SPEC-0005 REQ "Replay Safety",
// SPEC-0005 "Redirect Validation" + "Rate Limiting" security requirements.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

const (
	// replayTimeout bounds the whole outbound replay (dial + request + response headers). Replay is
	// meant for a local/trusted consumer, so a short ceiling is generous while capping how long an
	// abusive target can tie up a caller's replay budget.
	replayTimeout = 10 * time.Second
	// maxReplayResponseBytes caps how much of the downstream response body we drain before closing.
	// We report only the status code, so the body is read solely to allow connection reuse; bounding
	// it keeps a hostile target from streaming an unbounded response at us.
	maxReplayResponseBytes = 64 << 10 // 64 KiB
	// redactionMarker is the sentinel the ingest layer writes in place of a sensitive header value
	// (internal/ingest). A header still carrying it is a redacted secret and MUST NOT be replayed.
	redactionMarker = "«redacted»"

	settingReplayDefaultTarget  = "replay_default_target"
	settingReplayAllowedTargets = "replay_allowed_targets"
)

// errBlockedTarget is the sentinel for a target that resolves into a blocked (private/loopback/
// link-local/metadata) address. Surfaced to the client as invalid_argument at pre-flight and as a
// refused dial (→ replay_failed) if a DNS rebind slips a blocked address past pre-flight.
var errBlockedTarget = errors.New("mcp: replay target resolves to a blocked address")

// replayDropHeaders is the set of stored headers never forwarded on a replay: hop-by-hop headers
// (rewritten per-connection), Host/Content-Length (set by the HTTP client for the new request), and
// credential-bearing headers (never re-emitted downstream). Any other header whose value is still
// the redaction sentinel is dropped separately (replaySafeHeaders).
var replayDropHeaders = map[string]bool{
	"host":                true,
	"content-length":      true,
	"connection":          true,
	"keep-alive":          true,
	"proxy-connection":    true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"authorization":       true,
	"cookie":              true,
	"set-cookie":          true,
}

// isBlockedIP reports whether an address is one a replay MUST NOT reach by default: loopback
// (127.0.0.0/8, ::1), RFC 1918 private + IPv6 ULA fc00::/7 (IsPrivate), link-local incl. the cloud
// metadata address 169.254.169.254 (IsLinkLocal*), and the unspecified/multicast ranges. This is
// the SSRF blocklist; explicitly trusted targets (the configured default, or an operator allowlist)
// bypass it. Governing: SPEC-0005 "Redirect Validation".
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() || // 127.0.0.0/8, ::1
		ip.IsPrivate() || // 10/8, 172.16/12, 192.168/16, fc00::/7
		ip.IsLinkLocalUnicast() || // 169.254.0.0/16 (incl. 169.254.169.254 metadata), fe80::/10
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || // 0.0.0.0, ::
		ip.IsMulticast() ||
		ip.IsInterfaceLocalMulticast()
}

// replayTarget is a resolved, validated replay destination.
type replayTarget struct {
	url     *url.URL
	trusted bool // configured default or allowlisted → exempt from the SSRF blocklist
}

// resolveReplayTarget resolves the effective target string: the explicit arg when present, else the
// configured replay_default_target. Neither present is a hard error (never a guessed target).
// Governing: SPEC-0005 scenario "No target and no default is an error, not a guess".
func (h *Handler) resolveReplayTarget(ctx context.Context, arg string) (string, error) {
	raw := strings.TrimSpace(arg)
	if raw != "" {
		return raw, nil
	}
	def, err := h.store.SettingString(ctx, settingReplayDefaultTarget, "")
	if err != nil {
		// A settings read failure is a server-side fault, not caller input; surface it up so the
		// caller maps it to the generic internal code (never the raw error).
		return "", fmt.Errorf("read %s: %w", settingReplayDefaultTarget, err)
	}
	return strings.TrimSpace(def), nil
}

// validateReplayTarget parses and SSRF-hardens the resolved target. Scheme MUST be http/https
// (rejected before any request otherwise). A target equal to the configured default or matching the
// operator allowlist is trusted and skips the address blocklist — this is how the documented
// localhost/trusted-network dev consumer is permitted without opening the tool up as a general SSRF
// primitive. Any other target has every resolved address checked against isBlockedIP.
// Governing: SPEC-0005 scenario "Non-http scheme is rejected before any request", "Redirect Validation".
func (h *Handler) validateReplayTarget(ctx context.Context, raw string) (replayTarget, error) {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return replayTarget{}, &toolError{codeInvalidArgument, "target_url must be an absolute http(s) URL"}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return replayTarget{}, &toolError{codeInvalidArgument, "target_url scheme must be http or https"}
	}

	trusted, err := h.targetTrusted(ctx, raw, u)
	if err != nil {
		return replayTarget{}, err // internal (settings read failure)
	}
	if trusted {
		return replayTarget{url: u, trusted: true}, nil
	}

	// Untrusted target: every address the host resolves to must be outside the blocklist. An IP
	// literal is checked directly; a hostname is resolved once here (pre-flight) and re-checked at
	// dial time by the guarded dialer (DNS-rebinding defence).
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return replayTarget{}, &toolError{codeInvalidArgument, "target_url resolves to a disallowed address"}
		}
		return replayTarget{url: u, trusted: false}, nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return replayTarget{}, &toolError{codeInvalidArgument, "target_url host could not be resolved"}
	}
	if len(ips) == 0 {
		return replayTarget{}, &toolError{codeInvalidArgument, "target_url host has no addresses"}
	}
	for _, ipa := range ips {
		if isBlockedIP(ipa.IP) {
			return replayTarget{}, &toolError{codeInvalidArgument, "target_url resolves to a disallowed address"}
		}
	}
	return replayTarget{url: u, trusted: false}, nil
}

// targetTrusted reports whether the target is the configured default or on the operator allowlist.
func (h *Handler) targetTrusted(ctx context.Context, raw string, u *url.URL) (bool, error) {
	def, err := h.store.SettingString(ctx, settingReplayDefaultTarget, "")
	if err != nil {
		return false, fmt.Errorf("read %s: %w", settingReplayDefaultTarget, err)
	}
	if def != "" && strings.TrimSpace(def) == raw {
		return true, nil
	}
	allowed, err := h.store.SettingString(ctx, settingReplayAllowedTargets, "")
	if err != nil {
		return false, fmt.Errorf("read %s: %w", settingReplayAllowedTargets, err)
	}
	host, hostPort := u.Hostname(), u.Host
	for _, entry := range strings.Split(allowed, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry == hostPort || entry == host {
			return true, nil
		}
	}
	return false, nil
}

// replaySafeHeaders projects the stored sanitized headers onto the subset safe to replay: hop-by-hop
// and credential headers are dropped (replayDropHeaders), and any header still carrying the redaction
// sentinel (a secret the ingest layer scrubbed) is dropped rather than forwarded as garbage.
func replaySafeHeaders(stored map[string]string) http.Header {
	out := http.Header{}
	for k, v := range stored {
		if replayDropHeaders[strings.ToLower(k)] {
			continue
		}
		if strings.Contains(v, redactionMarker) {
			continue
		}
		out.Set(k, v)
	}
	return out
}

// guardedDialContext builds a dialer whose Control hook re-checks the concrete resolved address
// immediately before the socket connects — the DNS-rebinding defence, since it runs after the name
// has been resolved to an IP and refuses the dial if that IP is blocked. Trusted targets use a plain
// dialer (blocklist bypassed) but keep the timeout.
func guardedDialContext(trusted bool) func(context.Context, string, string) (net.Conn, error) {
	d := &net.Dialer{Timeout: replayTimeout}
	if trusted {
		return d.DialContext
	}
	d.Control = func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return errBlockedTarget
		}
		ip := net.ParseIP(host)
		if ip == nil || isBlockedIP(ip) {
			return errBlockedTarget
		}
		return nil
	}
	return d.DialContext
}

// replayClient builds a single-use HTTP client for one replay: bounded dial/response timeout, no
// redirect following (a 3xx to a fresh, unvalidated target is an SSRF re-entry the tool refuses),
// and the SSRF-guarded dialer for untrusted targets.
func replayClient(trusted bool) *http.Client {
	return &http.Client{
		Timeout: replayTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // do not follow; report the redirect status as delivered
		},
		Transport: &http.Transport{
			DialContext:           guardedDialContext(trusted),
			TLSHandshakeTimeout:   replayTimeout,
			ResponseHeaderTimeout: replayTimeout,
			DisableKeepAlives:     true,
		},
	}
}

// doReplay performs the bounded outbound POST and reports the outcome. A transport/connection
// failure returns (delivered=false, status=nil, err) — the caller maps err to replay_failed. Any
// completed exchange, including a non-2xx downstream status, returns delivered=true with that status
// and a nil error. Governing: SPEC-0005 scenario "Downstream non-2xx is reported, not raised".
func doReplay(ctx context.Context, tgt replayTarget, payload []byte, headers http.Header) (delivered bool, status *int, elapsed time.Duration, err error) {
	reqCtx, cancel := context.WithTimeout(ctx, replayTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, tgt.url.String(), bytes.NewReader(payload))
	if err != nil {
		return false, nil, 0, fmt.Errorf("build replay request: %w", err)
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	start := time.Now()
	resp, err := replayClient(tgt.trusted).Do(req)
	elapsed = time.Since(start)
	if err != nil {
		return false, nil, elapsed, fmt.Errorf("replay POST: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxReplayResponseBytes))
	code := resp.StatusCode
	return true, &code, elapsed, nil
}

// replay is the full SPEC-0005 delivery path invoked by replayWebhookEventTool once the event id has
// been resolved: rate limit, target resolution + SSRF validation, the outbound POST, mandatory
// structured logging, and the reported outcome. Never logs the payload or any secret material.
func (h *Handler) replay(ctx context.Context, slug, endpointID string, id int64, detail eventDetailOut, argTarget string) (replayWebhookEventOut, error) {
	// Governing: SPEC-0005 REQ "Rate Limiting" — replay is throttled per authenticated endpoint,
	// well under the read budget, before any outbound work.
	if !h.replayRL.allow(endpointID) {
		return replayWebhookEventOut{}, &toolError{codeRateLimited, "replay rate limit exceeded; slow down"}
	}

	raw, err := h.resolveReplayTarget(ctx, argTarget)
	if err != nil {
		h.log.Error("mcp replay target resolution failed", "slug", slug, "event_id", id,
			"err", fmt.Errorf("replay_webhook_event: %w", err))
		return replayWebhookEventOut{}, &toolError{codeInternal, "internal error"}
	}
	if raw == "" {
		return replayWebhookEventOut{}, &toolError{codeInvalidArgument,
			"no target_url provided and no replay_default_target configured"}
	}

	tgt, err := h.validateReplayTarget(ctx, raw)
	if err != nil {
		var te *toolError
		if errors.As(err, &te) {
			return replayWebhookEventOut{}, te
		}
		h.log.Error("mcp replay target validation failed", "slug", slug, "event_id", id,
			"err", fmt.Errorf("replay_webhook_event: %w", err))
		return replayWebhookEventOut{}, &toolError{codeInternal, "internal error"}
	}

	headers := replaySafeHeaders(detail.Headers)
	delivered, status, elapsed, err := doReplay(ctx, tgt, []byte(detail.Payload), headers)
	ms := elapsed.Milliseconds()

	// Every replay attempt is logged with structured context — endpoint, event, target host, and
	// outcome — and never the payload or a secret. Governing: SPEC-0005 REQ "Replay Safety".
	logAttrs := []any{
		"slug", slug, "event_id", id, "target_host", tgt.url.Host,
		"trusted", tgt.trusted, "delivered", delivered, "response_ms", ms,
	}
	if status != nil {
		logAttrs = append(logAttrs, "response_status", *status)
	}
	if err != nil {
		h.log.Warn("mcp replay_webhook_event failed", append(logAttrs, "err", err)...)
		return replayWebhookEventOut{}, &toolError{codeReplayFailed, "replay could not reach the target"}
	}
	h.log.Info("mcp replay_webhook_event delivered", logAttrs...)

	return replayWebhookEventOut{
		ID:             id,
		TargetURL:      tgt.url.String(),
		Delivered:      delivered,
		ResponseStatus: status,
		ResponseMs:     ms,
	}, nil
}

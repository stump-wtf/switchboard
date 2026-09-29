package mcp

// This file is the SPEC-0005 `replay_webhook_event` delivery path — the one side-effecting tool on
// the MCP surface. It resolves the outbound target (the explicit arg, else the calling endpoint's
// first OWNED replay target; neither is replay_target_required), validates it with the shared SSRF
// guard (internal/push.Validator) at call time, dials only addresses that same guard approves at
// dial time (resolved once, then pinned), performs the bounded outbound POST replaying the stored
// raw payload and a replay-safe subset of headers, and reports what happened. Every attempt is
// logged with structured context and never any payload/secret material.
//
// There is no trusted target: an owned target is a default, never an exemption, and no replay
// reaches the network without the guard. The instance settings that used to exempt targets for
// every tenant are gone (migration 0026).
//
// Governing: ADR-0005 (replay is the single, SSRF-conscious side-effecting verb), ADR-0038 and
// SPEC-0033 REQ "Owned Replay Targets" (audit F9), ADR-0029 (the shared SSRF guard), SPEC-0005 REQ
// "Replay Safety", SPEC-0005 "Redirect Validation" + "Rate Limiting" security requirements.

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

	"github.com/stump-wtf/switchboard/internal/push"
	"github.com/stump-wtf/switchboard/internal/store"
)

const (
	// replayTimeout bounds the whole outbound replay (dial + request + response headers), capping
	// how long an abusive target can tie up a caller's replay budget.
	replayTimeout = 10 * time.Second
	// maxReplayResponseBytes caps how much of the downstream response body we drain before closing.
	// We report only the status code, so the body is read solely to allow connection reuse; bounding
	// it keeps a hostile target from streaming an unbounded response at us.
	maxReplayResponseBytes = 64 << 10 // 64 KiB
	// redactionMarker is the sentinel the ingest layer writes in place of a sensitive header value
	// (internal/ingest). A header still carrying it is a redacted secret and MUST NOT be replayed.
	redactionMarker = "«redacted»"

	// codeReplayTargetRequired: the call named no target_url and the endpoint owns no replay target.
	// Governing: SPEC-0033 scenario "Instance trusted target is gone (F9)".
	codeReplayTargetRequired = "replay_target_required"
)

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

// replayGuard is the SSRF guard every replay goes through: the shared validator for the target URL
// at call time, the resolver the dialer resolves with, and the connect step that opens a socket to
// one already-checked "ip:port". Governing: SPEC-0033 REQ "Owned Replay Targets".
type replayGuard struct {
	validator *push.Validator
	resolver  push.Resolver
	// connect opens the TCP connection to one address dialContext has already approved. In
	// production it is a net.Dialer whose Control hook checks the address again as the socket
	// connects. Tests replace ONLY this step, to reach a local listener after the guard has approved
	// a public address; resolution and every check stay the production code.
	connect func(ctx context.Context, network, address string) (net.Conn, error)
}

// newReplayGuard builds the guard over resolver r (nil = the process resolver). With no options the
// validator is the shared default: https only, public addresses only.
func newReplayGuard(r push.Resolver, opts ...push.Option) *replayGuard {
	if r == nil {
		r = push.DefaultResolver
	}
	g := &replayGuard{
		validator: push.New(append([]push.Option{push.WithResolver(r)}, opts...)...),
		resolver:  r,
	}
	d := &net.Dialer{
		Timeout: replayTimeout,
		Control: func(_, address string, _ syscall.RawConn) error { return g.validator.CheckDialAddress(address) },
	}
	g.connect = d.DialContext
	return g
}

// dialContext resolves the host ONCE, refuses the whole dial if any answer is an address the guard
// refuses (the dialer could pick any of them), and then connects only to those checked addresses, so
// a DNS answer that changed after call-time validation cannot reach a refused range.
func (g *replayGuard) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: dial address %q: %v", push.ErrValidation, addr, err)
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		answers, err := g.resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("%w: resolve %q: %v", push.ErrValidation, host, err)
		}
		for _, a := range answers {
			ips = append(ips, a.IP)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%w: host %q resolved to no addresses", push.ErrValidation, host)
	}
	checked := make([]string, 0, len(ips))
	for _, ip := range ips {
		a := net.JoinHostPort(ip.String(), port)
		if err := g.validator.CheckDialAddress(a); err != nil {
			return nil, err
		}
		checked = append(checked, a)
	}
	var lastErr error
	for _, a := range checked {
		conn, err := g.connect(ctx, network, a)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// client builds a single-use HTTP client for one replay: bounded dial/response timeout, no redirect
// following (a 3xx to a fresh, unvalidated target is an SSRF re-entry the tool refuses), no proxy
// (a proxy would dial on our behalf, past the guard), and the guarded dialer. TLS still verifies the
// target URL's host name; only the TCP connection is pinned.
func (g *replayGuard) client() *http.Client {
	return &http.Client{
		Timeout: replayTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // do not follow; report the redirect status as delivered
		},
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           g.dialContext,
			TLSHandshakeTimeout:   replayTimeout,
			ResponseHeaderTimeout: replayTimeout,
			DisableKeepAlives:     true,
		},
	}
}

// resolveReplayTarget resolves the effective target string: the explicit arg when present, else the
// calling endpoint's first owned replay target. "" means neither exists, which the caller turns into
// replay_target_required (never a guessed target). The list read is keyed by the authenticated
// endpoint's own id, so one endpoint can never resolve another's targets.
// Governing: SPEC-0033 REQ "Owned Replay Targets", SPEC-0005 scenario "No target and no default is
// an error, not a guess".
func (h *Handler) resolveReplayTarget(ctx context.Context, endpointID, arg string) (string, error) {
	if raw := strings.TrimSpace(arg); raw != "" {
		return raw, nil
	}
	owned, err := h.store.EndpointReplayTargets(ctx, endpointID)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		// A store failure is a server-side fault, not caller input; the caller maps it to the
		// generic internal code (never the raw error).
		return "", fmt.Errorf("read owned replay targets: %w", err)
	}
	for _, t := range owned {
		if t = strings.TrimSpace(t); t != "" {
			return t, nil
		}
	}
	return "", nil
}

// validateReplayTarget parses the resolved target and runs it through the shared SSRF validator at
// call time: scheme, host, and every address the host resolves to right now. Owned or not, every
// target takes this path; there is no trusted target. Governing: SPEC-0033 REQ "Owned Replay
// Targets", SPEC-0005 scenario "Non-http scheme is rejected before any request".
func (h *Handler) validateReplayTarget(ctx context.Context, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return nil, &toolError{codeInvalidArgument, "target_url must be an absolute https URL"}
	}
	if err := h.replayGuard.validator.Validate(ctx, raw); err != nil {
		// The caller gets the refusal's class only. The validator's detail can name the address a
		// host resolved to, or the resolver's own error: the server's view of its network, not the
		// caller's input, so it goes to the log alone. Governing: SPEC-0033 REQ "Owned Replay
		// Targets".
		h.log.Info("mcp replay target refused", "target_host", u.Host, "err", err)
		return nil, &toolError{codeInvalidArgument, "target_url " + push.PublicReason(err)}
	}
	return u, nil
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

// doReplay performs the bounded outbound POST and reports the outcome. A transport/connection
// failure (including a dial the guard refused) returns (delivered=false, status=nil, err) — the
// caller maps err to replay_failed. Any completed exchange, including a non-2xx downstream status,
// returns delivered=true with that status and a nil error. Governing: SPEC-0005 scenario "Downstream
// non-2xx is reported, not raised".
func doReplay(ctx context.Context, client *http.Client, target *url.URL, payload []byte, headers http.Header) (delivered bool, status *int, elapsed time.Duration, err error) {
	reqCtx, cancel := context.WithTimeout(ctx, replayTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target.String(), bytes.NewReader(payload))
	if err != nil {
		return false, nil, 0, fmt.Errorf("build replay request: %w", err)
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	start := time.Now()
	resp, err := client.Do(req)
	elapsed = time.Since(start)
	if err != nil {
		// The client wraps its failure in a *url.Error whose text carries the full target URL,
		// query string included, and the caller logs this error. A token-in-URL target must not
		// reach the log, so keep only the underlying cause; target_host is the one target field
		// the log carries. Governing: SPEC-0005 REQ "Replay Safety".
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
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

	raw, err := h.resolveReplayTarget(ctx, endpointID, argTarget)
	if err != nil {
		h.log.Error("mcp replay target resolution failed", "slug", slug, "event_id", id,
			"err", fmt.Errorf("replay_webhook_event: %w", err))
		return replayWebhookEventOut{}, &toolError{codeInternal, "internal error"}
	}
	if raw == "" {
		return replayWebhookEventOut{}, &toolError{codeReplayTargetRequired,
			"no target_url given and this endpoint owns no replay target"}
	}

	target, err := h.validateReplayTarget(ctx, raw)
	if err != nil {
		return replayWebhookEventOut{}, err
	}

	headers := replaySafeHeaders(detail.Headers)
	delivered, status, elapsed, err := doReplay(ctx, h.replayGuard.client(), target, []byte(detail.Payload), headers)
	ms := elapsed.Milliseconds()

	// Every replay attempt is logged with structured context — endpoint, event, target host, and
	// outcome — and never the payload or a secret. Governing: SPEC-0005 REQ "Replay Safety".
	logAttrs := []any{
		"slug", slug, "event_id", id, "target_host", target.Host,
		"delivered", delivered, "response_ms", ms,
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
		TargetURL:      target.String(),
		Delivered:      delivered,
		ResponseStatus: status,
		ResponseMs:     ms,
	}, nil
}

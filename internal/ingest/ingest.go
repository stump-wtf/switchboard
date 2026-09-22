// Package ingest turns inbound deliveries into verified events + durable todos (ADR-0003/007/012).
//
// There is one ingestion surface: the per-endpoint self-managed webhook, POST /webhooks/w/{token}
// (selfmanaged.go). The unguessable path token routes the delivery to the webhook an agent created
// for its own endpoint, and the body is then verified per the webhook's source type — HMAC-SHA256
// over the raw body in constant time for GitHub and Gitea, Stripe's and Slack's signature schemes
// plus a replay window over the signed timestamp (verify.go), or a shared token for generic
// senders. A bad or missing signature is a 401 and the payload is NOT persisted (only a redacted
// rejection is logged). A successful delivery is recorded as an event and enqueued as a todo owned
// by the webhook's endpoint, then published to the hub so any attached Channels session is nudged.
//
// @joestump 09/21/2026 - Rewrote after the instance-wide receivers
// (/webhooks/{github,gitea,stripe,slack,generic/*}) were removed (#181): they belonged to no tenant,
// so they could not name the endpoint that owns their todos (ADR-0022).
package ingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/stump-wtf/switchboard/internal/routing"
	"github.com/stump-wtf/switchboard/internal/store"
)

const maxBody = 5 << 20 // 5 MiB

// defaultReplayTolerance is the freshness window for signature schemes that sign a timestamp
// (Stripe `t=`, Slack `X-Slack-Request-Timestamp`). Governing: SPEC-0001 REQ "Replay-Window
// Enforcement for Timestamped Signatures" (default 300 seconds).
const defaultReplayTolerance = 300 * time.Second

// sensitiveHeaders is the explicit denylist of headers redacted before an event's headers are
// persisted. Governing: SPEC-0001 REQ "Header and Secret Sanitization Before Persist" (the spec's
// named set), ADR-0003.
var sensitiveHeaders = map[string]bool{
	"x-hub-signature": true, "x-hub-signature-256": true, "x-gitea-signature": true,
	"authorization": true, "proxy-authorization": true, "cookie": true, "set-cookie": true,
	"x-slack-signature": true, "stripe-signature": true, "x-api-key": true, "x-webhook-token": true,
}

// sensitiveNameFragments catches secret-bearing headers beyond the explicit denylist (e.g.
// X-Gitlab-Token, X-Custom-Secret, X-Auth-Key variants) so a provider we have not enumerated can
// never leak a credential into stored headers. Defense in depth over sensitiveHeaders.
// Governing: SPEC-0001 REQ "Header and Secret Sanitization Before Persist".
var sensitiveNameFragments = []string{"signature", "token", "secret", "api-key", "apikey", "auth"}

// sensitiveHeaderName reports whether a header's VALUE must be redacted before persist: either an
// exact denylist hit or a name that carries a credential-suggesting fragment.
func sensitiveHeaderName(name string) bool {
	l := strings.ToLower(name)
	if sensitiveHeaders[l] {
		return true
	}
	for _, frag := range sensitiveNameFragments {
		if strings.Contains(l, frag) {
			return true
		}
	}
	return false
}

// Ingest holds the ingestion dependencies.
type Ingest struct {
	store *store.Store
	hub   *Hub
	log   *slog.Logger
	// tolerance/now: replay window + injectable clock for signed-webhook verification, kept from
	// the shared receivers because self-managed signed webhooks verify the same way.
	tolerance time.Duration
	now       func() time.Time
	devLogin  bool
	// instrument observes in-flight deliveries for the board's ephemeral received lane
	// (instrument.go). Nil = no observation. Governing: SPEC-0015 REQ "Patch Panel Board".
	instrument Instrument
	// router evaluates webhook routing rules (routing.go). New installs the out-of-process sandbox;
	// nil means none could be built, and rule-bearing webhooks then route by default with a recorded
	// fault. Governing: ADR-0024, SPEC-0020.
	router routing.Router
	// metricsSink receives the SPEC-0023 REQ-4 ingest and routing counters (metrics.go). Nil = no-op.
	metricsSink atomic.Pointer[Metrics]
}

// Config carries the ingestion settings that are not per-tenant. Nothing else is configured here:
// the only ingestion surface is the per-endpoint self-managed webhook (ADR-0012), which carries its
// own owner, secret and target queue.
type Config struct {
	DevLogin bool
}

// Normalized returns the config with defaults applied. Nothing to default today; kept as the
// seam New applies so future non-secret knobs have a home.
func (c Config) Normalized() Config {
	return c
}

// New builds an Ingest.
func New(st *store.Store, hub *Hub, log *slog.Logger, cfg Config) *Ingest {
	cfg = cfg.Normalized()
	ing := &Ingest{
		store: st, hub: hub, log: log,
		tolerance: defaultReplayTolerance, now: time.Now,
		devLogin: cfg.DevLogin,
	}
	if sb, err := routing.NewSandbox(""); err == nil {
		ing.router = sb
	} else if log != nil {
		log.Error("routing sandbox unavailable; webhooks with rules will route by default", "err", err)
	}
	return ing
}

// readBody drains the raw request body under the 5 MiB cap, writing the rejection itself on
// failure. It is the single body-limit boundary every receiver shares, so oversize semantics (413,
// nothing persisted) and error shapes are uniform across providers. MaxBytesReader (not
// io.LimitReader) so an over-limit body is REJECTED with 413 rather than silently truncated and
// then HMAC-verified against a short read. On failure it also names the bounded verify-failure
// reason (reasonTooLarge or reasonUnreadable) for the receiver to count. Governing: SPEC-0001 REQ
// "Request Body Size Limits", REQ "Error Handling Standards"; SPEC-0023 REQ-4.
func (i *Ingest) readBody(w http.ResponseWriter, r *http.Request) (body []byte, reason string, ok bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			i.log.Warn("webhook body over limit", "path", r.URL.Path, "limit", maxBody,
				"remote", clientIP(r))
			writeErr(w, http.StatusRequestEntityTooLarge, "payload too large")
			return nil, reasonTooLarge, false
		}
		// Wrap with boundary context before logging; the client sees only a generic message.
		i.log.Warn("webhook body read failed", "path", r.URL.Path, "remote", clientIP(r),
			"err", fmt.Errorf("read request body: %w", err))
		writeErr(w, http.StatusBadRequest, "read error")
		return nil, reasonUnreadable, false
	}
	return body, "", true
}

// Delivery Accounting
//
// Every delivery a receiver handles counts exactly one switchboard_webhook_deliveries_total verdict,
// however the handler returns. A receiver opens a deliveryCount at the top and defers its record, so
// an early return can never skip the count or count twice. The verdict starts at rejected because
// every early return is a refusal (nothing persisted, a non-2xx to the sender, server faults
// included); only a committed outcome overwrites it — accepted when the event persisted (an
// idempotent redelivery and an at-most-once repeat included), dropped when the persisted outcome is
// a drop.
//
// provider and trust_mode start as "unknown": a 413 or an unknown token names no webhook. Once the
// token resolves they are the webhook's source type and trust mode, both switchboard-derived at
// create time from a fixed map (mcp webhookTrustModes; the push API mints generic/token), so the
// label set stays bounded. The metrics side coerces anything off its alphabet as a backstop.
//
// These literals are the documented values of ingest.Metrics (metrics.go); this package does not
// import internal/metrics, so they are spelled out here once.
//
// Governing: SPEC-0023 REQ-4 "Ingest and routing", REQ-5 "Cardinality"; ADR-0028.
const (
	verdictAccepted = "accepted"
	verdictRejected = "rejected"
	verdictDropped  = "dropped"

	actionQueue = "queue"
	actionDrop  = "drop"

	// labelUnknown is the provider and trust_mode of a delivery refused before its webhook resolved.
	labelUnknown = "unknown"
)

// deliveryCount is one delivery's verdict, recorded once when the receiver returns.
type deliveryCount struct {
	provider, trustMode, verdict string
}

// newDeliveryCount opens a delivery's count: rejected, from an unknown webhook, until the receiver
// learns otherwise.
func newDeliveryCount() *deliveryCount {
	return &deliveryCount{provider: labelUnknown, trustMode: labelUnknown, verdict: verdictRejected}
}

// resolved attributes the delivery to the webhook its token named.
func (d *deliveryCount) resolved(wh store.Webhook) {
	d.provider, d.trustMode = wh.SourceType, wh.TrustMode
}

// record counts the delivery. Deferred by the receiver, so it runs exactly once.
func (d *deliveryCount) record(m Metrics) {
	m.WebhookDelivery(d.provider, d.trustMode, d.verdict)
}

// verifyFailed counts one refusal the delivery's own inputs caused, under its bounded reason, against
// whichever provider the count has resolved so far.
func (d *deliveryCount) verifyFailed(m Metrics, reason string) {
	m.WebhookVerifyFailure(d.provider, reason)
}

// DevCreateTodo creates a todo directly, for exercising the vend → agent drain loop (list_todos /
// claim / complete over a vended endpoint) without a real provider. Note it does NOT ring the
// Channels doorbell: it uses plain CreateTodo, and the store's SPEC-0011 sender gate fires the
// doorbell hook only for todos persisted together with a verified delivery event — dev todos have
// none, so they surface by pull (and via this package's Hub for the web UI), losing nothing.
// Guarded by dev mode: POST /dev/todos {queue,title,kind?,payload?}.
func (i *Ingest) DevCreateTodo(w http.ResponseWriter, r *http.Request) {
	if !i.devLogin {
		http.NotFound(w, r)
		return
	}
	// Same bounded-body contract as the webhook receivers: over-limit is 413, never a truncated
	// read (SPEC-0001 REQ "Request Body Size Limits").
	body, _, ok := i.readBody(w, r)
	if !ok {
		return
	}
	var in struct {
		Queue      string          `json:"queue"`
		Title      string          `json:"title"`
		Kind       string          `json:"kind"`
		Payload    json.RawMessage `json:"payload"`
		EndpointID string          `json:"endpoint_id"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.Queue == "" || in.Title == "" {
		writeErr(w, http.StatusBadRequest, "queue and title are required")
		return
	}
	// Every todo is owned by exactly one endpoint (ADR-0022). The dev helper exists to exercise the
	// vend → agent drain loop, so the caller has just vended an endpoint and can name it:
	// endpoint_id is required — there is no instance-wide owner to fall back to.
	if in.EndpointID == "" {
		writeErr(w, http.StatusBadRequest, "endpoint_id is required")
		return
	}
	endpointID := in.EndpointID
	td, created, err := i.store.CreateTodo(r.Context(), store.CreateTodoParams{
		EndpointID: endpointID,
		Queue:      in.Queue, Source: "dev", Kind: in.Kind, Title: in.Title, Payload: in.Payload,
	})
	if err != nil {
		// Governing: SPEC-0001 REQ "Error Handling Standards" — 500 is generic to the client,
		// structured for the log.
		i.log.Error("dev create todo", "queue", in.Queue, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if created {
		i.hub.Publish(td)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": td.ID, "queue": td.Queue, "created": created})
}

// verifyGitHub checks X-Hub-Signature-256 (sha256=<hex>) over the raw body in constant time.
// GitHub's scheme signs no timestamp, so no replay window is fabricated here.
// Governing: SPEC-0001 REQ "Signed Webhook Verification", REQ "Replay-Window Enforcement for
// Timestamped Signatures" (GitHub explicitly gets no freshness window).
func verifyGitHub(secret string, body []byte, sig string) bool {
	if !strings.HasPrefix(sig, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(sig))
}

// summarizeForge builds a one-line, legible todo title from a GitHub-style forge payload. GitHub
// and Gitea share this shape for the common events (pull_request, issues, push, ...), so both
// receivers feed it their provider label, which only prefixes the fallback title for events the
// switch doesn't know.
func summarizeForge(provider, event string, body []byte) string {
	var p struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		PullRequest struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
		} `json:"pull_request"`
		Issue struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
		} `json:"issue"`
	}
	_ = json.Unmarshal(body, &p)
	repo := p.Repository.FullName
	switch event {
	case "pull_request":
		return strings.TrimSpace("PR #" + itoa(p.PullRequest.Number) + " " + p.Action + " in " + repo + " — " + p.PullRequest.Title)
	case "issues":
		return strings.TrimSpace("Issue #" + itoa(p.Issue.Number) + " " + p.Action + " in " + repo + " — " + p.Issue.Title)
	default:
		if repo != "" {
			return provider + " " + event + " in " + repo
		}
		return provider + " " + event
	}
}

// summarizeGitHub builds a one-line, legible todo title from a GitHub payload.
func summarizeGitHub(event string, body []byte) string {
	return summarizeForge("github", event, body)
}

// urlSecretParam matches a secret carried in a URL query string (e.g. a `?token=…` webhook URL that
// shows up in a Referer/Location/Link header). The value is redacted so it is never persisted.
var urlSecretParam = regexp.MustCompile(`(?i)([?&](?:token|access_token|api[_-]?key|apikey|secret|signature|sig)=)[^&#\s]+`)

// sanitizeHeaders is the single sanitization point for persisted event headers: every receiver MUST
// route inbound headers through it before handing them to the store. Secret-bearing header values
// (denylist + name-fragment match) become «redacted», and secrets embedded in URL-valued headers
// (?token=…) are redacted in place. Governing: SPEC-0001 REQ "Header and Secret Sanitization Before
// Persist".
func sanitizeHeaders(h http.Header) []byte {
	out := map[string]string{}
	for k, v := range h {
		if sensitiveHeaderName(k) {
			out[k] = "«redacted»"
			continue
		}
		out[k] = urlSecretParam.ReplaceAllString(strings.Join(v, ", "), "${1}«redacted»")
	}
	b, _ := json.Marshal(out)
	return b
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

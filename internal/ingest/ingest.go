// Package ingest turns inbound deliveries into verified events + durable todos (ADR-0003/007/014).
//
// The GitHub adapter is the reference `signed` webhook: HMAC-SHA256 over the raw body, verified in
// constant time; a bad or missing signature is a 401 and the payload is NOT persisted (only a
// redacted rejection is logged). Stripe and Slack follow the same contract with their provider
// signature schemes plus a replay window over the signed timestamp (signed.go). A successful
// delivery is recorded as an event and enqueued as a todo, then published to the hub so any
// attached Channels session is nudged.
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
	"time"

	"github.com/joestump/switchboard/internal/store"
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

// idempotencyKey derives the dedup key for a delivery: the provider's delivery id when present,
// otherwise a hash of the body. Shared by the self-managed webhook path (the only remaining
// ingestion surface). Salvaged from the retired signed receivers.
func idempotencyKey(deliveryID string, body []byte) string {
	if deliveryID != "" {
		return deliveryID
	}
	return bodyHash(body)
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
}

// Config carries the ingestion settings that are not per-tenant. All shared-receiver
// configuration (per-provider secrets, queues, generic providers, the legacy receiver's owning
// endpoint) was removed with the shared receivers themselves: the only ingestion surface is the
// per-endpoint self-managed webhook (ADR-0012), which carries its own owner.
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
	return &Ingest{
		store: st, hub: hub, log: log,
		tolerance: defaultReplayTolerance, now: time.Now,
		devLogin: cfg.DevLogin,
	}
}

// readBody drains the raw request body under the 5 MiB cap, writing the rejection itself on
// failure. It is the single body-limit boundary every receiver shares, so oversize semantics (413,
// nothing persisted) and error shapes are uniform across providers. MaxBytesReader (not
// io.LimitReader) so an over-limit body is REJECTED with 413 rather than silently truncated and
// then HMAC-verified against a short read. Governing: SPEC-0001 REQ "Request Body Size Limits",
// REQ "Error Handling Standards".
func (i *Ingest) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			i.log.Warn("webhook body over limit", "path", r.URL.Path, "limit", maxBody,
				"remote", clientIP(r))
			writeErr(w, http.StatusRequestEntityTooLarge, "payload too large")
			return nil, false
		}
		// Wrap with boundary context before logging; the client sees only a generic message.
		i.log.Warn("webhook body read failed", "path", r.URL.Path, "remote", clientIP(r),
			"err", fmt.Errorf("read request body: %w", err))
		writeErr(w, http.StatusBadRequest, "read error")
		return nil, false
	}
	return body, true
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
	body, ok := i.readBody(w, r)
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
	// endpoint_id is required now that the legacy-receiver fallback is gone with the receivers.
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

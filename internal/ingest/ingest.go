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
	"context"
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
	"x-hub-signature": true, "x-hub-signature-256": true, "authorization": true,
	"proxy-authorization": true, "cookie": true, "set-cookie": true, "x-slack-signature": true,
	"stripe-signature": true, "x-api-key": true, "x-webhook-token": true,
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
	store        *store.Store
	hub          *Hub
	log          *slog.Logger
	githubSecret string
	githubQueue  string
	stripeSecret string
	stripeQueue  string
	slackSecret  string
	slackQueue   string
	generic      map[string]GenericProvider // token/open providers by name (generic.go)
	tolerance    time.Duration              // replay window for timestamped signatures
	now          func() time.Time           // injectable clock for replay-window tests
	devLogin     bool
	// instrument observes in-flight deliveries for the board's ephemeral received lane
	// (instrument.go). Nil = no observation. Governing: SPEC-0015 REQ "Patch Panel Board".
	instrument Instrument
}

// Config carries the per-provider ingestion settings (secrets + target queues).
type Config struct {
	GitHubSecret string
	GitHubQueue  string
	StripeSecret string
	StripeQueue  string
	SlackSecret  string
	SlackQueue   string
	// Generic maps provider name → token/open configuration for the generic endpoint
	// (POST /webhooks/generic/{name}); build it with ParseGenericProviders so every entry carries
	// an explicit, validated trust mode. Governing: SPEC-0001 REQ "Explicit Open Trust Mode".
	Generic  map[string]GenericProvider
	DevLogin bool
}

// Normalized returns the config with defaults applied: queue names (github→reviews,
// stripe→stripe, slack→slack, generic→provider name) and a non-nil Generic map. New applies it
// internally; the server's boot seed (SPEC-0017 REQ "Environment Config Import") uses it too, so
// the seeded registry rows carry exactly the effective queues the receivers would have used.
func (c Config) Normalized() Config {
	if c.GitHubQueue == "" {
		c.GitHubQueue = "reviews"
	}
	if c.StripeQueue == "" {
		c.StripeQueue = "stripe"
	}
	if c.SlackQueue == "" {
		c.SlackQueue = "slack"
	}
	if c.Generic == nil {
		c.Generic = map[string]GenericProvider{}
	}
	for name, p := range c.Generic {
		// ParseGenericProviders already defaults the queue; re-apply for hand-built maps.
		if p.Queue == "" {
			p.Queue = name
			c.Generic[name] = p
		}
	}
	return c
}

// New builds an Ingest.
func New(st *store.Store, hub *Hub, log *slog.Logger, cfg Config) *Ingest {
	cfg = cfg.Normalized()
	return &Ingest{
		store: st, hub: hub, log: log,
		githubSecret: cfg.GitHubSecret, githubQueue: cfg.GitHubQueue,
		stripeSecret: cfg.StripeSecret, stripeQueue: cfg.StripeQueue,
		slackSecret: cfg.SlackSecret, slackQueue: cfg.SlackQueue,
		generic:   cfg.Generic,
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

// resolveRegistry is the dispatch-path provider registry read (ADR-0020): the provider's registry
// row plus its decrypted held secret, resolved fresh (through the store's short cache) on every
// request so registry changes bind without a restart. A store-less Ingest — the nil-store rejection
// tests, which prove no persist path runs — has no registry and reports ErrNotFound, exactly like a
// missing row. Governing: SPEC-0017 REQ "Runtime Provider Registry".
func (i *Ingest) resolveRegistry(ctx context.Context, name string) (store.Adapter, string, error) {
	if i.store == nil {
		return store.Adapter{}, "", store.ErrNotFound
	}
	return i.store.ResolveProvider(ctx, name)
}

// signedSecret resolves a signed adapter's HMAC secret registry-or-env at request time: when the
// provider's registry row exists it is authoritative — its enabled flag gates the route, and its
// (envelope-decrypted) secret wins over env config when one is held (SPEC-0017 REQ "Environment
// Config Import": the registry row wins). ErrNotFound falls back to the env secret alone, which
// after the boot seed covers only store-less test wiring and the pre-seed window; any other
// registry failure fails CLOSED (500), never open on stale trust. On a false return the rejection
// response has already been written. Governing: ADR-0020, SPEC-0017 REQ "Runtime Provider
// Registry"; SPEC-0001 verification semantics themselves are untouched.
func (i *Ingest) signedSecret(w http.ResponseWriter, r *http.Request, name, envSecret string) (string, bool) {
	secret := envSecret
	reg, regSecret, err := i.resolveRegistry(r.Context(), name)
	switch {
	case err == nil && reg.Family == "webhook" && reg.TrustMode == "signed":
		if !reg.Enabled {
			// Disabled stops the line; nothing is persisted (SPEC-0017 REQ "Provider Lifecycle").
			i.log.Warn("signed webhook rejected: provider disabled", "provider", name, "remote", clientIP(r))
			writeErr(w, http.StatusForbidden, "provider disabled")
			return "", false
		}
		if regSecret != "" {
			secret = regSecret
		}
	case err == nil:
		// A registry row of some other shape (name collision with a generic/queue provider): this
		// signed route is not what the row configures — keep the env behavior for the route.
	case errors.Is(err, store.ErrNotFound):
		// No registry row: env config alone decides, as before the registry existed.
	default:
		i.log.Error("signed provider registry lookup", "provider", name, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return "", false
	}
	if secret == "" {
		// Governing: SPEC-0001 scenario "Signature secret not configured" — reject without
		// comparing any signature.
		writeErr(w, http.StatusServiceUnavailable, name+" adapter not configured")
		return "", false
	}
	return secret, true
}

// GitHub is the signed GitHub webhook receiver: POST /webhooks/github.
func (i *Ingest) GitHub(w http.ResponseWriter, r *http.Request) {
	body, ok := i.readBody(w, r)
	if !ok {
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	// Idempotency key from the GitHub delivery GUID; body-hash fallback if the header is absent so a
	// redelivery can never bypass dedup with a NULL key (SPEC-0001 REQ "Idempotency Key Extraction
	// and Dedup").
	key := idempotencyKey(r.Header.Get("X-GitHub-Delivery"), body)
	// The line is in flight: surface it on the board's ephemeral received lane (SPEC-0015).
	i.observeReceived("github", event, "signed", key)
	// Secret registry-or-env at request time (ADR-0020); verification itself is unchanged.
	secret, ok := i.signedSecret(w, r, "github", i.githubSecret)
	if !ok {
		i.observeRejected("github", event, "signed", key, "provider unavailable")
		return
	}
	sig := r.Header.Get("X-Hub-Signature-256")
	if !verifyGitHub(secret, body, sig) {
		// Reject without persisting; log a redacted line (never the signature value).
		i.log.Warn("github signature rejected", "delivery", r.Header.Get("X-GitHub-Delivery"),
			"event", r.Header.Get("X-GitHub-Event"), "remote", clientIP(r))
		i.observeRejected("github", event, "signed", key, "signature verification failed")
		writeErr(w, http.StatusUnauthorized, "signature verification failed")
		return
	}
	// Governing: SPEC-0002/0004 REQ atomic ingestion — persist the event and enqueue its todo in a
	// single transaction so a CreateTodo failure can never leave an orphaned event row behind.
	_, td, created, err := i.store.CreateEventTodo(r.Context(),
		store.EventInput{
			Source: "github", Family: "webhook", EventType: event, ExternalID: key,
			TrustMode: "signed", Verified: true, VerifyDetail: "hmac-sha256 ok",
			ContentType: r.Header.Get("Content-Type"), Headers: sanitizeHeaders(r.Header),
			Payload: body, SourceIP: clientIP(r),
		},
		store.CreateTodoParams{
			Queue: i.githubQueue, Source: "github", Kind: event, Title: summarizeGitHub(event, body),
			Payload: body, IdempotencyKey: key,
		})
	if err != nil {
		i.log.Error("ingest github delivery", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if created {
		i.hub.Publish(td)
	} else {
		// Idempotent redelivery: resolve the in-flight card without a lane advance (SPEC-0015).
		i.observeDeduped("github", event, "signed", key)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": td.ID, "queue": td.Queue, "verified": true})
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
		Queue   string          `json:"queue"`
		Title   string          `json:"title"`
		Kind    string          `json:"kind"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.Queue == "" || in.Title == "" {
		writeErr(w, http.StatusBadRequest, "queue and title are required")
		return
	}
	td, created, err := i.store.CreateTodo(r.Context(), store.CreateTodoParams{
		Queue: in.Queue, Source: "dev", Kind: in.Kind, Title: in.Title, Payload: in.Payload,
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

// summarizeGitHub builds a one-line, legible todo title from a GitHub payload.
func summarizeGitHub(event string, body []byte) string {
	var p struct {
		Action     string `json:"action"`
		Number     int    `json:"number"`
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
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
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
			return "github " + event + " in " + repo
		}
		return "github " + event
	}
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

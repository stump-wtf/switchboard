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
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/joestump/switchboard/internal/agentapi"
	"github.com/joestump/switchboard/internal/store"
)

const maxBody = 5 << 20 // 5 MiB

// defaultReplayTolerance is the freshness window for signature schemes that sign a timestamp
// (Stripe `t=`, Slack `X-Slack-Request-Timestamp`). Governing: SPEC-0001 REQ "Replay-Window
// Enforcement for Timestamped Signatures" (default 300 seconds).
const defaultReplayTolerance = 300 * time.Second

// sensitiveHeaders are redacted before an event's headers are persisted (ADR-0002/003).
var sensitiveHeaders = map[string]bool{
	"x-hub-signature": true, "x-hub-signature-256": true, "authorization": true,
	"cookie": true, "x-slack-signature": true, "stripe-signature": true, "x-api-key": true,
	"x-webhook-token": true,
}

// Ingest holds the ingestion dependencies.
type Ingest struct {
	store        *store.Store
	hub          *agentapi.Hub
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

// New builds an Ingest.
func New(st *store.Store, hub *agentapi.Hub, log *slog.Logger, cfg Config) *Ingest {
	if cfg.GitHubQueue == "" {
		cfg.GitHubQueue = "reviews"
	}
	if cfg.StripeQueue == "" {
		cfg.StripeQueue = "stripe"
	}
	if cfg.SlackQueue == "" {
		cfg.SlackQueue = "slack"
	}
	if cfg.Generic == nil {
		cfg.Generic = map[string]GenericProvider{}
	}
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
// failure. Governing: SPEC-0001 REQ body limits. MaxBytesReader (not io.LimitReader) so an
// over-limit body is REJECTED with 413 rather than silently truncated and then HMAC-verified
// against a short read.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "payload too large")
			return nil, false
		}
		http.Error(w, "read error", http.StatusBadRequest)
		return nil, false
	}
	return body, true
}

// GitHub is the signed GitHub webhook receiver: POST /webhooks/github.
func (i *Ingest) GitHub(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if i.githubSecret == "" {
		http.Error(w, "github adapter not configured", http.StatusServiceUnavailable)
		return
	}
	sig := r.Header.Get("X-Hub-Signature-256")
	if !verifyGitHub(i.githubSecret, body, sig) {
		// Reject without persisting; log a redacted line (never the signature value).
		i.log.Warn("github signature rejected", "delivery", r.Header.Get("X-GitHub-Delivery"),
			"event", r.Header.Get("X-GitHub-Event"), "remote", clientIP(r))
		writeErr(w, http.StatusUnauthorized, "signature verification failed")
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	// Idempotency key from the GitHub delivery GUID; body-hash fallback if the header is absent so a
	// redelivery can never bypass dedup with a NULL key (SPEC-0001 REQ "Idempotency Key Extraction
	// and Dedup").
	key := idempotencyKey(r.Header.Get("X-GitHub-Delivery"), body)
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
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": td.ID, "queue": td.Queue, "verified": true})
}

// DevCreateTodo creates a todo directly, for exercising the vend → agent → Channels loop without a
// real provider. Guarded by dev mode: POST /dev/todos {queue,title,kind?,payload?}.
func (i *Ingest) DevCreateTodo(w http.ResponseWriter, r *http.Request) {
	if !i.devLogin {
		http.NotFound(w, r)
		return
	}
	var in struct {
		Queue   string          `json:"queue"`
		Title   string          `json:"title"`
		Kind    string          `json:"kind"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&in); err != nil || in.Queue == "" || in.Title == "" {
		writeErr(w, http.StatusBadRequest, "queue and title are required")
		return
	}
	td, created, err := i.store.CreateTodo(r.Context(), store.CreateTodoParams{
		Queue: in.Queue, Source: "dev", Kind: in.Kind, Title: in.Title, Payload: in.Payload,
	})
	if err != nil {
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

func sanitizeHeaders(h http.Header) []byte {
	out := map[string]string{}
	for k, v := range h {
		if sensitiveHeaders[strings.ToLower(k)] {
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

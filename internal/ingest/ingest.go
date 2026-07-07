// Package ingest turns inbound deliveries into verified events + durable todos (ADR-0003/007/014).
//
// The GitHub adapter is the reference `signed` webhook: HMAC-SHA256 over the raw body, verified in
// constant time; a bad or missing signature is a 401 and the payload is NOT persisted (only a
// redacted rejection is logged). A successful delivery is recorded as an event and enqueued as a
// todo, then published to the hub so any attached Channels session is nudged.
package ingest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/joestump/switchboard/internal/agentapi"
	"github.com/joestump/switchboard/internal/store"
)

const maxBody = 5 << 20 // 5 MiB

// sensitiveHeaders are redacted before an event's headers are persisted (ADR-0002/003).
var sensitiveHeaders = map[string]bool{
	"x-hub-signature": true, "x-hub-signature-256": true, "authorization": true,
	"cookie": true, "x-slack-signature": true, "stripe-signature": true, "x-api-key": true,
}

// Ingest holds the ingestion dependencies.
type Ingest struct {
	store        *store.Store
	hub          *agentapi.Hub
	log          *slog.Logger
	githubSecret string
	githubQueue  string
	devLogin     bool
}

// New builds an Ingest.
func New(st *store.Store, hub *agentapi.Hub, log *slog.Logger, githubSecret, githubQueue string, devLogin bool) *Ingest {
	if githubQueue == "" {
		githubQueue = "reviews"
	}
	return &Ingest{store: st, hub: hub, log: log, githubSecret: githubSecret, githubQueue: githubQueue, devLogin: devLogin}
}

// GitHub is the signed GitHub webhook receiver: POST /webhooks/github.
func (i *Ingest) GitHub(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
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
	delivery := r.Header.Get("X-GitHub-Delivery")
	eventID, err := i.store.InsertEvent(r.Context(), store.EventInput{
		Source: "github", Family: "webhook", EventType: event, ExternalID: delivery,
		TrustMode: "signed", Verified: true, VerifyDetail: "hmac-sha256 ok",
		ContentType: r.Header.Get("Content-Type"), Headers: sanitizeHeaders(r.Header),
		Payload: body, SourceIP: clientIP(r),
	})
	if err != nil {
		i.log.Error("insert event", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	td, created, err := i.store.CreateTodo(r.Context(), store.CreateTodoParams{
		Queue: i.githubQueue, Source: "github", Kind: event, Title: summarizeGitHub(event, body),
		Payload: body, EventID: &eventID, IdempotencyKey: delivery,
	})
	if err != nil {
		i.log.Error("create todo", "err", err)
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

func sanitizeHeaders(h http.Header) []byte {
	out := map[string]string{}
	for k, v := range h {
		if sensitiveHeaders[strings.ToLower(k)] {
			out[k] = "«redacted»"
			continue
		}
		out[k] = strings.Join(v, ", ")
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

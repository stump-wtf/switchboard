package main

// Webhook Lifecycle Verbs: create, rotate, delete
//
// `switchboard webhook create|rotate|delete` drive the human API's lifecycle routes (SPEC-0035 REQ
// "Webhook Management") as the logged-in human, so the three things an owner does with a webhook no
// longer need the owning endpoint's MCP credential: make one for an endpoint you own, retire a leaked
// secret, and remove one for good.
//
// Where the secret goes is the part these commands exist for. A create and a rotate mint a signing
// secret that switchboard shows exactly once, and until now the only place it could go was stdout,
// which means a scripted vend prints a credential into a terminal, a scrollback buffer, a CI transcript and
// whatever ships terminal output. With --secret-file or --env-file the CLI writes the secret to a file
// created 0600, prints the path, and puts no secret on stdout at all. Without either flag the reveal
// goes to stdout as it does today, because a human at a keyboard has to see it once in order to paste
// it into the producer.
//
// Governing: SPEC-0035 REQ "CLI Parity", REQ "Webhook Management"; SPEC-0006 REQ
// "Webhook Self-Management Within a Vended Ceiling"; SPEC-0032 REQ-2, REQ-3; ADR-0012.
//
// @joestump-agent 10/22/2026 - Added webhook create/rotate/delete for stump.wtf/switchboard#584.
//
// @joestump-agent 10/22/2026 - A --secret-file on a provider-origin source now says on stderr that
// the file holds the ingest URL, not a signing secret, and --json stops claiming a secret was
// written when none exists (#607 review).

import (
	"context"
	"encoding/json"

	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// webhookDoc is a create or rotate response (internal/server webhookOut). SigningSecret is the one-time
// reveal: read it, write it where the caller chose, and never print it when a file was named.
type webhookDoc struct {
	WebhookID         string `json:"webhook_id"`
	EndpointID        string `json:"endpoint_id"`
	EndpointSlug      string `json:"endpoint_slug,omitempty"`
	IngestURL         string `json:"ingest_url"`
	SourceType        string `json:"source_type"`
	TargetQueue       string `json:"target_queue"`
	TrustMode         string `json:"trust_mode"`
	SigningSecret     string `json:"signing_secret,omitempty"`
	SecretOrigin      string `json:"secret_origin"`
	SecretSet         bool   `json:"secret_set"`
	SecretFingerprint string `json:"secret_fingerprint,omitempty"`
	Warning           string `json:"warning,omitempty"`
}

// --- webhook create ---

func cmdWebhookCreate(c *cli, args []string) int {
	fs := c.flagSet("webhook create", "ENDPOINT", "Create an ingestion webhook on an endpoint you own, inside the ceiling it was vended with.\n"+
		"ENDPOINT is its slug (from `switchboard endpoint list`) or its id. That ceiling decides what is\n"+
		"accepted: --source must be one of the endpoint's allowed source types and --queue one of its\n"+
		"allowed webhook queues, and a refusal names which of those it was.\n"+
		"The signing secret is shown ONCE. Give --secret-file or --env-file to keep it off the terminal.")
	source := fs.String("source", "", "the ingestion source type to create (github, gitea, cairn, stripe, slack, generic, telemetry)")
	queue := fs.String("queue", "", "the queue delivered events route to; must be inside the endpoint's webhook ceiling")
	fs.StringVar(queue, "q", "", "shorthand for --queue")
	secretFile := fs.String("secret-file", "", "write the signing secret to this file (created 0600) instead of printing it")
	envFile := fs.String("env-file", "", "write SWITCHBOARD_WEBHOOK_SECRET and SWITCHBOARD_WEBHOOK_URL to this file (created 0600)")
	asJSON := fs.Bool("json", false, "print the raw API response")
	pos, exit, ok := c.parseArgs(fs, args)
	if !ok {
		return exit
	}
	switch {
	case len(pos) == 0 || strings.TrimSpace(pos[0]) == "":
		return c.usageError(fs, "an endpoint is required: a slug from `switchboard endpoint list`, or an endpoint id")
	case len(pos) > 1:
		return c.usageError(fs, fmt.Sprintf("unexpected argument %q", pos[1]))
	}
	if strings.TrimSpace(*source) == "" {
		return c.usageError(fs, "--source is required (`switchboard endpoint list -v` shows what an endpoint may create)")
	}
	if strings.TrimSpace(*queue) == "" {
		return c.usageError(fs, "--queue is required: the queue delivered events route to")
	}
	dest, err := secretDestination(*secretFile, *envFile)
	if err != nil {
		return c.usageError(fs, err.Error())
	}
	api, err := c.apiClient(context.Background())
	if err != nil {
		return c.fail(err)
	}
	target, err := c.resolveEndpointRef(api, strings.TrimSpace(pos[0]))
	if err != nil {
		return c.fail(err)
	}
	body, err := api.post("/api/v1/endpoints/"+target+"/webhooks", map[string]any{
		"source_type": strings.TrimSpace(*source), "target_queue": strings.TrimSpace(*queue),
	})
	if err != nil {
		return c.fail(err)
	}
	if *asJSON {
		return c.revealJSON(body, dest, "webhook create")
	}
	var doc webhookDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return c.fail(fmt.Errorf("webhook create: malformed response: %w", err))
	}
	if err := c.reportSecret("Created", dest, doc); err != nil {
		return c.fail(err)
	}
	return exitOK
}

// --- webhook rotate ---

func cmdWebhookRotate(c *cli, args []string) int {
	fs := c.flagSet("webhook rotate", "WEBHOOK_ID", "Retire a webhook's ingest URL and signing secret and mint new ones, which is what turns a\n"+
		"leaked secret back into a working webhook. Deliveries signed with the old secret stop passing\n"+
		"the moment this returns. For a source whose producer supplies its own secret (stripe, slack) only\n"+
		"the URL changes: that secret stays, because a fresh one is a value nobody signs with.\n"+
		"The new signing secret is shown ONCE. Give --secret-file or --env-file to keep it off the terminal.")
	secretFile := fs.String("secret-file", "", "write the new signing secret to this file (created 0600) instead of printing it")
	envFile := fs.String("env-file", "", "write SWITCHBOARD_WEBHOOK_SECRET and SWITCHBOARD_WEBHOOK_URL to this file (created 0600)")
	asJSON := fs.Bool("json", false, "print the raw API response")
	id, exit, ok := c.webhookArg(fs, args)
	if !ok {
		return exit
	}
	dest, err := secretDestination(*secretFile, *envFile)
	if err != nil {
		return c.usageError(fs, err.Error())
	}
	api, err := c.apiClient(context.Background())
	if err != nil {
		return c.fail(err)
	}
	body, err := api.post("/api/v1/webhooks/"+url.PathEscape(id)+"/rotate", nil)
	if err != nil {
		return c.fail(err)
	}
	if *asJSON {
		return c.revealJSON(body, dest, "webhook rotate")
	}
	var doc webhookDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return c.fail(fmt.Errorf("webhook rotate: malformed response: %w", err))
	}
	if err := c.reportSecret("Rotated", dest, doc); err != nil {
		return c.fail(err)
	}
	return exitOK
}

// --- webhook delete ---

func cmdWebhookDelete(c *cli, args []string) int {
	fs := c.flagSet("webhook delete", "WEBHOOK_ID", "Delete a webhook: its ingest URL stops working at once, its routes go with it, and the\n"+
		"ceiling slot it held frees for the next one. Events already recorded stay in history. This is not\n"+
		"undone: create a new webhook and hand the producer its new URL instead.")
	asJSON := fs.Bool("json", false, "print the raw API response")
	force := fs.Bool("force", false, "delete without asking")
	id, exit, ok := c.webhookArg(fs, args)
	if !ok {
		return exit
	}
	if !*force {
		fmt.Fprintf(c.stderr, "Delete webhook %s? Its ingest URL stops working at once. [y/N] ", id)
		if !c.confirm() {
			fmt.Fprintln(c.stderr, "Left alone.")
			return exitOK
		}
	}
	api, err := c.apiClient(context.Background())
	if err != nil {
		return c.fail(err)
	}
	body, err := api.del("/api/v1/webhooks/" + url.PathEscape(id))
	if err != nil {
		return c.fail(err)
	}
	if *asJSON {
		return c.printJSON(body)
	}
	fmt.Fprintf(c.stdout, "Deleted webhook %s. Anything wired to it needs a new one: `switchboard webhook create ENDPOINT --source TYPE --queue Q`.\n", id)
	return exitOK
}

// --- the one-time reveal, to a file instead of to a terminal ---

// secretDest is where a revealed secret goes: an env file (shell-sourceable KEY=VALUE lines), a secret
// file (the bare value), or neither, which means stdout — the shape a human at a keyboard has always
// had. The two flags are mutually exclusive because a caller that wanted both would be asking for the
// same secret in two places, one of which is a file someone will eventually commit.
type secretDest struct {
	path string
	env  bool
}

func secretDestination(secretFile, envFile string) (secretDest, error) {
	secretFile, envFile = strings.TrimSpace(secretFile), strings.TrimSpace(envFile)
	switch {
	case secretFile != "" && envFile != "":
		return secretDest{}, fmt.Errorf("--secret-file and --env-file are mutually exclusive: pick where the secret goes")
	case secretFile != "":
		return secretDest{path: secretFile}, nil
	case envFile != "":
		return secretDest{path: envFile, env: true}, nil
	}
	return secretDest{}, nil
}

// writeSecretFile writes the webhook reveal to dest.path. Governing: SPEC-0007 (one-time reveal).
func writeSecretFile(dest secretDest, doc webhookDoc) error {
	lines := make([]string, 0, 2)
	switch {
	case dest.env && doc.SigningSecret != "":
		lines = append(lines, "SWITCHBOARD_WEBHOOK_URL="+doc.IngestURL, "SWITCHBOARD_WEBHOOK_SECRET="+doc.SigningSecret)
	case dest.env:
		lines = append(lines, "SWITCHBOARD_WEBHOOK_URL="+doc.IngestURL)
	case doc.SigningSecret != "":
		lines = append(lines, doc.SigningSecret)
	default:
		lines = append(lines, doc.IngestURL)
	}
	return writeSecretLines(dest, lines)
}

// writeSecretLines writes the reveal to dest.path, which is created 0600 and truncated if it existed. The
// file IS the secret, so the umask of whoever happened to run the command has no business deciding who
// can read it; the explicit Chmod is what makes that true on a system with a loose umask, and it is
// re-applied to an existing file so a world-readable one cannot be quietly reused for a credential.
// Governing: SPEC-0007 (a credential is revealed once, and to one place).
func writeSecretLines(dest secretDest, lines []string) error {
	if dest.path == "" {
		return nil
	}
	if dir := filepath.Dir(dest.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(dest.path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", dest.path, err)
	}
	if err := os.Chmod(dest.path, 0o600); err != nil {
		return fmt.Errorf("protect %s: %w", dest.path, err)
	}
	return nil
}

// reportSecret prints a create or rotate result without leaking the secret it carries: with a destination
// file named, the secret goes there and stdout learns only where it went; without one, stdout carries
// the one-time reveal, which is what a human at a keyboard has to see once in order to paste it.
func (c *cli) reportSecret(action string, dest secretDest, doc webhookDoc) error {
	if dest.path != "" {
		if err := writeSecretFile(dest, doc); err != nil {
			return err
		}
		warnNonSecretFile(c, dest, doc)
	}
	fmt.Fprintf(c.stdout, "%s %s webhook %s, delivering to queue %s.\n", action, doc.SourceType, doc.WebhookID, doc.TargetQueue)
	fmt.Fprintf(c.stdout, "Ingest URL: %s\n", doc.IngestURL)
	switch {
	case dest.path != "" && doc.SigningSecret != "":
		fmt.Fprintf(c.stdout, "Signing secret written to %s (mode 0600); it is not printed here.\n", dest.path)
	case doc.SigningSecret != "":
		fmt.Fprintln(c.stdout, "This signing secret is shown ONCE and cannot be recovered: paste it into the producer now.")
		fmt.Fprintf(c.stdout, "Signing secret: %s\n", doc.SigningSecret)
	case doc.TrustMode == "token":
		fmt.Fprintln(c.stdout, "Trust: token. The unguessable ingest URL is the credential, so there is no secret to paste.")
	default:
		fmt.Fprintf(c.stdout, "Trust: %s, secret origin %s. The producer signs with a secret only it knows, so this webhook awaits that one.\n", doc.TrustMode, doc.SecretOrigin)
	}
	if doc.Warning != "" {
		fmt.Fprintf(c.stdout, "\n%s\n", doc.Warning)
	}
	return nil
}

// revealJSON is --json with a destination file named: the raw body is printed as promised, so the secret
// it carries must go to the file first, or --json would undo the very flag that asked for a file.
func (c *cli) revealJSON(body []byte, dest secretDest, what string) int {
	if dest.path == "" {
		return c.printJSON(body)
	}
	var doc webhookDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return c.fail(fmt.Errorf("%s: malformed response: %w", what, err))
	}
	if err := writeSecretFile(dest, doc); err != nil {
		return c.fail(err)
	}
	warnNonSecretFile(c, dest, doc)
	if doc.SigningSecret != "" {
		fmt.Fprintf(c.stderr, "switchboard: the signing secret was written to %s (mode 0600) and is not in the printed body\n", dest.path)
	}
	return c.printJSON(redactSecret(body))
}

// warnNonSecretFile tells a caller whose --secret-file got something other than a secret: a
// provider-origin source (stripe, slack) has no switchboard-issued signing secret, so the 0600 file
// named for one holds the ingest URL instead. The write itself is kept, because the URL is what the
// producer setup needs next, but a file quietly holding something other than what its flag promised
// is how a setup step gets skipped and every delivery 401s. Governing: SPEC-0032 REQ-2.
func warnNonSecretFile(c *cli, dest secretDest, doc webhookDoc) {
	if dest.path == "" || dest.env || doc.SigningSecret != "" {
		return
	}
	fmt.Fprintf(c.stderr, "switchboard: note: %s holds the ingest URL, not a signing secret — %s's producer signs with a secret only it issues\n", dest.path, doc.SourceType)
}

// redactSecret replaces a signing_secret value in a JSON body with a marker, so `--json` combined with
// a destination file never doubles the secret into stdout. The shape is fixed (the API's own response), so
// this is a substitution on one key and not a general scrub.
func redactSecret(body []byte) []byte {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return body
	}
	if _, ok := doc["signing_secret"]; !ok {
		return body
	}
	doc["signing_secret"] = "REDACTED: written to the --secret-file/--env-file you named"
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

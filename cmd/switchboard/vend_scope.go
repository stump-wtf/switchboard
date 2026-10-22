package main

// `switchboard endpoint vend` takes the same scope choices the web wizard's steps ask for, from flags
// or from a JSON scope file, so a scripted operator can vend a router endpoint (named source types,
// a multi-queue ceiling, a credential lifetime) instead of only the basics endpoint.
//
// The file is the shape for scope that is checked into a repo: it carries a subset of the vend request
// body, unknown keys are refused rather than ignored, and a flag always beats the file it duplicates,
// because the operator typing the override meant it. What the file may say and what the server accepts
// stay one contract: the same internal/vend rules the wizard runs are applied here before the request
// leaves the machine, so a bad scope costs a usage error instead of a minted-and-revoked endpoint.
//
// A named credential destination (--secret-file, --env-file) writes the minted bearer token to a 0600
// file and prints only the path. The token appears in TWO places in a vend response, the top-level
// field and the Authorization header inside the .mcp.json wiring, so both are scrubbed from anything
// printed on stdout.
//
// Governing: SPEC-0006 REQ "Webhook Self-Management Within a Vended Ceiling", SPEC-0007 (one-time
// reveal, immutable scope), SPEC-0016 REQ "Credential Lifetime", ADR-0047 (the narrow verb default).
//
// @joestump-agent 10/22/2026 - Added for stump.wtf/switchboard#584.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/stump-wtf/switchboard/internal/vend"
)

// vendScopeFile is the subset of POST /api/v1/endpoints a scope file may carry. Anything else in the
// body is a caller's per-request choice, not something to check in.
type vendScopeFile struct {
	Name                   string   `json:"name,omitempty"`
	Queue                  string   `json:"queue,omitempty"`
	ReplayTargets          []string `json:"replay_targets,omitempty"`
	Verbs                  []string `json:"verbs,omitempty"`
	SourceTypes            []string `json:"source_types,omitempty"`
	AllowedQueues          []string `json:"allowed_queues,omitempty"`
	WebhookMax             *int     `json:"webhook_max,omitempty"`
	TTL                    string   `json:"ttl,omitempty"`
	DefaultLeaseTTLSeconds *int     `json:"default_lease_ttl_seconds,omitempty"`
}

// readVendScopeFile reads and decodes a scope file. An unknown key is refused by name: a scope file
// whose typo silently dropped a ceiling field would vend an endpoint with a wider or narrower grant
// than the file appears to say, and scope cannot be edited after vend.
func readVendScopeFile(path string) (*vendScopeFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--from-file %s: %w", path, err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var f vendScopeFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("--from-file %s: %w", path, err)
	}
	return &f, nil
}

// checkVendScope validates the scope fields a CLI invocation is about to send, using the values as
// supplied: fields left unset travel unset and take the server's basics defaults, so nothing here
// repeats what those defaults are. A lifetime is checked only when one was named, because an absent
// lifetime means "valid until revoked", not an empty duration.
func checkVendScope(sourceTypes, allowedQueues []string, webhookMax *int, ttl string) error {
	max := 0
	if webhookMax != nil {
		max = *webhookMax
	}
	if err := vend.CheckCeiling(sourceTypes, allowedQueues, max); err != nil {
		return err
	}
	if strings.TrimSpace(ttl) != "" {
		if _, err := vend.ParseLifetime(ttl); err != nil {
			return err
		}
	}
	return nil
}

// splitCSV turns a flag's comma-separated list into trimmed values, dropping blanks, so `--source-types
// a,b` and the wizard's chips produce the same grant.
func splitCSV(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// flagWasSet reports whether the operator typed any of these flag names (a shorthand counts).
func flagWasSet(set map[string]bool, names ...string) bool {
	for _, n := range names {
		if set[n] {
			return true
		}
	}
	return false
}

// fileSourceTypes and fileAllowedQueues read one ceiling list off the scope file, where nil means the
// file did not name the field at all and an empty non-nil slice means it named an empty one.
func fileSourceTypes(f *vendScopeFile) []string {
	if f == nil {
		return nil
	}
	return f.SourceTypes
}

func fileAllowedQueues(f *vendScopeFile) []string {
	if f == nil {
		return nil
	}
	return f.AllowedQueues
}

// scopeList resolves one ceiling list: a typed flag wins over the file, and a field neither names stays
// out of the request (ok=false) so the server's default applies to it. An empty list named on purpose
// is reported as named and empty, which is a different scope from "not named".
func scopeList(set map[string]bool, flagName, flagValue string, fileValue []string) ([]string, bool) {
	if set[flagName] {
		return splitCSV(flagValue), true
	}
	if fileValue != nil {
		return fileValue, true
	}
	return nil, false
}

// writeVendSecret puts the minted endpoint credential in dest: an env file carries the endpoint URL and
// the bearer token as shell-sourceable lines, a secret file carries the bare token, which is what a
// reader that already knows the URL wants.
func writeVendSecret(dest secretDest, mcpURL, token string) error {
	if token == "" {
		return fmt.Errorf("vend: the response carried no token, so nothing was written to %s", dest.path)
	}
	if dest.env {
		return writeSecretLines(dest, []string{"SWITCHBOARD_MCP_URL=" + mcpURL, "SWITCHBOARD_ENDPOINT_TOKEN=" + token})
	}
	return writeSecretLines(dest, []string{token})
}

// redactVendBody strips the minted bearer token from a vend response wherever it appears: the
// top-level token field and the Authorization header the .mcp.json wiring embeds. Without the second
// one, --json would print the credential it was told to put in a file.
func redactVendBody(body []byte) []byte {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return body
	}
	token, _ := doc["token"].(string)
	if token == "" {
		return body
	}
	doc["token"] = "REDACTED: written to the --secret-file/--env-file you named"
	substituteString(doc, token, "Bearer REDACTED")
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

// substituteString replaces every string value containing needle, at any depth.
func substituteString(v any, needle, replacement string) {
	switch node := v.(type) {
	case map[string]any:
		for k, child := range node {
			if s, ok := child.(string); ok {
				if strings.Contains(s, needle) {
					node[k] = strings.ReplaceAll(s, needle, replacement)
				}
				continue
			}
			substituteString(child, needle, replacement)
		}
	case []any:
		for i, child := range node {
			if s, ok := child.(string); ok {
				if strings.Contains(s, needle) {
					node[i] = strings.ReplaceAll(s, needle, replacement)
				}
				continue
			}
			substituteString(child, needle, replacement)
		}
	}
}

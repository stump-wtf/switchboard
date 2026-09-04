package mcp

// Client Wiring For A Vended Endpoint
//
// The .mcp.json stanza an MCP client pastes to reach /mcp/{slug} over Streamable HTTP. One
// renderer serves every surface that reveals a credential — the web wizard's one-time reveal, the
// operator API's vend response, and therefore the CLI — so the wiring can never drift between
// them. Governing: SPEC-0014 (Streamable HTTP endpoint), SPEC-0015 (reveal wiring), ADR-0023.
//
// @joestump-agent 09/03/2026 - Lifted out of internal/web so the /api/v1 vend response carries
// the same stanza the web reveal shows.

import (
	"encoding/json"
	"strings"
)

// EndpointURL is the Streamable-HTTP URL of a vended endpoint: <base>/mcp/<slug>.
func EndpointURL(baseURL, slug string) string {
	return strings.TrimRight(baseURL, "/") + "/mcp/" + slug
}

// ClientConfigJSON renders the .mcp.json stanza that wires an MCP client to the endpoint with the
// vended bearer credential embedded: type http, the /mcp/{slug} URL, and an Authorization header.
func ClientConfigJSON(baseURL, slug, token string) string {
	return clientConfigJSON(EndpointURL(baseURL, slug), token)
}

// ClientConfigJSONURLOnly renders the URL-only variant for OAuth-capable clients: the same
// endpoint with no embedded credential — the client discovers the authorization server from the
// endpoint's RFC 9728 metadata and signs the human in over OAuth instead (ADR-0019).
func ClientConfigJSONURLOnly(baseURL, slug string) string {
	return clientConfigJSON(EndpointURL(baseURL, slug), "")
}

func clientConfigJSON(url, token string) string {
	server := map[string]any{"type": "http", "url": url}
	if token != "" {
		server["headers"] = map[string]string{"Authorization": "Bearer " + token}
	}
	b, _ := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{"switchboard": server}}, "", "  ")
	return string(b)
}

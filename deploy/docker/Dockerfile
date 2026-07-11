# switchboard container image — a single static binary (assets + migrations embedded via embed.FS).
# Built and pushed by .gitea/workflows/build.yaml to gitea.stump.rocks/joestump/switchboard.
#
# Runtime needs: PostgreSQL (SWITCHBOARD_DATABASE_URL) and, for real login, a Pocket ID OIDC client
# (SWITCHBOARD_OIDC_*). The container binds 0.0.0.0:8080 — switchboard is a complete, self-contained
# OIDC relying party (ADR-011: its own /auth/callback, PKCE, session cookies), so put a plain Caddy
# reverse_proxy in front. No forward_auth: it would be redundant with the app's own login and could
# conflict with the OIDC redirect flow.
#
# Reverse-proxy streaming (ADR-0017; SPEC-0014): vended MCP endpoints are served over Streamable HTTP,
# and `GET /mcp/{endpoint}` is a long-lived notification stream. The proxy MUST NOT buffer it or
# channel doorbells stall — Caddy: `reverse_proxy ... { flush_interval -1 }` (nginx: `proxy_buffering
# off;`) on `/mcp/*`. See deploy/docker/compose.yaml.

# Vendored build: deps are committed under vendor/ so the image builds with NO network. The Gitea
# act_runner's BuildKit (docker-container driver) has no egress to the Go module proxy, so an
# in-container `go mod download` (or a GOTOOLCHAIN fetch) fails — vendoring makes the build hermetic.
# Base MUST be >= the go.mod `go` directive or GOTOOLCHAIN would try to fetch a toolchain (needs network).
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -mod=vendor -trimpath -ldflags "-s -w" -o /out/switchboard ./cmd/switchboard

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/switchboard /usr/local/bin/switchboard
# Bind all interfaces inside the container (the container network is the boundary; Caddy gates ingress).
ENV SWITCHBOARD_ADDR=0.0.0.0:8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/switchboard"]
CMD ["serve"]

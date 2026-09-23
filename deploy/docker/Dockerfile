# switchboard container image — a single static binary (assets + migrations embedded via embed.FS).
# Published to ghcr.io/stump-wtf/switchboard on v* tags (.github/workflows/release.yml); internal
# registry builds happen via .gitea/workflows/build.yaml.
# Self-hosting? Pull the published image, or build this yourself — docs/guides/14-self-hosting.md.
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
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY . .
# The build identity (internal/buildinfo, SPEC-0027 REQ-1). .git is not in the build context
# (.dockerignore), so the binary has no embedded VCS info to fall back on: pass COMMIT and DATE
# (git log -1 --format=%cI) or the image reports them as unknown.
ARG VERSION=dev
ARG COMMIT=
ARG DATE=
RUN CGO_ENABLED=0 go build -mod=vendor -trimpath -ldflags "-s -w \
      -X github.com/stump-wtf/switchboard/internal/buildinfo.Version=${VERSION} \
      -X github.com/stump-wtf/switchboard/internal/buildinfo.Commit=${COMMIT} \
      -X github.com/stump-wtf/switchboard/internal/buildinfo.Date=${DATE}" \
    -o /out/switchboard ./cmd/switchboard

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/switchboard /usr/local/bin/switchboard
# Bind all interfaces inside the container (the container network is the boundary; Caddy gates ingress).
ENV SWITCHBOARD_ADDR=0.0.0.0:8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/switchboard"]
CMD ["serve"]

# switchboard container image — a single static binary (assets + migrations embedded via embed.FS).
# Built and pushed by .gitea/workflows/build.yaml to gitea.stump.rocks/joestump/switchboard.
#
# Runtime needs: PostgreSQL (SWITCHBOARD_DATABASE_URL) and, for real login, a Pocket ID OIDC client
# (SWITCHBOARD_OIDC_*). The container binds 0.0.0.0:8080 — put it behind Caddy forward_auth (ADR-001).

FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/switchboard ./cmd/switchboard

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/switchboard /usr/local/bin/switchboard
# Bind all interfaces inside the container (the container network is the boundary; Caddy gates ingress).
ENV SWITCHBOARD_ADDR=0.0.0.0:8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/switchboard"]
CMD ["serve"]

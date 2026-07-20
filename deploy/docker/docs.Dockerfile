# Static docs image: the compiled Docusaurus site served by Caddy on :80, fronted at
# https://switchboard.stump.wtf/docs/. The site is built on the CI runner (npm) and ONLY the static
# `docs-site/build/` output is copied in here — no Node stage — so this image build fetches nothing
# and works on a BuildKit runner with no proxy egress (unlike a from-source Node build).
#
# Build context is the repo root; `docs-site/build/` must already exist (the docs workflow runs
# `npm ci && npm run build` before `docker build`).
FROM caddy:2-alpine
COPY docs-site/build /srv
COPY deploy/docker/docs.Caddyfile /etc/caddy/Caddyfile
EXPOSE 80

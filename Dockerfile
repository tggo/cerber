# cerber — TLS-impersonation image (DOCKER ONLY).
# Inside this container, api.anthropic.com is redirected to cerber (extra_hosts),
# cerber's CA is trusted (NODE_EXTRA_CA_CERTS), and Claude Code runs here — so it
# believes it talks to first-party Anthropic and enables 1M context + tool-search.
# cerber reaches the real Anthropic via DoH (bypassing the container's hosts).

FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /cerber ./cmd/cerber

FROM node:22-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g @anthropic-ai/claude-code
COPY --from=build /cerber /usr/local/bin/cerber
COPY deploy/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
WORKDIR /work
# Claude Code reads this to trust cerber's CA; the cert is generated on first run.
ENV NODE_EXTRA_CA_CERTS=/work/certs/ca.pem
ENTRYPOINT ["/entrypoint.sh"]

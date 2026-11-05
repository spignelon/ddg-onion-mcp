# syntax=docker/dockerfile:1
#
# ddg-onion-mcp with its own Tor daemon, so `docker run` needs no other setup.
# Multi-arch (amd64, arm64): the Go binary is cross-compiled, never emulated.

ARG GO_VERSION=1.27
ARG ALPINE_VERSION=3.24

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/ddg-onion-mcp ./cmd/ddg-onion-mcp

FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache tor tini ca-certificates-bundle
COPY --from=build /out/ddg-onion-mcp /usr/local/bin/ddg-onion-mcp
COPY docker/torrc /etc/tor/torrc
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
COPY LICENSE /licenses/ddg-onion-mcp/LICENSE

# Runs as the unprivileged "tor" user created by the Alpine package.
USER tor
ENV DDG_MCP_TRANSPORT=http \
    DDG_MCP_LISTEN=0.0.0.0:8085
EXPOSE 8085
HEALTHCHECK --interval=30s --timeout=5s --start-period=150s --retries=3 \
    CMD ["ddg-onion-mcp", "--healthcheck"]
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/entrypoint.sh"]

LABEL org.opencontainers.image.title="ddg-onion-mcp" \
      org.opencontainers.image.description="MCP server: DuckDuckGo (onion service) search and page fetch, always over Tor. Bundles Tor." \
      org.opencontainers.image.source="https://github.com/spignelon/ddg-onion-mcp" \
      org.opencontainers.image.licenses="GPL-3.0-or-later"

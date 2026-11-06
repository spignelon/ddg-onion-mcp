# syntax=docker/dockerfile:1
#
# ddg-onion-mcp: a tiny image holding only the static Go binary and the CA
# certificates. It does NOT contain Tor: it connects to a Tor SOCKS proxy that
# you run (default 127.0.0.1:9050). Because 127.0.0.1 inside a container is the
# container itself, run it with `--network host` to reach the host's Tor.
# Multi-arch (amd64, arm64): the binary is cross-compiled, nothing is emulated.

ARG GO_VERSION=1.27

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
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.requireAuthDefault=true" \
    -o /out/ddg-onion-mcp ./cmd/ddg-onion-mcp

FROM scratch
# Root certificates, so the server can verify the HTTPS sites it fetches.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/ddg-onion-mcp /ddg-onion-mcp
COPY LICENSE /licenses/ddg-onion-mcp/LICENSE

# Unprivileged, numeric user (the image has no /etc/passwd).
USER 65532:65532

# With host networking this listens on the host's loopback, where every local
# process could reach it, so a bearer token is mandatory (the binary in this
# image is built with --require-auth on by default).
ENV DDG_MCP_TRANSPORT=http \
    DDG_MCP_LISTEN=127.0.0.1:8085

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD ["/ddg-onion-mcp", "--healthcheck"]
ENTRYPOINT ["/ddg-onion-mcp"]

LABEL org.opencontainers.image.title="ddg-onion-mcp" \
      org.opencontainers.image.description="MCP server: DuckDuckGo (onion service) search and page fetch, always over Tor. Uses the host's Tor (127.0.0.1:9050); run with --network host." \
      org.opencontainers.image.source="https://github.com/spignelon/ddg-onion-mcp" \
      org.opencontainers.image.licenses="GPL-3.0-or-later"

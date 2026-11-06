#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Smoke test for the container image. Usage: test/docker-smoke.sh IMAGE [--offline]
#
# The offline checks need only Docker. The rest also need a Tor SOCKS proxy on
# the host's 127.0.0.1:9050 (the image uses it through --network host) and
# curl. SMOKE_PORT (default 18999) is the loopback port the test server uses.
set -eu

IMAGE="${1:?usage: $0 IMAGE [--offline]}"
OFFLINE="${2:-}"
PORT="${SMOKE_PORT:-18999}"
fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok:   $*"; }

# 1. Refuses to start without a token (even on loopback), immediately.
set +e
out=$(docker run --rm "$IMAGE" 2>&1)
rc=$?
set -e
[ "$rc" -eq 64 ] || fail "missing token should exit 64, got $rc: $out"
echo "$out" | grep -q "token is required" || fail "missing-token message not shown: $out"
ok "refuses to start without an auth token (exit 64)"

# 2. The binary runs and accepts a valid configuration, with no network.
docker run --rm "$IMAGE" --version | grep -q "^ddg-onion-mcp " || fail "--version"
ok "binary runs"
docker run --rm -e DDG_MCP_AUTH_TOKEN=smoke-test-token-123456 "$IMAGE" --check-config >/dev/null 2>&1 \
	|| fail "--check-config rejected a valid configuration"
ok "accepts a valid configuration"

[ "$OFFLINE" = "--offline" ] && { echo "offline checks passed"; exit 0; }

# 3. HTTP mode against the host's Tor.
TOKEN=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
NAME="ddg-smoke-$$"
trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
docker run -d --name "$NAME" --network host \
	-e DDG_MCP_AUTH_TOKEN="$TOKEN" -e DDG_MCP_LISTEN="127.0.0.1:$PORT" "$IMAGE" >/dev/null
curl -s --retry 60 --retry-connrefused --retry-delay 1 --retry-all-errors -m 3 \
	"http://127.0.0.1:$PORT/healthz" >/dev/null \
	|| fail "server did not come up (is Tor running on 127.0.0.1:9050?): $(docker logs "$NAME" 2>&1 | tail -3)"
ok "server is up and passed its startup Tor check"

body='{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
mcp() { curl -s -o /dev/null -w '%{http_code}' -m 20 "http://127.0.0.1:$PORT/mcp" \
	-H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' "$@" -d "$body"; }
[ "$(mcp)" = "401" ] || fail "request without a token was not rejected"
[ "$(mcp -H "Authorization: Bearer $TOKEN")" = "200" ] || fail "request with the token was rejected"
ok "bearer token enforced"

# 4. stdio mode: stdout must carry protocol messages only.
req='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
stdout=$( { echo "$req"; sleep 12; } | docker run -i --rm --network host "$IMAGE" --transport stdio 2>/dev/null | head -n 2 )
[ -n "$stdout" ] || fail "stdio mode produced no output"
if echo "$stdout" | grep -v '^{' | grep -q .; then
	fail "non-JSON line on stdout: $(echo "$stdout" | grep -v '^{' | head -1)"
fi
echo "$stdout" | grep -q '"search"' || fail "tools/list did not return the search tool: $stdout"
ok "stdio mode: stdout is clean JSON-RPC and lists the tools"

echo "all checks passed"

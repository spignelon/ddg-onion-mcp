#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Smoke test for the container image. Usage: test/docker-smoke.sh IMAGE [--offline]
# --offline skips the checks that need to reach the Tor network.
set -eu

IMAGE="${1:?usage: $0 IMAGE [--offline]}"
OFFLINE="${2:-}"
fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok:   $*"; }

# 1. Refuses to start exposed without a token, fast and with the right code.
set +e
out=$(docker run --rm "$IMAGE" 2>&1)
rc=$?
set -e
[ "$rc" -eq 64 ] || fail "missing token should exit 64, got $rc"
echo "$out" | grep -q "bearer token" || fail "missing-token message not shown: $out"
ok "refuses to start without an auth token (exit 64)"

# 2. The binary runs without any network.
docker run --rm --entrypoint ddg-onion-mcp "$IMAGE" --version | grep -q "^ddg-onion-mcp " || fail "--version"
ok "binary runs"
[ "$OFFLINE" = "--offline" ] && { echo "offline checks passed"; exit 0; }

# 3. stdio mode: stdout must carry protocol messages only (no Tor banner).
req='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
stdout=$( { echo "$req"; } | { cat; sleep 45; } | docker run -i --rm "$IMAGE" --transport stdio 2>/dev/null | head -n 2 )
[ -n "$stdout" ] || fail "stdio mode produced no output"
if echo "$stdout" | grep -v '^{' | grep -q .; then
	fail "non-JSON line on stdout: $(echo "$stdout" | grep -v '^{' | head -1)"
fi
echo "$stdout" | grep -q '"search"' || fail "tools/list did not return the search tool: $stdout"
ok "stdio mode: stdout is clean JSON-RPC and lists the tools"

echo "all checks passed"

#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Container entrypoint: validate the configuration, start Tor, wait for it to
# bootstrap, then run ddg-onion-mcp. If either process exits, stop the other
# and exit, so a Docker restart policy can bring the whole thing back.
set -eu

log() { echo "entrypoint: $*" >&2; }

# 1. Fail fast on configuration mistakes (for example a missing auth token),
#    before spending time on Tor bootstrap.
ddg-onion-mcp --check-config "$@" || {
	log "invalid configuration (see the message above)"
	exit 64
}

# 2. Start Tor.
# Tor prints a startup banner to stdout before its Log settings apply. stdout
# belongs to the MCP protocol in stdio mode, so send Tor's output to stderr.
rm -f /tmp/tor-notices.log
tor -f /etc/tor/torrc >&2 &
TOR_PID=$!

# 3. Wait until Tor can build circuits.
timeout="${TOR_BOOTSTRAP_TIMEOUT:-180}"
waited=0
until grep -q 'Bootstrapped 100%' /tmp/tor-notices.log 2>/dev/null; do
	if ! kill -0 "$TOR_PID" 2>/dev/null; then
		log "Tor exited before it finished bootstrapping"
		exit 1
	fi
	if [ "$waited" -ge "$timeout" ]; then
		log "Tor did not bootstrap within ${timeout}s"
		kill -TERM "$TOR_PID" 2>/dev/null || true
		exit 1
	fi
	waited=$((waited + 1))
	sleep 1
done
log "Tor is ready after ${waited}s"

# 4. Run the server. Stdio mode needs the real stdin, but a shell gives
#    background jobs /dev/null (even with "<&0" in BusyBox ash), so hand it
#    over through a duplicated file descriptor.
exec 3<&0
ddg-onion-mcp "$@" <&3 &
APP_PID=$!
exec 3<&-

stop() { kill -TERM "$APP_PID" "$TOR_PID" 2>/dev/null || true; }
trap stop TERM INT

# 5. Supervise.
while kill -0 "$APP_PID" 2>/dev/null && kill -0 "$TOR_PID" 2>/dev/null; do
	sleep 1
done

status=0
if kill -0 "$APP_PID" 2>/dev/null; then
	log "Tor exited unexpectedly; stopping the server"
	status=1
fi
stop
wait "$APP_PID" 2>/dev/null || { rc=$?; [ "$status" -ne 0 ] || status=$rc; }
wait "$TOR_PID" 2>/dev/null || true
exit "$status"

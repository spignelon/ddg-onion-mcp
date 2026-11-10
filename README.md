<h1 align="center">ddg-onion-mcp</h1>

An [MCP](https://modelcontextprotocol.io) server that gives an AI client web search and page
reading, **entirely over Tor**: search goes through DuckDuckGo's **Lite onion service**, and pages
are fetched and reduced to clean Markdown. There is no direct-connection fallback. Single Go
binary, no external accounts or API keys.

Lightweight: Docker image is **~12 MB**, container idles at **~20 MB RAM**.

> Same job as `nickclyde/duckduckgo-mcp-server` and the Brave Search MCP server, for people who
> want the traffic to leave through Tor and the server to be tiny.

---

## ⚠️ Read before you deploy this

- **Search queries are not private from your model provider.** Tor hides your IP from DuckDuckGo
  and from the sites you fetch. The AI service that writes the queries (OpenRouter, OpenAI, ...)
  still sees them.
- **Tor does not make this indistinguishable from Tor Browser.** Only the HTTP request headers are
  matched. The TLS and HTTP/2 fingerprints are still Go's, so a site that compares them can tell
  (see [Request identity](#request-identity)).
- **Fetched pages are untrusted input.** A page can contain text that tries to instruct your
  model. Both tools are marked read-only and say so, but treat the output accordingly.
- **No bot-detection bypass.** If DuckDuckGo or a site refuses or challenges the client, the server
  reports an error. It does not retry under another identity and does not solve challenges.
  Respect the terms of the sites you query.

---

## Quick start (Docker)

The image does **not** contain Tor. It needs a **Tor service running on the host OS**, and uses its
SOCKS proxy at `127.0.0.1:9050` (the default of the distribution packages). Install and start it
first:

| Distribution | Install and start |
|---|---|
| Debian / Ubuntu | `sudo apt install tor && sudo systemctl enable --now tor` |
| Arch | `sudo pacman -S tor && sudo systemctl enable --now tor` |
| Fedora | `sudo dnf install tor && sudo systemctl enable --now tor` |
| openSUSE | `sudo zypper install tor && sudo systemctl enable --now tor` |

Then check that it is listening: `ss -tln | grep 9050` should show `127.0.0.1:9050`. (The `tor`
service name and port were confirmed on openSUSE Tumbleweed; the other rows are the standard
package names and have not been run by the author.) Without this service the container refuses to
start, see "No Tor, no fallback" below.

Inside a normal container `127.0.0.1` is the container itself, so run it with **`--network host`**.
No repo clone needed:

```bash
export DDG_MCP_AUTH_TOKEN=$(openssl rand -hex 24)
echo "$DDG_MCP_AUTH_TOKEN"        # keep this: your MCP client needs it as a bearer token

docker run -d --name ddg-onion-mcp --restart unless-stopped \
  --network host \
  -e DDG_MCP_AUTH_TOKEN \
  ghcr.io/spignelon/ddg-onion-mcp:latest
```

**Alternative: clone the repo and use `compose.yaml`** (hardened: read-only filesystem, all
capabilities dropped, 128 MB memory cap):

```bash
git clone https://github.com/spignelon/ddg-onion-mcp.git && cd ddg-onion-mcp
export DDG_MCP_AUTH_TOKEN=$(openssl rand -hex 24)
docker compose up -d
```

The MCP endpoint is `http://127.0.0.1:8085/mcp` with the header `Authorization: Bearer <token>`.
With host networking nothing is published with `-p`: the server listens directly on the host's
loopback (change it with `-e DDG_MCP_LISTEN=host:port`).

**Desktop MCP clients (stdio):** have the client run the image itself:

```json
{ "mcpServers": { "ddg-onion": { "command": "docker",
    "args": ["run", "-i", "--rm", "--network", "host", "ghcr.io/spignelon/ddg-onion-mcp:latest", "--transport", "stdio"] } } }
```

Update with `docker pull ghcr.io/spignelon/ddg-onion-mcp:latest` and recreate the container. Build
it yourself with `docker build -t ddg-onion-mcp .`.

### Docker notes

- **A token is mandatory in the image**, even on loopback, because with host networking every
  local process could otherwise reach the server. Without `DDG_MCP_AUTH_TOKEN` the container
  refuses to start (exit code 64, immediately). `-e DDG_MCP_REQUIRE_AUTH=false` or
  `--insecure-no-auth` disables the requirement.
- **No Tor, no fallback.** If the proxy isn't reachable the server fails closed within a second and
  says so; forgetting `--network host` produces exactly that error, with a hint. The startup check
  retries transient failures but refuses to run if Tor reports that traffic is not leaving through
  Tor.
- **Hardened variant:** add `--read-only --cap-drop ALL --security-opt no-new-privileges:true
  --memory 128m --pids-limit 64`. This is how it was tested; no volume or `/tmp` is needed. The
  container runs as an unprivileged user (uid 65532).
- **Options** are the flags and environment variables in the [table below](#flags-and-environment),
  for example `... ghcr.io/spignelon/ddg-onion-mcp:latest --identity honest`. The health check reads
  `DDG_MCP_LISTEN` and `DDG_TOR_PROXY` from the environment, so set those with `-e`, not as flags.
  It probes the address the server listens on (for example the Docker bridge address), or
  `127.0.0.1` for a wildcard address such as `0.0.0.0`.
- **A different Tor location.** To use a Tor proxy elsewhere (for example one bound to the Docker
  bridge address, instead of `--network host`), set `-e DDG_TOR_PROXY=host:port` and pass
  `--allow-remote-tor-proxy`; a proxy that is not on loopback may not be Tor, so this is an explicit
  opt-in. This variant has not been tested.
- **Tags:** `latest` and `main` follow the main branch, `X.Y.Z` and `X.Y` follow version tags, and
  `sha-<commit>` pins an exact build. Images are built for `linux/amd64` and `linux/arm64`; the
  arm64 image is built by CI but has not been run by the author. Older images that bundled their own
  Tor (up to `sha-622bbc0`) are no longer what `latest` points to.
- **Size and memory** (hardened container, measured): about 20 MiB resident at idle (11.6 MiB
  charged to the container's cgroup), 23 MiB after a 100,000-character page, and 57 MiB peak after
  fetching eight large pages at once with the rate limiter disabled. The Tor daemon on the host is
  separate.

## What it does

Two tools, both read-only:

| Tool | Use case |
|---|---|
| **`search`** | Web search through DuckDuckGo Lite's onion service. Returns titles, URLs and snippets. |
| **`fetch_content`** | Downloads a page and returns its main content as Markdown or text, with paging for long pages. |

### `search`

| Parameter | Type | Notes |
|---|---|---|
| `query` | string, required | up to 400 characters |
| `max_results` | integer | 1–20, default 10 |
| `region` | string | e.g. `us-en`, `de-de`, `wt-wt` (no region) |
| `safe_search` | string | `off`, `moderate` (default), `strict` |
| `time_range` | string | `d`, `w`, `m`, `y` |

Sponsored results are dropped and DuckDuckGo's redirect links are unwrapped to the real
destination. The query is sent in a POST body, never in a URL.

### `fetch_content`

| Parameter | Type | Notes |
|---|---|---|
| `url` | string, required | `http(s)` only |
| `start_index` | integer | character offset, for paging long pages |
| `max_length` | integer | 100–50000, default 8000 |
| `parse_mode` | string | `markdown` (default), `text`, `full` |

Extraction follows the recipe of the official MCP `fetch` server: Mozilla's **Readability**
algorithm (the Go port from [Readeck](https://codeberg.org/readeck/go-readability)) finds the main
article, then [html-to-markdown](https://github.com/JohannesKaufmann/html-to-markdown) renders it.
If no article is found (forums, link lists) the whole page is converted minus navigation and
footers. `parse_mode=full` skips Readability. Fetched pages are cached for 5 minutes so paging
through a long page downloads it once.

## Key features

- **Tor only, verified.** Every connection goes through the Tor SOCKS5 proxy, host names are sent to
  Tor unresolved (no local DNS), and the server refuses to start unless the Tor Project's check
  service confirms traffic leaves through Tor. See [How "Tor only" is enforced](#how-tor-only-is-enforced).
- **Tor Browser request headers** by default (Firefox 153 on Linux), with Brotli and Zstandard
  decoding. See [Request identity](#request-identity).
- **Circuit isolation:** one random SOCKS credential per process (default) or per connection.
- **SSRF guard:** `fetch_content` refuses loopback, private, link-local and metadata addresses and
  re-validates every redirect hop. `.onion` pages can be fetched.
- **Two transports:** stdio for desktop clients, Streamable HTTP (stateless, bearer token,
  `Origin` check, `GET /healthz`) for servers.
- **Rate limits, caching, size caps** on everything, and logs that contain hosts and counts only,
  never queries or URLs.
- Works with [Open WebUI, LibreChat and DEEIX Chat](#using-it-with-open-webui-librechat-and-deeix-chat), and any client that speaks MCP.

## Architecture, at a glance

Go 1.26, the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk), and
`golang.org/x/net` for the SOCKS5 dialer and HTML parsing. No cgo, so the Docker image is a
`FROM scratch` image holding one static binary (about 11 MB) and the CA certificates; there is no
shell and no Tor inside.

```
cmd/ddg-onion-mcp/       flags, transports, startup checks
internal/torhttp/        Tor-only HTTP client: SOCKS5, retries, identity profile, decoders
internal/ddg/            DuckDuckGo Lite search and result parser
internal/fetch/          URL validation, redirect handling, page cache
internal/extract/        Readability + Markdown conversion
internal/mcpserver/      tool definitions
internal/limits/         rate limiting
```

## How "Tor only" is enforced

- Every connection is opened through the Tor SOCKS5 proxy. `HTTP_PROXY`/`ALL_PROXY` are ignored.
- Host names are sent to Tor **unresolved** (SOCKS5 domain-name address type). This machine never
  does a DNS lookup for a destination, and URL validation works on the literal URL only.
- If the proxy is down, requests fail. The server also refuses to start unless the Tor Project's
  check service confirms traffic leaves through Tor (`--skip-tor-check` to override).
- The proxy must be on loopback unless you pass `--allow-remote-tor-proxy`.
- Circuit isolation: one random SOCKS credential per process (`--isolation session`, default) or
  per connection (`--isolation request`).
- `fetch_content` refuses non-public targets (loopback, RFC 1918, link-local/metadata, CGNAT,
  ambiguous numeric hosts such as `2130706433` or `0x7f.1`, `*.local`, `*.internal`, ...), and
  re-validates every redirect hop. `.onion` pages can be fetched (`--allow-onion-fetch=false` to
  disable).

Verified, not just designed: unit tests use a fake SOCKS5 server to check that names arrive
unresolved, that nothing is contacted directly when the proxy is down, and that proxy environment
variables are ignored. A one-off `strace -f -e trace=connect,sendto,sendmsg` audit of the real
binary (startup check + a search, and startup check + a page fetch, against a live Tor) showed
exactly one connection per request and **every connection went to 127.0.0.1:9050, with no DNS
traffic**.

## Request identity

By default the server sends the request headers of a Tor Browser page navigation, taken from a real
capture (Firefox 153 on Linux, from <https://myip.wtf/headers>):

```
User-Agent: Mozilla/5.0 (X11; Linux x86_64; rv:153.0) Gecko/20100101 Firefox/153.0
Accept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8
Accept-Language: en-US,en;q=0.9
Accept-Encoding: gzip, deflate, br, zstd
Upgrade-Insecure-Requests: 1
Sec-GPC: 1
TE: trailers
Priority: u=0, i
```

plus `Sec-Fetch-Dest: document`, `Sec-Fetch-Mode: navigate`, `Sec-Fetch-User: ?1` and a
`Sec-Fetch-Site` that fits the request: `none` for a fetched page, `same-origin` for a search (a
form submission from DuckDuckGo's own page, which also sends `Origin` and `Referer`) and for a
same-site redirect, `cross-site` for a redirect to another site. Because `Accept-Encoding` offers
`br` and `zstd`, the client decodes gzip, deflate, Brotli and Zstandard itself (Zstandard window
capped at 8 MiB).

**These values go stale.** Tor Browser changes version about every four weeks. To refresh them,
capture new headers and put the changed ones in a JSON file:

```json
{ "User-Agent": "Mozilla/5.0 (X11; Linux x86_64; rv:154.0) Gecko/20100101 Firefox/154.0" }
```

then run with `--identity-file that.json` (an empty value removes a header). `--user-agent`
overrides just the User-Agent. `--identity honest` sends only a `ddg-onion-mcp/<version>`
User-Agent and an `Accept`. `Host`, `Cookie`, `Authorization`, `Origin`, `Referer` and other
protocol or credential headers cannot be set from a file.

**What this is not.** A profile only changes HTTP request headers. The TLS handshake, HTTP/2
settings and header order still look like a Go program, not like Firefox, so traffic is *not*
indistinguishable from a real Tor Browser, and a site that compares the two can tell. No TLS
impersonation is included.

## Build and run from source

Requirements: Go 1.26+ to build, and a running Tor with its SOCKS port on `127.0.0.1:9050` (install
and start it as in the [Quick start](#quick-start-docker) table).

```bash
go build -trimpath -o ddg-onion-mcp ./cmd/ddg-onion-mcp

./ddg-onion-mcp                         # stdio, for desktop MCP clients

export DDG_MCP_AUTH_TOKEN=$(openssl rand -hex 24)
./ddg-onion-mcp --transport http --listen 127.0.0.1:8085   # Streamable HTTP at /mcp
```

Stdio client config (Claude Desktop style):

```json
{ "mcpServers": { "ddg-onion": { "command": "/path/to/ddg-onion-mcp" } } }
```

A hardened systemd unit is in `deploy/ddg-onion-mcp.service`. Its syntax was checked with
`systemd-analyze verify`, but it has **not been run on a real server**.

### Flags and environment

| Flag | Env | Default |
|---|---|---|
| `--transport` | `DDG_MCP_TRANSPORT` | `stdio` (or `http`; `http` in the image) |
| `--listen` | `DDG_MCP_LISTEN` | `127.0.0.1:8085` |
| `--auth-token-file` | `DDG_MCP_AUTH_TOKEN_FILE` | none; or set `DDG_MCP_AUTH_TOKEN` (min 16 chars) |
| `--require-auth` | `DDG_MCP_REQUIRE_AUTH` | off; **on in the Docker image** |
| `--allowed-origins` | `DDG_MCP_ALLOWED_ORIGINS` | none (loopback origins always allowed) |
| `--tor-proxy` | `DDG_TOR_PROXY` | `127.0.0.1:9050` |
| `--isolation` | `DDG_TOR_ISOLATION` | `session` |
| `--identity` | `DDG_IDENTITY` | `tor-browser` (or `honest`) |
| `--identity-file` | `DDG_IDENTITY_FILE` | none; JSON header overrides |
| `--user-agent` | `DDG_USER_AGENT` | none; overrides only the User-Agent |
| `--ddg-url` | `DDG_URL` | the Lite onion URL |
| `--region`, `--safe-search` | `DDG_REGION`, `DDG_SAFE_SEARCH` | none, `moderate` |
| `--search-rpm`, `--fetch-rpm` | | 20 / 20 |
| `--cache-ttl` | | 5m |
| `--max-bytes` | | 2 MiB per download |
| `--check-config`, `--healthcheck` | | validate configuration and exit; probe a running server |

HTTP mode refuses to listen on a non-loopback address without a bearer token. The bearer check is
constant-time, requests with a foreign `Origin` header are rejected, and sessions are stateless to
keep memory low. `GET /healthz` returns `ok` without touching Tor. Configuration mistakes exit with
code 64.

## Using it with Open WebUI, LibreChat and DEEIX Chat

All three speak MCP over Streamable HTTP, so they use the same endpoint and bearer token:
`http://<host>:8085/mcp` with `Authorization: Bearer <DDG_MCP_AUTH_TOKEN>`.

**Where the chat app runs decides `<host>`:**

- **On the host itself** (not in Docker): use `127.0.0.1`. Nothing else to do.
- **In a Docker container** (the usual case): the container cannot reach the host's loopback, so
  start this server listening on the Docker bridge address and point the app at the host gateway:

  ```bash
  BRIDGE=$(ip -4 -o addr show docker0 | awk '{print $4}' | cut -d/ -f1)     # usually 172.17.0.1
  docker run -d --name ddg-onion-mcp --restart unless-stopped --network host \
    -e DDG_MCP_AUTH_TOKEN -e DDG_MCP_LISTEN=$BRIDGE:8085 ghcr.io/spignelon/ddg-onion-mcp:latest
  ```

  Then use `http://host.docker.internal:8085/mcp` in the app. On Linux the app's container needs
  `extra_hosts: ["host.docker.internal:host-gateway"]` (or `--add-host
  host.docker.internal:host-gateway`).

Some hosts firewall container-to-host traffic (on one Oracle Cloud Ubuntu VPS, containers got
`EHOSTUNREACH` for a service on the host's Docker bridge address); there, allow the bridge to reach
port 8085, or run both with host networking.

### DEEIX Chat

Admin console → add an MCP server with the URL above and the token as its auth token → *Sync*.

**Tested:** a DEEIX container reached this image over the host gateway and synced both tools
(`active`, no error, schemas intact). **Not tested:** a tool call from a real chat, since that needs
a model and API key.

### Open WebUI

Admin Settings → External Tools → add a server, type **MCP (Streamable HTTP)**, enter the URL,
set authentication to **Bearer** and put the token in the **Key** field (leaving it empty sends an
empty `Authorization` header). Save, then enable the tools for a chat. Only admins can add MCP
servers.

**Not tested** by the author: this follows the
[Open WebUI MCP documentation](https://docs.openwebui.com/features/extensibility/mcp).

### LibreChat

Add the server to `librechat.yaml` and restart LibreChat:

```yaml
mcpServers:
  ddg-onion:
    type: streamable-http
    url: http://host.docker.internal:8085/mcp
    headers:
      Authorization: "Bearer ${DDG_MCP_AUTH_TOKEN}"   # set DDG_MCP_AUTH_TOKEN in LibreChat's .env

mcpSettings:
  allowedAddresses:
    - "host.docker.internal:8085"
```

LibreChat blocks private and internal addresses for MCP servers by default, so a server on the host
needs `allowedAddresses` (with the port). Leave `allowedDomains` unset: it is a strict whitelist and
would block every public destination you do not list.

**Not tested** by the author: this follows the
[LibreChat MCP server](https://www.librechat.ai/docs/configuration/librechat_yaml/object_structure/mcp_servers)
and [MCP settings](https://www.librechat.ai/docs/configuration/librechat_yaml/object_structure/mcp_settings)
documentation.

## Limitations (read these)

- **No JavaScript.** Pages that build their content in the browser come back nearly empty. The
  tool flags this; try `parse_mode=full` or another source. A headless browser was ruled out for
  RAM and Tor-speed reasons. PDFs and other binary content cannot be read.
- **DuckDuckGo may refuse automated clients.** It rejected a stock `curl` User-Agent with
  `406 Not Acceptable` (a controlled test showed the User-Agent alone decides it) while accepting
  both the honest `ddg-onion-mcp/...` identity and the Tor Browser profile. A refusal or challenge
  page is reported as an error; the server never retries under another identity. One `403` was
  seen from `example.com` over one Tor circuit during testing and did not recur in 16 further
  fetches (8 per identity, fresh circuits), so it could not be attributed to the identity.
- **The result parser depends on DuckDuckGo Lite's markup.** It is unit-tested against a synthetic
  fixture modelled on that markup and was checked live on a few queries; a markup change can
  break it.
- **Tor is slow:** expect 2–10 seconds per call. Onion services can be slow to reach, especially from
  a freshly started Tor: when Tor cannot connect to a destination the request is retried on a
  fresh circuit, up to 3 attempts of at most 45 s each (only failures before any request data was
  sent are retried). In a stress test of an earlier image that bundled its own Tor (12 containers
  cold-started at once, each bootstrapping a fresh Tor, which a long-running host Tor usually
  avoids), 10 first searches succeeded (two only after a retry, at about 48 s), one got
  DuckDuckGo's rate limit (`HTTP 202`) and one failed after all attempts (135 s). Starting many
  fresh clients from one IP at once makes DuckDuckGo rate-limit you.
- Not implemented compared with `nickclyde/duckduckgo-mcp-server`: `ref://` link shortening and
  `expand_link`, TLS-fingerprint impersonation backends (deliberately omitted: only HTTP headers are
  profiled), per-host fetch limits.

## Development

```bash
go vet ./... && go test ./... -race
go test -tags live ./test/live -v     # needs Tor on 127.0.0.1:9050 and network access
test/docker-smoke.sh ddg-onion-mcp    # image checks; add --offline to skip the ones that need Tor
```

`LIVE_WRAP` lets the live tests run the binary under an audit tool, for example
`LIVE_WRAP="strace -f -qq -e trace=connect,sendto,sendmsg -o /tmp/trace.txt"`.

## Credits

Design inspired by [nickclyde/duckduckgo-mcp-server](https://github.com/nickclyde/duckduckgo-mcp-server)
and [brave/brave-search-mcp-server](https://github.com/brave/brave-search-mcp-server) (no code
copied). Built on the [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk),
[go-readability](https://codeberg.org/readeck/go-readability) and
[html-to-markdown](https://github.com/JohannesKaufmann/html-to-markdown).

## License

GPL-3.0-or-later. See [`LICENSE`](LICENSE).

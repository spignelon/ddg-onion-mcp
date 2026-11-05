# ddg-onion-mcp

An [MCP](https://modelcontextprotocol.io) server that gives an AI client two tools:

- **`search`**: web search through DuckDuckGo's **Lite onion service**.
- **`fetch_content`**: download a web page and return its main content as clean Markdown or text.

**All traffic goes through Tor.** There is no direct-connection fallback. Written in Go; a single
~11 MB binary using **about 22 MB of RAM idle (25 MB peak** after fetching a 100k-character page).

Licensed under **GPL-3.0-or-later** (see `LICENSE`).

## Tools

### `search`

| Parameter | Type | Notes |
|---|---|---|
| `query` | string, required | up to 400 characters |
| `max_results` | integer | 1–20, default 10 |
| `region` | string | e.g. `us-en`, `de-de`, `wt-wt` (no region) |
| `safe_search` | string | `off`, `moderate` (default), `strict` |
| `time_range` | string | `d`, `w`, `m`, `y` |

Returns titles, URLs and snippets. Sponsored results are dropped and DuckDuckGo's redirect links
are unwrapped to the real destination. The query is sent in a POST body, never in a URL.

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

Both tools are marked read-only and carry a warning that their output is untrusted web content.

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
impersonation is included. If DuckDuckGo or a site challenges the client, the server reports it as
an error: it does not retry under another identity and does not try to solve or bypass challenges.

## Install and run

Requirements: Go 1.26+ to build, and a running Tor with its SOCKS port on `127.0.0.1:9050`
(Debian/Ubuntu: `apt install tor`).

```sh
go build -trimpath -o ddg-onion-mcp ./cmd/ddg-onion-mcp

./ddg-onion-mcp                         # stdio, for desktop MCP clients

export DDG_MCP_AUTH_TOKEN=$(openssl rand -hex 24)
./ddg-onion-mcp --transport http --listen 127.0.0.1:8085   # Streamable HTTP at /mcp
```

Stdio client config (Claude Desktop style):

```json
{ "mcpServers": { "ddg-onion": { "command": "/path/to/ddg-onion-mcp" } } }
```

### Flags and environment

| Flag | Env | Default |
|---|---|---|
| `--transport` | `DDG_MCP_TRANSPORT` | `stdio` (or `http`) |
| `--listen` | `DDG_MCP_LISTEN` | `127.0.0.1:8085` |
| `--auth-token-file` | `DDG_MCP_AUTH_TOKEN_FILE` | none; or set `DDG_MCP_AUTH_TOKEN` (min 16 chars) |
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

HTTP mode refuses to listen on a non-loopback address without a bearer token. The bearer check is
constant-time, requests with a foreign `Origin` header are rejected, and sessions are stateless to
keep memory low. `GET /healthz` returns `ok` without touching Tor. Logs go to stderr and contain
hosts and counts only, never queries or URLs.

## Using it with DEEIX Chat

DEEIX Chat accepts remote MCP servers over Streamable HTTP. In the admin console add an MCP server
with the URL `http://<host>:8085/mcp` and the bearer token as its auth token, then run *Sync*.

Tested: a DEEIX Chat container reached this server over the Docker host gateway
(`host.docker.internal`, with the server listening on the `docker0` address and a bearer token).
Sync discovered both tools, both `active`, no error, with their schemas intact. **Not tested:** a
tool call made from a real chat, since that needs a model and API key.

**VPS caveat (observed on the target VPS earlier in this project):** containers there could not
reach a service on the host's Docker bridge address (`EHOSTUNREACH`, host firewall rules). Either
allow that traffic or run Tor and this server as containers on DEEIX's compose network. A ready
Dockerfile/compose file is **not provided yet**.

A hardened systemd unit is in `deploy/ddg-onion-mcp.service`. Its syntax was checked with
`systemd-analyze verify`, but it has **not been run on a real server**.

## Limitations (read these)

- **No JavaScript.** Pages that build their content in the browser come back nearly empty. The
  tool flags this; try `parse_mode=full` or another source. A headless browser was ruled out for
  RAM and Tor-speed reasons.
- **DuckDuckGo may refuse automated clients.** It rejected a stock `curl` User-Agent with
  `406 Not Acceptable` (a controlled test showed the User-Agent alone decides it) while accepting
  both the honest `ddg-onion-mcp/...` identity and the Tor Browser profile. A refusal or challenge
  page is reported as an error; the server never retries under another identity. One `403` was
  seen from `example.com` over one Tor circuit during testing and did not recur in 16 further
  fetches (8 per identity, fresh circuits), so it could not be attributed to the identity.
- **The result parser depends on DuckDuckGo Lite's markup.** It is unit-tested against a synthetic
  fixture modelled on that markup and was checked live on a few queries; a markup change can
  break it.
- Tor is slow: expect 2–10 seconds per call. No PDFs or other binary content.
- Not implemented compared with `nickclyde/duckduckgo-mcp-server`: `ref://` link shortening and
  `expand_link`, TLS-fingerprint impersonation backends (deliberately omitted: only HTTP headers are
  profiled), per-host fetch limits.

## Development

```sh
go vet ./... && go test ./... -race
go test -tags live ./test/live -v     # needs Tor on 127.0.0.1:9050 and network access
```

`LIVE_WRAP` lets the live tests run the binary under an audit tool, for example
`LIVE_WRAP="strace -f -qq -e trace=connect,sendto,sendmsg -o /tmp/trace.txt"`.

## Credits and third-party licences

The design (tool shape, paging, caching, rate limits, transport options) was informed by
[nickclyde/duckduckgo-mcp-server](https://github.com/nickclyde/duckduckgo-mcp-server) and
[brave/brave-search-mcp-server](https://github.com/brave/brave-search-mcp-server), both MIT. No
code was copied from either. Dependencies: the official
[MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) (MIT, transitioning to Apache-2.0),
`go-readability` (MIT), `html-to-markdown` (MIT), `golang.org/x/net` (BSD-3-Clause),
`andybalholm/brotli` (MIT) and `klauspost/compress` (BSD-3-Clause), all compatible with GPL-3.0.

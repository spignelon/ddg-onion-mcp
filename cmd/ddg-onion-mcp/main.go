// SPDX-License-Identifier: GPL-3.0-or-later

// Command ddg-onion-mcp is an MCP server that searches DuckDuckGo through its
// onion service and reads web pages, always over Tor.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/spignelon/ddg-onion-mcp/internal/ddg"
	"github.com/spignelon/ddg-onion-mcp/internal/fetch"
	"github.com/spignelon/ddg-onion-mcp/internal/limits"
	"github.com/spignelon/ddg-onion-mcp/internal/mcpserver"
	"github.com/spignelon/ddg-onion-mcp/internal/torhttp"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ddg-onion-mcp:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("ddg-onion-mcp", flag.ContinueOnError)
	var (
		transport    = fs.String("transport", env("DDG_MCP_TRANSPORT", "stdio"), "stdio or http (Streamable HTTP)")
		listen       = fs.String("listen", env("DDG_MCP_LISTEN", "127.0.0.1:8085"), "HTTP listen address")
		tokenFile    = fs.String("auth-token-file", env("DDG_MCP_AUTH_TOKEN_FILE", ""), "file holding the bearer token for HTTP (or set DDG_MCP_AUTH_TOKEN)")
		noAuth       = fs.Bool("insecure-no-auth", false, "allow a non-loopback HTTP listener without a bearer token (not recommended)")
		origins      = fs.String("allowed-origins", env("DDG_MCP_ALLOWED_ORIGINS", ""), "comma-separated extra Origin values allowed over HTTP (loopback always allowed)")
		stateless    = fs.Bool("stateless", true, "stateless HTTP sessions (lower memory)")
		torProxy     = fs.String("tor-proxy", env("DDG_TOR_PROXY", "127.0.0.1:9050"), "Tor SOCKS5 proxy address")
		remoteTor    = fs.Bool("allow-remote-tor-proxy", false, "allow a non-loopback Tor proxy")
		isolation    = fs.String("isolation", env("DDG_TOR_ISOLATION", "session"), "Tor circuit isolation: session or request")
		ddgURL       = fs.String("ddg-url", env("DDG_URL", ddg.DefaultBaseURL), "DuckDuckGo Lite URL")
		identity     = fs.String("identity", env("DDG_IDENTITY", "tor-browser"), "request headers to send: tor-browser (mirrors Tor Browser) or honest (names this program)")
		identityFile = fs.String("identity-file", env("DDG_IDENTITY_FILE", ""), "JSON object of header overrides applied on top of the identity (refresh a stale Tor Browser version)")
		userAgent    = fs.String("user-agent", env("DDG_USER_AGENT", ""), "override just the User-Agent header")
		region       = fs.String("region", env("DDG_REGION", ""), "default search region, e.g. us-en")
		safeSearch   = fs.String("safe-search", env("DDG_SAFE_SEARCH", ""), "default safe search: off, moderate or strict")
		searchRPM    = fs.Int("search-rpm", 20, "search requests per minute (0 = unlimited)")
		fetchRPM     = fs.Int("fetch-rpm", 20, "page fetches per minute (0 = unlimited)")
		cacheTTL     = fs.Duration("cache-ttl", 5*time.Minute, "how long fetched pages are cached (0 = off)")
		maxBytes     = fs.Int64("max-bytes", 2<<20, "largest page download in bytes")
		allowOnion   = fs.Bool("allow-onion-fetch", true, "let fetch_content read .onion sites")
		skipTorCheck = fs.Bool("skip-tor-check", false, "do not verify Tor at startup")
		verbose      = fs.Bool("verbose", false, "debug logging (stderr)")
		checkConfig  = fs.Bool("check-config", false, "validate the configuration without starting anything, then exit")
		healthcheck  = fs.Bool("healthcheck", false, "probe a running HTTP server and the Tor proxy, then exit (for Docker HEALTHCHECK)")
		showVersion  = fs.Bool("version", false, "print the version and exit")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println("ddg-onion-mcp", version)
		return nil
	}

	if *healthcheck {
		return probe(*listen, *torProxy)
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	// Logs go to stderr: stdout belongs to the MCP protocol in stdio mode.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if *transport != "stdio" && *transport != "http" {
		return fmt.Errorf("--transport must be stdio or http, got %q", *transport)
	}

	iso := torhttp.IsolateSession
	switch *isolation {
	case "session":
	case "request":
		iso = torhttp.IsolateRequest
	default:
		return fmt.Errorf("--isolation must be session or request, got %q", *isolation)
	}
	profile, err := buildProfile(*identity, *identityFile, *userAgent)
	if err != nil {
		return err
	}
	client, err := torhttp.New(torhttp.Config{
		ProxyAddr:        *torProxy,
		AllowRemoteProxy: *remoteTor,
		Isolation:        iso,
		Profile:          profile,
	})
	if err != nil {
		return err
	}

	if *checkConfig {
		if *transport == "http" {
			token, err := loadToken(*tokenFile)
			if err != nil {
				return err
			}
			if err := validateHTTP(*listen, token, *noAuth); err != nil {
				return err
			}
		}
		fmt.Fprintln(os.Stderr, "configuration OK")
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if !*skipTorCheck {
		cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
		err := client.Check(cctx)
		cancel()
		if err != nil {
			return fmt.Errorf("startup Tor check failed: %w (start Tor, or pass --skip-tor-check)", err)
		}
		logger.Info("Tor check passed", "proxy", *torProxy)
	}
	logger.Info("request identity", "profile", client.ProfileName())

	srv := mcpserver.New(mcpserver.Config{
		Version:           version,
		Search:            &ddg.Client{HTTP: client, BaseURL: *ddgURL},
		Fetch:             &fetch.Fetcher{Client: client, MaxBytes: *maxBytes, AllowOnion: *allowOnion},
		SearchLimiter:     limits.NewLimiter(*searchRPM),
		FetchLimiter:      limits.NewLimiter(*fetchRPM),
		Cache:             limits.NewCache[*fetch.Page](*cacheTTL, 32),
		DefaultRegion:     *region,
		DefaultSafeSearch: *safeSearch,
		Logger:            logger,
	})

	switch *transport {
	case "stdio":
		logger.Info("serving MCP over stdio", "version", version)
		err := srv.Run(ctx, &mcp.StdioTransport{})
		if err != nil && ctx.Err() == nil {
			return err
		}
		return nil
	case "http":
		token, err := loadToken(*tokenFile)
		if err != nil {
			return err
		}
		return serveHTTP(ctx, logger, srv, httpConfig{
			addr: *listen, token: token, noAuth: *noAuth,
			origins: splitList(*origins), stateless: *stateless,
		})
	}
	return fmt.Errorf("--transport must be stdio or http, got %q", *transport)
}

// buildProfile resolves --identity, --identity-file and --user-agent.
func buildProfile(identity, file, userAgent string) (torhttp.Profile, error) {
	var p torhttp.Profile
	switch identity {
	case "tor-browser":
		p = torhttp.TorBrowserProfile()
	case "honest":
		p = torhttp.HonestProfile("ddg-onion-mcp/" + version)
	default:
		return p, fmt.Errorf("--identity must be tor-browser or honest, got %q", identity)
	}
	var err error
	if file != "" {
		if p, err = p.OverrideFromFile(file); err != nil {
			return p, err
		}
	}
	if userAgent != "" {
		if p, err = p.With(map[string]string{"User-Agent": userAgent}); err != nil {
			return p, err
		}
	}
	return p, nil
}

type httpConfig struct {
	addr      string
	token     string
	noAuth    bool
	origins   []string
	stateless bool
}

func serveHTTP(ctx context.Context, logger *slog.Logger, srv *mcp.Server, c httpConfig) error {
	if err := validateHTTP(c.addr, c.token, c.noAuth); err != nil {
		return err
	}

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: c.stateless, SessionTimeout: 10 * time.Minute})

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/mcp", originGuard(c.origins, bearerAuth(c.token, mcpHandler)))

	hs := &http.Server{
		Addr:              c.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	logger.Info("serving MCP over Streamable HTTP", "addr", c.addr, "path", "/mcp", "auth", c.token != "", "stateless", c.stateless)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return hs.Shutdown(sctx)
	}
}

// validateHTTP rejects an HTTP listener that would be exposed without a token.
func validateHTTP(addr, token string, noAuth bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid --listen %q: %w", addr, err)
	}
	if token == "" && !loopback(host) && !noAuth {
		return errors.New("refusing to listen on a non-loopback address without a bearer token: set DDG_MCP_AUTH_TOKEN or --auth-token-file (or --insecure-no-auth)")
	}
	return nil
}

// probe checks that the local HTTP server answers /healthz and that the Tor
// SOCKS port accepts connections. It sends nothing through Tor.
func probe(listen, torProxy string) error {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", listen, err)
	}
	hc := &http.Client{Timeout: 3 * time.Second}
	resp, err := hc.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		return fmt.Errorf("server not answering: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server health check returned HTTP %d", resp.StatusCode)
	}
	conn, err := net.DialTimeout("tcp", torProxy, 2*time.Second)
	if err != nil {
		return fmt.Errorf("Tor proxy not accepting connections: %w", err)
	}
	return conn.Close()
}

// bearerAuth requires "Authorization: Bearer <token>" when token is set.
func bearerAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ddg-onion-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originGuard rejects browser requests from origins that are not loopback or
// explicitly allowed. Requests without an Origin header (normal MCP clients)
// pass through.
func originGuard(allowed []string, next http.Handler) http.Handler {
	allow := map[string]bool{}
	for _, o := range allowed {
		allow[strings.ToLower(strings.TrimRight(o, "/"))] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" {
			u, err := url.Parse(o)
			if err != nil || !(loopback(u.Hostname()) || allow[strings.ToLower(strings.TrimRight(o, "/"))]) {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func loadToken(file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("reading --auth-token-file: %w", err)
		}
		t := strings.TrimSpace(string(b))
		if len(t) < 16 {
			return "", errors.New("the auth token must be at least 16 characters")
		}
		return t, nil
	}
	t := strings.TrimSpace(os.Getenv("DDG_MCP_AUTH_TOKEN"))
	if t != "" && len(t) < 16 {
		return "", errors.New("DDG_MCP_AUTH_TOKEN must be at least 16 characters")
	}
	return t, nil
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil && ip.IsLoopback()
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http/httptest"

	"github.com/spignelon/ddg-onion-mcp/internal/ddg"
	"github.com/spignelon/ddg-onion-mcp/internal/extract"
	"github.com/spignelon/ddg-onion-mcp/internal/fetch"
	"github.com/spignelon/ddg-onion-mcp/internal/mcpserver"
	"github.com/spignelon/ddg-onion-mcp/internal/torhttp"
)

type okSearch struct{}

func (okSearch) Search(context.Context, string, int, ddg.Options) ([]ddg.Result, error) {
	return []ddg.Result{{Position: 1, Title: "Hit", URL: "https://hit.example/", Snippet: "snippet"}}, nil
}

type okFetch struct{}

func (okFetch) Get(context.Context, string, extract.Options) (*fetch.Page, error) {
	return &fetch.Page{URL: "https://hit.example/", Text: "body"}, nil
}

func testServer() *mcp.Server {
	return mcpserver.New(mcpserver.Config{Search: okSearch{}, Fetch: okFetch{}})
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestBearerAuth(t *testing.T) {
	h := bearerAuth("s3cret-token-0123456", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for hdr, want := range map[string]int{
		"": 401, "Bearer": 401, "Bearer wrong": 401, "Basic s3cret-token-0123456": 401,
		"bearer s3cret-token-0123456": 401, "Bearer s3cret-token-0123456": 204,
	} {
		req := httptest.NewRequest("POST", "/mcp", nil)
		if hdr != "" {
			req.Header.Set("Authorization", hdr)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Authorization %q -> %d, want %d", hdr, rec.Code, want)
		}
	}
	// Without a token configured the handler is not wrapped.
	open := bearerAuth("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	rec := httptest.NewRecorder()
	open.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 204 {
		t.Errorf("no-token mode -> %d", rec.Code)
	}
}

func TestOriginGuard(t *testing.T) {
	h := originGuard([]string{"https://chat.example.com/"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for origin, want := range map[string]int{
		"": 204, "http://localhost:3000": 204, "http://127.0.0.1:8080": 204, "http://[::1]:9": 204,
		"https://chat.example.com": 204, "https://CHAT.example.com": 204,
		"https://evil.example": 403, "http://localhost.evil.example": 403, "null": 403, "::::": 403,
	} {
		req := httptest.NewRequest("POST", "/mcp", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Origin %q -> %d, want %d", origin, rec.Code, want)
		}
	}
}

func TestLoadToken(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if tok, err := loadToken(write("good", "  0123456789abcdef-token \n")); err != nil || tok != "0123456789abcdef-token" {
		t.Errorf("good file: %q, %v", tok, err)
	}
	if _, err := loadToken(write("short", "tiny\n")); err == nil {
		t.Error("short token accepted")
	}
	if _, err := loadToken(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file accepted")
	}
	t.Setenv("DDG_MCP_AUTH_TOKEN", "")
	if tok, err := loadToken(""); err != nil || tok != "" {
		t.Errorf("no token configured: %q, %v", tok, err)
	}
	t.Setenv("DDG_MCP_AUTH_TOKEN", "short")
	if _, err := loadToken(""); err == nil {
		t.Error("short env token accepted")
	}
	t.Setenv("DDG_MCP_AUTH_TOKEN", "env-token-0123456789")
	if tok, err := loadToken(""); err != nil || tok != "env-token-0123456789" {
		t.Errorf("env token: %q, %v", tok, err)
	}
}

func TestRefusesNonLoopbackListenerWithoutToken(t *testing.T) {
	err := serveHTTP(context.Background(), slog.New(slog.DiscardHandler), testServer(), httpConfig{addr: "0.0.0.0:0"})
	if err == nil || !strings.Contains(err.Error(), "bearer token") {
		t.Fatalf("error = %v, want a refusal mentioning the bearer token", err)
	}
}

type authTransport struct{ token string }

func (a authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if a.token != "" {
		r.Header.Set("Authorization", "Bearer "+a.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestStreamableHTTPEndToEnd(t *testing.T) {
	const token = "end-to-end-token-123456"
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveHTTP(ctx, slog.New(slog.DiscardHandler), testServer(), httpConfig{addr: addr, token: token, stateless: true})
	}()
	base := "http://" + addr
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || !strings.Contains(string(b), "ok") {
				t.Fatalf("healthz: %d %q", resp.StatusCode, b)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// No token: rejected before reaching the MCP handler. Wrong Origin: forbidden.
	for name, hdr := range map[string]map[string]string{
		"no token":    {},
		"wrong token": {"Authorization": "Bearer nope-nope-nope-nope"},
		"bad origin":  {"Authorization": "Bearer " + token, "Origin": "https://evil.example"},
	} {
		req, _ := http.NewRequest("POST", base+"/mcp", strings.NewReader(`{}`))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 && resp.StatusCode != 403 {
			t.Errorf("%s: status %d, want 401/403", name, resp.StatusCode)
		}
	}

	// A real MCP client over Streamable HTTP with the token works.
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil)
	cctx, ccancel := context.WithTimeout(ctx, 10*time.Second)
	defer ccancel()
	session, err := client.Connect(cctx, &mcp.StreamableClientTransport{
		Endpoint:   base + "/mcp",
		HTTPClient: &http.Client{Transport: authTransport{token: token}},
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()
	tools, err := session.ListTools(cctx, nil)
	if err != nil || len(tools.Tools) != 2 {
		t.Fatalf("tools: %v, %v", tools, err)
	}
	res, err := session.CallTool(cctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{"query": "anything"}})
	if err != nil || res.IsError {
		t.Fatalf("search: %v %+v", err, res)
	}
	if txt := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(txt, "https://hit.example/") {
		t.Fatalf("search output: %q", txt)
	}

	// Without the token the client cannot even initialise.
	bad := mcp.NewClient(&mcp.Implementation{Name: "bad", Version: "0"}, nil)
	if s, err := bad.Connect(cctx, &mcp.StreamableClientTransport{Endpoint: base + "/mcp", HTTPClient: &http.Client{Transport: authTransport{}}}, nil); err == nil {
		s.Close()
		t.Fatal("client connected without a token")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestRunFlagValidation(t *testing.T) {
	if err := run([]string{"--version"}); err != nil {
		t.Errorf("--version: %v", err)
	}
	for _, args := range [][]string{
		{"--transport", "websocket", "--skip-tor-check"},
		{"--isolation", "sometimes", "--skip-tor-check"},
		{"--tor-proxy", "203.0.113.5:9050", "--skip-tor-check"},
		{"--tor-proxy", "nonsense", "--skip-tor-check"},
		{"--no-such-flag"},
	} {
		if err := run(args); err == nil {
			t.Errorf("run(%v) succeeded", args)
		}
	}
}

func TestStartupFailsClosedWhenTorIsDown(t *testing.T) {
	dead := freeAddr(t) // nothing listens here
	err := run([]string{"--tor-proxy", dead, "--transport", "http", "--listen", freeAddr(t)})
	if err == nil || !strings.Contains(err.Error(), "Tor check failed") {
		t.Fatalf("error = %v, want a startup Tor check failure", err)
	}
}

func TestBuildProfile(t *testing.T) {
	p, err := buildProfile("tor-browser", "", "")
	if err != nil || p.Name != "tor-browser" || !p.Browser {
		t.Fatalf("tor-browser: %+v, %v", p, err)
	}
	p, err = buildProfile("honest", "", "")
	if err != nil || p.Name != "honest" || p.Browser || !strings.HasPrefix(p.Headers[0].Value, "ddg-onion-mcp/") {
		t.Fatalf("honest: %+v, %v", p, err)
	}
	p, err = buildProfile("tor-browser", "", "custom-agent/1")
	if err != nil || p.Headers[0].Value != "custom-agent/1" {
		t.Fatalf("user-agent override: %+v, %v", p, err)
	}
	file := filepath.Join(t.TempDir(), "id.json")
	if err := os.WriteFile(file, []byte(`{"User-Agent":"from-file/1","Accept-Language":"de-DE,de;q=0.5"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// --user-agent wins over the file, the file wins over the built-in profile.
	p, err = buildProfile("tor-browser", file, "flag/2")
	if err != nil || p.Headers[0].Value != "flag/2" {
		t.Fatalf("precedence: %+v, %v", p, err)
	}
	found := false
	for _, h := range p.Headers {
		if h.Name == "Accept-Language" && h.Value == "de-DE,de;q=0.5" {
			found = true
		}
	}
	if !found {
		t.Errorf("identity file override missing: %+v", p.Headers)
	}
	if _, err := buildProfile("firefox", "", ""); err == nil {
		t.Error("unknown identity accepted")
	}
	if _, err := buildProfile("tor-browser", filepath.Join(t.TempDir(), "nope.json"), ""); err == nil {
		t.Error("missing identity file accepted")
	}
}

func TestCheckConfig(t *testing.T) {
	t.Setenv("DDG_MCP_AUTH_TOKEN", "")
	t.Setenv("DDG_MCP_AUTH_TOKEN_FILE", "")
	// Exposed without a token: rejected before anything starts.
	if err := run([]string{"--check-config", "--transport", "http", "--listen", "0.0.0.0:8085"}); err == nil || !strings.Contains(err.Error(), "bearer token") {
		t.Fatalf("error = %v, want a bearer-token refusal", err)
	}
	// Loopback needs no token; stdio never does.
	for _, args := range [][]string{
		{"--check-config", "--transport", "http", "--listen", "127.0.0.1:8085"},
		{"--check-config", "--transport", "stdio"},
		{"--check-config", "--transport", "http", "--listen", "0.0.0.0:8085", "--insecure-no-auth"},
	} {
		if err := run(args); err != nil {
			t.Errorf("run(%v): %v", args, err)
		}
	}
	t.Setenv("DDG_MCP_AUTH_TOKEN", "a-long-enough-token-1234")
	if err := run([]string{"--check-config", "--transport", "http", "--listen", "0.0.0.0:8085"}); err != nil {
		t.Errorf("with token: %v", err)
	}
	// --check-config must not touch the network: Tor check is skipped.
	if err := run([]string{"--check-config", "--tor-proxy", freeAddr(t)}); err != nil {
		t.Errorf("check-config contacted Tor: %v", err)
	}
	// Bad settings are still caught.
	if err := run([]string{"--check-config", "--identity", "chrome"}); err == nil {
		t.Error("bad identity accepted")
	}
}

func TestHealthcheck(t *testing.T) {
	tor, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tor.Close()
	go func() {
		for {
			c, err := tor.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	addr := freeAddr(t)
	if err := run([]string{"--healthcheck", "--listen", addr, "--tor-proxy", tor.Addr().String()}); err == nil {
		t.Fatal("healthcheck passed with no server running")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveHTTP(ctx, slog.New(slog.DiscardHandler), testServer(), httpConfig{addr: addr, stateless: true})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := run([]string{"--healthcheck", "--listen", addr, "--tor-proxy", tor.Addr().String()}); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("healthcheck never passed: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Server up but Tor proxy down: unhealthy.
	if err := run([]string{"--healthcheck", "--listen", addr, "--tor-proxy", freeAddr(t)}); err == nil {
		t.Error("healthcheck passed with Tor proxy down")
	}
	// A wildcard listen address is probed on loopback.
	_, port, _ := net.SplitHostPort(addr)
	if err := run([]string{"--healthcheck", "--listen", "0.0.0.0:" + port, "--tor-proxy", tor.Addr().String()}); err != nil {
		t.Errorf("wildcard listen address: %v", err)
	}
	cancel()
	<-done
}

type scriptedChecker struct {
	results []error
	calls   int
}

func (s *scriptedChecker) Check(context.Context) error {
	i := s.calls
	s.calls++
	if i < len(s.results) {
		return s.results[i]
	}
	return s.results[len(s.results)-1]
}

func TestWaitForTor(t *testing.T) {
	quiet := slog.New(slog.DiscardHandler)
	glitch := errors.New(`Get "https://check.torproject.org/api/ip": EOF`)

	// Transport glitches are retried until the check passes.
	c := &scriptedChecker{results: []error{glitch, glitch, nil}}
	if err := waitForTor(context.Background(), c, quiet, 5, time.Millisecond); err != nil || c.calls != 3 {
		t.Fatalf("err=%v calls=%d, want success on the 3rd attempt", err, c.calls)
	}

	// It gives up after the configured number of attempts and reports the last error.
	c = &scriptedChecker{results: []error{glitch}}
	if err := waitForTor(context.Background(), c, quiet, 4, time.Millisecond); !errors.Is(err, glitch) || c.calls != 4 {
		t.Fatalf("err=%v calls=%d, want the glitch after 4 attempts", err, c.calls)
	}

	// A definitive "not Tor" answer, or a dead proxy, is never retried.
	for name, e := range map[string]error{
		"not tor":    fmt.Errorf("check: %w", torhttp.ErrNotTor),
		"proxy down": fmt.Errorf("%w at 127.0.0.1:9050", torhttp.ErrProxyUnavailable),
	} {
		c = &scriptedChecker{results: []error{e}}
		if err := waitForTor(context.Background(), c, quiet, 5, time.Millisecond); err == nil || c.calls != 1 {
			t.Errorf("%s: err=%v calls=%d, want an immediate failure", name, err, c.calls)
		}
	}

	// Cancellation stops the retries.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c = &scriptedChecker{results: []error{glitch}}
	if err := waitForTor(ctx, c, quiet, 5, time.Hour); err == nil || c.calls > 1 {
		t.Errorf("cancelled: err=%v calls=%d", err, c.calls)
	}
}

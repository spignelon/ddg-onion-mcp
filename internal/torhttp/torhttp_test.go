// SPDX-License-Identifier: GPL-3.0-or-later

package torhttp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSOCKS is a minimal SOCKS5 server (RFC 1928/1929). After CONNECT it
// answers one HTTP request itself, recording what the client asked for.
type fakeSOCKS struct {
	ln net.Listener

	mu      sync.Mutex
	atyps   []byte
	hosts   []string
	users   []string
	headers []http.Header
	bodies  []string

	// failure injection
	connects     int
	failuresLeft int           // refuse this many CONNECTs
	failCode     byte          // SOCKS reply code for refusals (6 = TTL expired)
	hang         bool          // never answer a CONNECT
	respondDelay time.Duration // wait before answering the HTTP request
}

func newFakeSOCKS(t *testing.T) *fakeSOCKS {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSOCKS{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.handle(c)
		}
	}()
	return f
}

func (f *fakeSOCKS) addr() string { return f.ln.Addr().String() }

func (f *fakeSOCKS) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil || hdr[0] != 5 {
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	user := ""
	if strings.Contains(string(methods), "\x02") {
		c.Write([]byte{5, 2})
		ver, _ := br.ReadByte()
		if ver != 1 {
			return
		}
		ulen, _ := br.ReadByte()
		ub := make([]byte, ulen)
		io.ReadFull(br, ub)
		plen, _ := br.ReadByte()
		pb := make([]byte, plen)
		io.ReadFull(br, pb)
		user = string(ub)
		c.Write([]byte{1, 0})
	} else {
		c.Write([]byte{5, 0})
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil {
		return
	}
	var host string
	switch req[3] {
	case 1:
		b := make([]byte, 4)
		io.ReadFull(br, b)
		host = net.IP(b).String()
	case 3:
		n, _ := br.ReadByte()
		b := make([]byte, n)
		io.ReadFull(br, b)
		host = string(b)
	case 4:
		b := make([]byte, 16)
		io.ReadFull(br, b)
		host = net.IP(b).String()
	}
	port := make([]byte, 2)
	io.ReadFull(br, port)
	f.mu.Lock()
	f.atyps = append(f.atyps, req[3])
	f.hosts = append(f.hosts, host)
	f.users = append(f.users, user)
	f.mu.Unlock()
	f.mu.Lock()
	f.connects++
	fail, hang, delay := f.failuresLeft > 0, f.hang, f.respondDelay
	if fail {
		f.failuresLeft--
	}
	code := f.failCode
	f.mu.Unlock()
	if hang {
		io.Copy(io.Discard, c) // hold the connection open without replying
		return
	}
	if fail {
		c.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})

	hr, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	reqBody, _ := io.ReadAll(hr.Body)
	f.mu.Lock()
	f.headers = append(f.headers, hr.Header.Clone())
	f.bodies = append(f.bodies, string(reqBody))
	f.mu.Unlock()
	time.Sleep(delay)
	body := "tor says hi"
	io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 11\r\nConnection: close\r\n\r\n"+body)
}

func get(t *testing.T, c *Client, url string) (*http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	return c.Do(req)
}

func TestHostNamesGoToTheProxyUnresolved(t *testing.T) {
	f := newFakeSOCKS(t)
	c, err := New(Config{ProxyAddr: f.addr(), UserAgent: "test-agent/1"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := get(t, c, "http://hidden-service.example/path")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "tor says hi" {
		t.Fatalf("body = %q", body)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hosts) != 1 || f.atyps[0] != 3 || f.hosts[0] != "hidden-service.example" {
		t.Fatalf("proxy saw atyp=%v hosts=%v; want one domain-name request (ATYP 3) for the unresolved name", f.atyps, f.hosts)
	}
	if got := f.headers[0].Get("User-Agent"); got != "test-agent/1" {
		t.Fatalf("User-Agent = %q", got)
	}
}

func TestSessionIsolationSharesCredentialsRequestIsolationDoesNot(t *testing.T) {
	for _, tc := range []struct {
		name     string
		iso      Isolation
		wantSame bool
	}{{"session", IsolateSession, true}, {"request", IsolateRequest, false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSOCKS(t)
			c, err := New(Config{ProxyAddr: f.addr(), Isolation: tc.iso})
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				resp, err := get(t, c, "http://one.example/")
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.users) != 2 {
				t.Fatalf("expected 2 connections, got %d", len(f.users))
			}
			if !strings.HasPrefix(f.users[0], "ddg-onion-mcp-") {
				t.Fatalf("SOCKS user = %q", f.users[0])
			}
			if (f.users[0] == f.users[1]) != tc.wantSame {
				t.Fatalf("users %q / %q: same=%v, want %v", f.users[0], f.users[1], f.users[0] == f.users[1], tc.wantSame)
			}
		})
	}
}

func TestFailsClosedWhenProxyIsDown(t *testing.T) {
	// A destination that is reachable directly: if the client ever bypassed
	// the proxy it would hit this server.
	var hits int
	var mu sync.Mutex
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	defer direct.Close()

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close() // nothing listens here any more

	c, err := New(Config{ProxyAddr: dead})
	if err != nil {
		t.Fatal(err)
	}
	_, err = get(t, c, direct.URL)
	if err == nil {
		t.Fatal("request succeeded without a proxy")
	}
	if !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("error = %v, want ErrProxyUnavailable", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("destination was contacted directly %d time(s)", hits)
	}
}

func TestIgnoresProxyEnvironmentVariables(t *testing.T) {
	var accepted int
	var mu sync.Mutex
	other, _ := net.Listen("tcp", "127.0.0.1:0")
	defer other.Close()
	go func() {
		for {
			c, err := other.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted++
			mu.Unlock()
			c.Close()
		}
	}()
	t.Setenv("HTTP_PROXY", "http://"+other.Addr().String())
	t.Setenv("http_proxy", "http://"+other.Addr().String())
	t.Setenv("ALL_PROXY", "socks5://"+other.Addr().String())

	f := newFakeSOCKS(t)
	c, _ := New(Config{ProxyAddr: f.addr()})
	resp, err := get(t, c, "http://env.example/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if accepted != 0 {
		t.Fatalf("environment proxy was contacted %d time(s)", accepted)
	}
}

func TestRemoteProxyNeedsExplicitOptIn(t *testing.T) {
	if _, err := New(Config{ProxyAddr: "192.0.2.10:9050"}); err == nil {
		t.Fatal("non-loopback proxy accepted by default")
	}
	if _, err := New(Config{ProxyAddr: "192.0.2.10:9050", AllowRemoteProxy: true}); err != nil {
		t.Fatalf("explicit opt-in rejected: %v", err)
	}
	for _, bad := range []string{"", "9050", "127.0.0.1", ":9050"} {
		if _, err := New(Config{ProxyAddr: bad}); err == nil {
			t.Fatalf("invalid proxy %q accepted", bad)
		}
	}
	for _, ok := range []string{"127.0.0.1:9050", "localhost:9050", "[::1]:9050"} {
		if _, err := New(Config{ProxyAddr: ok}); err != nil {
			t.Fatalf("loopback proxy %q rejected: %v", ok, err)
		}
	}
}

func TestRejectsNonHTTPSchemes(t *testing.T) {
	c := NewWithDialer(func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("dial attempted")
		return nil, nil
	}, "ua")
	for _, u := range []string{"ftp://example.com/", "file:///etc/passwd", "gopher://example.com/"} {
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		if _, err := c.Do(req); err == nil {
			t.Fatalf("%s accepted", u)
		}
	}
}

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantErr    string
	}{
		{"tor", `{"IsTor":true,"IP":"203.0.113.9"}`, 200, ""},
		{"not tor", `{"IsTor":false,"IP":"198.51.100.2"}`, 200, "NOT leaving through Tor"},
		{"server error", `oops`, 500, "unexpected HTTP status"},
		{"garbage", `<html>`, 200, "bad response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			old := checkURL
			checkURL = "http://check.example/api/ip"
			defer func() { checkURL = old }()
			c := NewWithDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
			}, "ua")
			err := c.Check(context.Background())
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

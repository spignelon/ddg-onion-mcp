// SPDX-License-Identifier: GPL-3.0-or-later

package torhttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRetriesWhenTorCannotConnect(t *testing.T) {
	f := newFakeSOCKS(t)
	f.failuresLeft, f.failCode = 2, 6 // SOCKS "TTL expired", what a cold onion service gives
	c, err := New(Config{ProxyAddr: f.addr(), ConnectAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := get(t, c, "http://onion-like.example/")
	if err != nil {
		t.Fatalf("request failed despite retries: %v", err)
	}
	resp.Body.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.connects != 3 {
		t.Fatalf("CONNECT attempts = %d, want 3 (two failures, then success)", f.connects)
	}
}

func TestGivesUpAfterTheConfiguredAttempts(t *testing.T) {
	f := newFakeSOCKS(t)
	f.failuresLeft, f.failCode = 100, 6
	c, _ := New(Config{ProxyAddr: f.addr(), ConnectAttempts: 2})
	_, err := get(t, c, "http://onion-like.example/")
	var ce *ConnectError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "Tor could not connect to onion-like.example:80") {
		t.Fatalf("error = %v, want a *ConnectError naming the destination", err)
	}
	if errors.Is(err, ErrProxyUnavailable) {
		t.Fatal("a refused CONNECT is not an unreachable proxy")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.connects != 2 {
		t.Fatalf("CONNECT attempts = %d, want exactly 2", f.connects)
	}
}

func TestPostBodyIsResentIntactOnRetry(t *testing.T) {
	f := newFakeSOCKS(t)
	f.failuresLeft, f.failCode = 1, 4 // host unreachable
	c, _ := New(Config{ProxyAddr: f.addr(), ConnectAttempts: 3})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://form.example/lite/", strings.NewReader("q=golang+tor&kl=wt-wt"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) != 1 || f.bodies[0] != "q=golang+tor&kl=wt-wt" {
		t.Fatalf("bodies received = %q, want the full form once", f.bodies)
	}
}

func TestTestClientsDoNotRetry(t *testing.T) {
	// NewWithDialer clients (used by other packages' tests) must stay
	// single-attempt so those tests remain deterministic.
	c := NewWithDialer(func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("unused") }, "ua")
	if c.attempts != 1 {
		t.Fatalf("attempts = %d, want 1", c.attempts)
	}
}

func TestRetryableClassification(t *testing.T) {
	if !retryable(&ConnectError{Addr: "x:80", Err: errors.New("TTL expired")}) {
		t.Error("connect failures should be retryable")
	}
	if !retryable(fmt.Errorf("wrapped: %w", &ConnectError{Addr: "x:80", Err: errors.New("boom")})) {
		t.Error("wrapped connect failures should be retryable")
	}
	if retryable(ErrProxyUnavailable) || retryable(errors.New("HTTP 500")) || retryable(context.Canceled) {
		t.Error("only Tor connect failures are retryable")
	}
}

func TestConnectTimeoutBoundsAHangingConnect(t *testing.T) {
	f := newFakeSOCKS(t)
	f.hang = true
	c, _ := New(Config{ProxyAddr: f.addr(), ConnectTimeout: 250 * time.Millisecond, ConnectAttempts: 1})
	start := time.Now()
	_, err := get(t, c, "http://hang.example/")
	var ce *ConnectError
	if !errors.As(err, &ce) {
		t.Fatalf("error = %v, want a *ConnectError", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v; the connect timeout did not apply", d)
	}
}

func TestConnectTimeoutDoesNotKillEstablishedConnections(t *testing.T) {
	f := newFakeSOCKS(t)
	f.respondDelay = 800 * time.Millisecond // longer than the connect timeout
	c, _ := New(Config{ProxyAddr: f.addr(), ConnectTimeout: 300 * time.Millisecond, ConnectAttempts: 1})
	resp, err := get(t, c, "http://slow.example/")
	if err != nil {
		t.Fatalf("an established connection was cut by the connect timeout: %v", err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(b) != "tor says hi" {
		t.Fatalf("body = %q, err = %v", b, err)
	}
}

func TestCancellationStopsRetries(t *testing.T) {
	f := newFakeSOCKS(t)
	f.failuresLeft, f.failCode = 1000, 6
	c, _ := New(Config{ProxyAddr: f.addr(), ConnectAttempts: 50})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://gone.example/", nil)
	if _, err := c.Do(req); err == nil {
		t.Fatal("expected an error")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.connects > 1 {
		t.Fatalf("made %d connection attempts after cancellation", f.connects)
	}
}

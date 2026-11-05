// SPDX-License-Identifier: GPL-3.0-or-later

// Package torhttp provides an HTTP client that can only reach the network
// through a Tor SOCKS5 proxy.
//
// Guarantees:
//   - Every connection is opened through the SOCKS5 proxy. The standard
//     HTTP_PROXY/ALL_PROXY environment variables are ignored.
//   - Host names are sent to the proxy unresolved (the SOCKS5 "domain name"
//     address type), so DNS is resolved by Tor and never by this machine.
//   - If the proxy cannot be reached the request fails; there is no fallback
//     to a direct connection.
package torhttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// Isolation controls how Tor circuits are shared between requests.
type Isolation int

const (
	// IsolateSession uses one random SOCKS credential for the life of the
	// process, so all requests share circuits that no other Tor client on the
	// machine shares.
	IsolateSession Isolation = iota
	// IsolateRequest uses a fresh SOCKS credential (and so a fresh circuit)
	// for every connection and disables connection reuse. Slower.
	IsolateRequest
)

// DialFunc opens a connection to addr ("host:port").
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Config configures New.
type Config struct {
	// ProxyAddr is the Tor SOCKS5 listener, "host:port".
	ProxyAddr string
	// AllowRemoteProxy permits a non-loopback ProxyAddr. A proxy on another
	// host may not be Tor, so this must be an explicit choice.
	AllowRemoteProxy bool
	Isolation        Isolation
	// Profile is the identity headers to send. If it has no headers, an
	// honest profile using UserAgent is used.
	Profile       Profile
	UserAgent     string
	DialTimeout   time.Duration // time to reach the SOCKS proxy (default 10s)
	HeaderTimeout time.Duration // time to first response byte (default 60s)
}

// Client is an HTTP client that only connects through Tor.
type Client struct {
	hc      *http.Client
	profile Profile
}

// ErrProxyUnavailable is returned when the SOCKS proxy cannot be reached.
var ErrProxyUnavailable = errors.New("cannot reach the Tor SOCKS proxy (refusing to connect directly)")

// New returns a Client that tunnels every connection through cfg.ProxyAddr.
func New(cfg Config) (*Client, error) {
	host, port, err := net.SplitHostPort(cfg.ProxyAddr)
	if err != nil || port == "" || host == "" {
		return nil, fmt.Errorf("torhttp: invalid proxy address %q (want host:port)", cfg.ProxyAddr)
	}
	if !cfg.AllowRemoteProxy && !isLoopbackHost(host) {
		return nil, fmt.Errorf("torhttp: proxy %q is not a loopback address; a remote SOCKS proxy may not be Tor (set the allow-remote option to override)", cfg.ProxyAddr)
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.HeaderTimeout <= 0 {
		cfg.HeaderTimeout = 60 * time.Second
	}
	sd := &socksDialer{addr: cfg.ProxyAddr, timeout: cfg.DialTimeout, perRequest: cfg.Isolation == IsolateRequest}
	if !sd.perRequest {
		sd.session = randomToken()
	}
	profile := cfg.Profile
	if len(profile.Headers) == 0 {
		profile = HonestProfile(cfg.UserAgent)
	}
	return newClient(sd.dial, profile, cfg.HeaderTimeout, cfg.Isolation == IsolateRequest), nil
}

// NewWithDialer builds a Client around an arbitrary dial function. It exists
// so tests can stand in for Tor; production code uses New.
func NewWithDialer(dial DialFunc, userAgent string) *Client {
	return newClient(dial, HonestProfile(userAgent), 30*time.Second, false)
}

// NewWithProfile is NewWithDialer with an explicit identity profile (tests).
func NewWithProfile(dial DialFunc, p Profile) *Client {
	return newClient(dial, p, 30*time.Second, false)
}

func newClient(dial DialFunc, profile Profile, headerTimeout time.Duration, disableKeepAlives bool) *Client {
	tr := &http.Transport{
		Proxy:                 nil, // never honour HTTP_PROXY & co.
		DialContext:           dial,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   60 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
		DisableKeepAlives:     disableKeepAlives,
	}
	return &Client{
		profile: profile,
		hc: &http.Client{
			Transport: tr,
			// Callers validate each redirect hop themselves.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Do sends req through Tor. Redirects are not followed.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if req.URL == nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.Host == "" {
		return nil, fmt.Errorf("torhttp: unsupported URL %q", req.URL)
	}
	if req.Header == nil {
		req.Header = http.Header{}
	}
	c.profile.apply(req.Header)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	// Decode here only if the profile set Accept-Encoding: otherwise Go has
	// already decompressed the body itself.
	if !resp.Uncompressed {
		if err := decodeResponse(resp); err != nil {
			resp.Body.Close()
			return nil, err
		}
	}
	return resp, nil
}

// CloseIdleConnections closes pooled connections.
func (c *Client) CloseIdleConnections() { c.hc.CloseIdleConnections() }

// checkURL is the Tor Project's "am I using Tor" API (a variable for tests).
var checkURL = "https://check.torproject.org/api/ip"

// Check asks the Tor Project's check service, through this client, whether the
// connection really exits through Tor.
func (c *Client) Check(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tor check: unexpected HTTP status %d", resp.StatusCode)
	}
	var out struct {
		IsTor bool `json:"IsTor"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&out); err != nil {
		return fmt.Errorf("tor check: bad response: %w", err)
	}
	if !out.IsTor {
		return errors.New("tor check: the proxy is reachable but traffic is NOT leaving through Tor")
	}
	return nil
}

type socksDialer struct {
	addr       string
	timeout    time.Duration
	perRequest bool
	session    string
}

func (s *socksDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("torhttp: unsupported network %q", network)
	}
	user := s.session
	if s.perRequest {
		user = randomToken()
	}
	// The forward dialer is only ever used to reach the SOCKS proxy itself.
	d, err := proxy.SOCKS5("tcp", s.addr, &proxy.Auth{User: "ddg-onion-mcp-" + user, Password: "x"}, &net.Dialer{Timeout: s.timeout})
	if err != nil {
		return nil, err
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("torhttp: SOCKS5 dialer does not support contexts")
	}
	// addr still holds the host name: it is passed to Tor unresolved.
	conn, err := cd.DialContext(ctx, "tcp", addr)
	if err != nil {
		if failedToReach(err, s.addr) {
			return nil, fmt.Errorf("%w at %s: %v", ErrProxyUnavailable, s.addr, err)
		}
		return nil, err
	}
	return conn, nil
}

// failedToReach reports whether err is a failure to connect to the proxy
// itself (as opposed to a SOCKS-level failure reaching the destination). The
// SOCKS library wraps the underlying dial error, so walk the whole chain.
func failedToReach(err error, proxyAddr string) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if oe, ok := e.(*net.OpError); ok && oe.Op == "dial" && oe.Addr != nil && oe.Addr.String() == proxyAddr {
			return true
		}
	}
	return false
}

func randomToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("torhttp: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil && ip.IsLoopback()
}

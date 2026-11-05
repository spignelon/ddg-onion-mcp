// SPDX-License-Identifier: GPL-3.0-or-later

package torhttp

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// capture is the header set pasted from a real Tor Browser at myip.wtf/headers
// (minus the Sec-Fetch-* set, which depends on the kind of navigation).
var capture = map[string]string{
	"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
	"Accept-Encoding":           "gzip, deflate, br, zstd",
	"Accept-Language":           "en-US,en;q=0.9",
	"Priority":                  "u=0, i",
	"Sec-Gpc":                   "1",
	"Te":                        "trailers",
	"Upgrade-Insecure-Requests": "1",
	"User-Agent":                "Mozilla/5.0 (X11; Linux x86_64; rv:153.0) Gecko/20100101 Firefox/153.0",
}

func TestTorBrowserProfileSendsTheCapturedHeaders(t *testing.T) {
	f := newFakeSOCKS(t)
	c, err := New(Config{ProxyAddr: f.addr(), Profile: TorBrowserProfile()})
	if err != nil {
		t.Fatal(err)
	}
	if c.ProfileName() != "tor-browser" {
		t.Fatalf("profile = %q", c.ProfileName())
	}
	ctx := context.Background()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://site.example/", nil)
	// A caller-supplied Accept must not leak through: the profile wins.
	req.Header.Set("Accept", "text/plain")
	c.Navigate(req, Nav{Site: "none"})
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	f.mu.Lock()
	defer f.mu.Unlock()
	got := f.headers[0]
	for name, want := range capture {
		if got.Get(name) != want {
			t.Errorf("%s = %q, want %q", name, got.Get(name), want)
		}
	}
	for name, want := range map[string]string{
		"Sec-Fetch-Dest": "document", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "none", "Sec-Fetch-User": "?1",
	} {
		if got.Get(name) != want {
			t.Errorf("%s = %q, want %q", name, got.Get(name), want)
		}
	}
	if strings.Contains(got.Get("User-Agent"), "Go-http-client") || got.Get("Origin") != "" || got.Get("Referer") != "" {
		t.Errorf("unexpected identity leak: %v", got)
	}
}

func TestFormSubmissionNavigationHeaders(t *testing.T) {
	f := newFakeSOCKS(t)
	c, _ := New(Config{ProxyAddr: f.addr(), Profile: TorBrowserProfile()})
	req, _ := http.NewRequest(http.MethodGet, "http://site.example/lite/", nil)
	c.Navigate(req, Nav{Site: "same-origin", Origin: "http://site.example", Referer: "http://site.example/lite/"})
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.headers[0]
	if h.Get("Sec-Fetch-Site") != "same-origin" || h.Get("Origin") != "http://site.example" || h.Get("Referer") != "http://site.example/lite/" {
		t.Errorf("headers = %v", h)
	}
}

func TestHonestProfileSendsNoBrowserHeaders(t *testing.T) {
	f := newFakeSOCKS(t)
	c, _ := New(Config{ProxyAddr: f.addr(), UserAgent: "my-tool/2"})
	req, _ := http.NewRequest(http.MethodGet, "http://site.example/", nil)
	c.Navigate(req, Nav{Site: "none", Origin: "http://x", Referer: "http://y"}) // no-op for this profile
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.headers[0]
	if h.Get("User-Agent") != "my-tool/2" {
		t.Errorf("User-Agent = %q", h.Get("User-Agent"))
	}
	for _, name := range []string{"Sec-Fetch-Dest", "Sec-Fetch-Mode", "Sec-Fetch-Site", "Sec-Fetch-User", "Sec-Gpc", "Te", "Priority", "Origin", "Referer", "Accept-Language"} {
		if h.Get(name) != "" {
			t.Errorf("honest profile sent %s = %q", name, h.Get(name))
		}
	}
	if c.ProfileName() != "honest" {
		t.Errorf("profile = %q", c.ProfileName())
	}
}

func TestProfileOverrides(t *testing.T) {
	p := TorBrowserProfile()
	q, err := p.With(map[string]string{
		"user-agent":      "Mozilla/5.0 (X11; Linux x86_64; rv:154.0) Gecko/20100101 Firefox/154.0",
		"Accept-Language": "", // removed
		"X-Custom":        "yes",
	})
	if err != nil {
		t.Fatal(err)
	}
	val := func(p Profile, name string) (string, bool) {
		for _, h := range p.Headers {
			if strings.EqualFold(h.Name, name) {
				return h.Value, true
			}
		}
		return "", false
	}
	if v, _ := val(q, "User-Agent"); !strings.Contains(v, "Firefox/154.0") {
		t.Errorf("User-Agent = %q", v)
	}
	if _, ok := val(q, "Accept-Language"); ok {
		t.Error("Accept-Language was not removed")
	}
	if v, _ := val(q, "X-Custom"); v != "yes" {
		t.Errorf("X-Custom = %q", v)
	}
	if v, _ := val(p, "User-Agent"); !strings.Contains(v, "Firefox/153.0") {
		t.Error("With modified the original profile")
	}

	for name, value := range map[string]string{
		"Host": "evil.example", "Cookie": "a=b", "Authorization": "Bearer x", "Content-Length": "1",
		"Connection": "keep-alive", "Origin": "http://x", "Referer": "http://x", "Transfer-Encoding": "chunked",
		"Bad Name": "x", "": "x", "X-Ok": "line1\r\nInjected: yes",
	} {
		if _, err := p.With(map[string]string{name: value}); err == nil {
			t.Errorf("header %q=%q accepted", name, value)
		}
	}
}

func TestOverrideFromFile(t *testing.T) {
	dir := t.TempDir()
	write := func(content string) string {
		path := filepath.Join(dir, "id.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	q, err := TorBrowserProfile().OverrideFromFile(write(`{"User-Agent":"Mozilla/5.0 (rv:200.0) Gecko/20100101 Firefox/200.0"}`))
	if err != nil {
		t.Fatal(err)
	}
	if q.Headers[0].Value != "Mozilla/5.0 (rv:200.0) Gecko/20100101 Firefox/200.0" {
		t.Errorf("User-Agent = %q", q.Headers[0].Value)
	}
	for _, bad := range []string{`not json`, `["a"]`, `{"User-Agent": 5}`, `{"Cookie":"a=b"}`} {
		if _, err := TorBrowserProfile().OverrideFromFile(write(bad)); err == nil {
			t.Errorf("bad identity file %q accepted", bad)
		}
	}
	if _, err := TorBrowserProfile().OverrideFromFile(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("missing file accepted")
	}
}

// encodeServer replies with body encoded as encoding, only if the request
// asked for it.
func encodeServer(t *testing.T, encoding string, body []byte) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), strings.Split(encoding, ",")[0]) {
			t.Errorf("client did not offer %s: %q", encoding, r.Header.Get("Accept-Encoding"))
		}
		out := body
		for _, enc := range strings.Split(encoding, ",") {
			out = encodeWith(t, strings.TrimSpace(enc), out)
		}
		w.Header().Set("Content-Encoding", encoding)
		w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return NewWithProfile(func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
	}, TorBrowserProfile())
}

func encodeWith(t *testing.T, enc string, in []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch enc {
	case "gzip":
		w := gzip.NewWriter(&buf)
		w.Write(in)
		w.Close()
	case "deflate": // zlib-wrapped, as the RFC says
		w := zlib.NewWriter(&buf)
		w.Write(in)
		w.Close()
	case "deflate-raw":
		w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
		w.Write(in)
		w.Close()
	case "br":
		w := brotli.NewWriter(&buf)
		w.Write(in)
		w.Close()
	case "zstd":
		w, _ := zstd.NewWriter(&buf)
		w.Write(in)
		w.Close()
	default:
		t.Fatalf("unknown encoding %s", enc)
	}
	return buf.Bytes()
}

func TestResponsesAreDecoded(t *testing.T) {
	payload := []byte(strings.Repeat("hello from a compressed page. ", 200))
	for _, enc := range []string{"gzip", "deflate", "br", "zstd", "gzip, br"} {
		t.Run(enc, func(t *testing.T) {
			c := encodeServer(t, enc, payload)
			req, _ := http.NewRequest(http.MethodGet, "http://site.example/", nil)
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("decoded %d bytes, want %d", len(got), len(payload))
			}
			if resp.Header.Get("Content-Encoding") != "" || resp.ContentLength != -1 || !resp.Uncompressed {
				t.Errorf("response metadata not normalised: %v %d", resp.Header, resp.ContentLength)
			}
		})
	}
}

func TestRawDeflateIsAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "deflate")
		w.Write(encodeWith(t, "deflate-raw", []byte("raw deflate body that is long enough to compress ........")))
	}))
	defer srv.Close()
	c := NewWithProfile(func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
	}, TorBrowserProfile())
	req, _ := http.NewRequest(http.MethodGet, "http://site.example/", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || !strings.HasPrefix(string(b), "raw deflate body") {
		t.Fatalf("body = %q, err = %v", b, err)
	}
}

func TestUnknownContentEncodingIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "compress")
		w.Write([]byte("x"))
	}))
	defer srv.Close()
	c := NewWithProfile(func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
	}, TorBrowserProfile())
	req, _ := http.NewRequest(http.MethodGet, "http://site.example/", nil)
	if _, err := c.Do(req); err == nil || !strings.Contains(err.Error(), "unsupported Content-Encoding") {
		t.Fatalf("error = %v", err)
	}
}

func TestDecompressionBombIsBoundedByTheCallersLimit(t *testing.T) {
	huge := bytes.Repeat([]byte("A"), 64<<20) // 64 MiB of zeros-like data compresses to a few KB
	c := encodeServer(t, "zstd", huge)
	req, _ := http.NewRequest(http.MethodGet, "http://site.example/", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || len(got) != 1<<20 {
		t.Fatalf("read %d bytes, err %v; a limit reader must stop a bomb at its limit", len(got), err)
	}
}

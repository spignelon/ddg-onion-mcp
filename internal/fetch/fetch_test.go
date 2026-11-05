// SPDX-License-Identifier: GPL-3.0-or-later

package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spignelon/ddg-onion-mcp/internal/extract"
	"github.com/spignelon/ddg-onion-mcp/internal/torhttp"
)

func TestValidateURL(t *testing.T) {
	ok := []string{
		"https://example.com/", "http://example.com:8080/a?b=c#frag", "https://sub.example.co.uk/x",
		"http://93.184.216.34/", "http://[2606:2800:220:1:248:1893:25c8:1946]/",
		"http://duckduckgogg42xjoc72x3sjasowoarfbgcmvfimaftt6twagswzczad.onion/", "https://bücher.example/",
	}
	for _, u := range ok {
		if _, err := ValidateURL(u, true); err != nil {
			t.Errorf("ValidateURL(%q) rejected: %v", u, err)
		}
	}

	bad := map[string]string{
		"ftp://example.com/":             "scheme",
		"file:///etc/passwd":             "scheme",
		"javascript:alert(1)":            "scheme",
		"https://user:pw@example.com/":   "credentials",
		"http://localhost/":              "loopback",
		"http://foo.localhost/":          "local-network",
		"http://printer.local/":          "local-network",
		"http://db.internal/":            "local-network",
		"http://router.home.arpa/":       "local-network",
		"http://intranet/":               "single-label",
		"http://127.0.0.1/":              "non-public",
		"http://127.1.2.3:8080/":         "non-public",
		"http://10.0.0.5/":               "non-public",
		"http://172.16.9.9/":             "non-public",
		"http://192.168.1.1/":            "non-public",
		"http://169.254.169.254/latest/": "non-public",
		"http://100.64.0.1/":             "non-public",
		"http://0.0.0.0/":                "non-public",
		"http://[::1]/":                  "non-public",
		"http://[fe80::1]/":              "non-public",
		"http://[fd00::1]/":              "non-public",
		"http://[::ffff:127.0.0.1]/":     "non-public",
		"http://2130706433/":             "numeric",
		"http://0x7f.0.0.1/":             "numeric",
		"http://017700000001/":           "numeric",
		"http://example.com:99999/":      "",
		"http:///nohost":                 "",
		"":                               "",
		"not a url":                      "",
	}
	for u, why := range bad {
		_, err := ValidateURL(u, true)
		if err == nil {
			t.Errorf("ValidateURL(%q) accepted; want rejection", u)
			continue
		}
		var be *BlockedError
		if !errors.As(err, &be) {
			t.Errorf("ValidateURL(%q) error is %T, want *BlockedError", u, err)
		}
		if why != "" && !strings.Contains(err.Error(), why) {
			t.Errorf("ValidateURL(%q) = %q, want mention of %q", u, err, why)
		}
	}

	if _, err := ValidateURL("http://abcdefghijklmnop.onion/", false); err == nil {
		t.Error(".onion accepted although disabled")
	}
}

// torStandIn serves handler for every destination and records what the client
// asked to connect to, standing in for Tor.
type torStandIn struct {
	srv   *httptest.Server
	mu    sync.Mutex
	addrs []string
}

func newStandIn(t *testing.T, h http.Handler) (*torStandIn, *Fetcher) {
	t.Helper()
	s := &torStandIn{srv: httptest.NewServer(h)}
	t.Cleanup(s.srv.Close)
	client := torhttp.NewWithDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
		s.mu.Lock()
		s.addrs = append(s.addrs, addr)
		s.mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, "tcp", s.srv.Listener.Addr().String())
	}, "test-agent")
	return s, &Fetcher{Client: client, AllowOnion: true}
}

func (s *torStandIn) dialed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.addrs...)
}

const articleHTML = `<html><head><title>Hello Page</title></head><body><main><article>
<h1>Hello Page</h1>
<p>This is a reasonably long paragraph of body text so that the readability extractor is confident it has found the real article on the page, rather than falling back to the whole document.</p>
<p>A second paragraph adds more substance: pages fetched through Tor are slow, so each request should extract everything useful in one pass and keep the output tidy for the model.</p>
</article></main></body></html>`

func TestGetFetchesAndExtracts(t *testing.T) {
	s, f := newStandIn(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "test-agent" {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		if r.Host != "docs.example.test" {
			t.Errorf("Host = %q", r.Host)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, articleHTML)
	}))
	page, err := f.Get(context.Background(), "http://docs.example.test/page#section", extract.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Title != "Hello Page" || !strings.Contains(page.Text, "reasonably long paragraph") {
		t.Fatalf("page = %+v", page)
	}
	if page.URL != "http://docs.example.test/page" {
		t.Fatalf("final URL = %q (fragment should be dropped)", page.URL)
	}
	// The destination was handed to the dialer as a name, never resolved here.
	if got := s.dialed(); len(got) != 1 || got[0] != "docs.example.test:80" {
		t.Fatalf("dialed = %v, want [docs.example.test:80]", got)
	}
}

func TestRedirectsAreFollowedAndEveryHopIsValidated(t *testing.T) {
	var step int
	_, f := newStandIn(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/middle", http.StatusFound)
		case "/middle":
			http.Redirect(w, r, "http://other.example.test/final", http.StatusMovedPermanently)
		case "/final":
			step++
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, articleHTML)
		case "/to-loopback":
			http.Redirect(w, r, "http://127.0.0.1:8080/admin", http.StatusFound)
		case "/to-metadata":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
		case "/to-file":
			http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		}
	}))

	page, err := f.Get(context.Background(), "http://a.example.test/start", extract.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if page.URL != "http://other.example.test/final" || step != 1 {
		t.Fatalf("final URL = %q, step = %d", page.URL, step)
	}

	for path, want := range map[string]string{
		"/to-loopback": "non-public", "/to-metadata": "non-public", "/to-file": "scheme", "/loop": "too many redirects",
	} {
		_, err := f.Get(context.Background(), "http://a.example.test"+path, extract.Options{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want mention of %q", path, err, want)
		}
	}
}

func TestContentTypeAndStatusHandling(t *testing.T) {
	_, f := newStandIn(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pdf":
			w.Header().Set("Content-Type", "application/pdf")
			fmt.Fprint(w, "%PDF-1.7 binary")
		case "/png":
			w.Header().Set("Content-Type", "image/png")
			fmt.Fprint(w, "\x89PNG")
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok": true}`)
		case "/plain":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprint(w, "  just text \n")
		case "/missing":
			http.NotFound(w, r)
		case "/boom":
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	for _, p := range []string{"/pdf", "/png"} {
		if _, err := f.Get(context.Background(), "http://c.example.test"+p, extract.Options{}); err == nil || !strings.Contains(err.Error(), "unsupported content type") {
			t.Errorf("%s: error = %v", p, err)
		}
	}
	for _, p := range []string{"/missing", "/boom"} {
		if _, err := f.Get(context.Background(), "http://c.example.test"+p, extract.Options{}); err == nil || !strings.Contains(err.Error(), "HTTP ") {
			t.Errorf("%s: error = %v", p, err)
		}
	}
	if p, err := f.Get(context.Background(), "http://c.example.test/json", extract.Options{}); err != nil || p.Text != `{"ok": true}` || p.Method != "raw" {
		t.Errorf("json: %+v, %v", p, err)
	}
	if p, err := f.Get(context.Background(), "http://c.example.test/plain", extract.Options{}); err != nil || p.Text != "just text" {
		t.Errorf("plain: %+v, %v", p, err)
	}
}

func TestDownloadSizeIsCapped(t *testing.T) {
	_, f := newStandIn(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Repeat("a", 5000))
	}))
	f.MaxBytes = 1000
	p, err := f.Get(context.Background(), "http://big.example.test/", extract.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !p.SourceTruncated || len(p.Text) != 1000 {
		t.Fatalf("truncated=%v len=%d, want truncated 1000", p.SourceTruncated, len(p.Text))
	}
}

func TestLegacyCharsetIsDecoded(t *testing.T) {
	_, f := newStandIn(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=iso-8859-1")
		w.Write([]byte("caf\xe9 cr\xe8me")) // "café crème" in Latin-1
	}))
	p, err := f.Get(context.Background(), "http://latin.example.test/", extract.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Text != "café crème" {
		t.Fatalf("text = %q", p.Text)
	}
}

func TestBlockedURLsNeverReachTheDialer(t *testing.T) {
	s, f := newStandIn(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, u := range []string{"http://127.0.0.1/", "http://localhost/", "http://192.168.0.1/", "ftp://example.com/"} {
		if _, err := f.Get(context.Background(), u, extract.Options{}); err == nil {
			t.Errorf("%s fetched", u)
		}
	}
	if got := s.dialed(); len(got) != 0 {
		t.Fatalf("dialer was called for blocked URLs: %v", got)
	}
}

func TestContextCancelStopsTheFetch(t *testing.T) {
	_, f := newStandIn(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := f.Get(ctx, "http://slow.example.test/", extract.Options{}); err == nil {
		t.Fatal("expected an error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("cancellation did not stop the fetch promptly")
	}
}

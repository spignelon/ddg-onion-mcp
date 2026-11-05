// SPDX-License-Identifier: GPL-3.0-or-later

package ddg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/spignelon/ddg-onion-mcp/internal/torhttp"
)

// liteFixture is synthetic markup modelled on DuckDuckGo Lite's results table:
// result-link anchors, result-snippet cells, a sponsored row, redirector links
// and a "Next Page" form with hidden fields. It is NOT a capture of the live
// page; the live onion service has not been parsed by these tests.
const liteFixture = `<!DOCTYPE html><html><head><title>DuckDuckGo</title></head><body>
<form action="/lite/" method="post"><input class="query" type="text" name="q" value="golang tor"></form>
<table border="0">
<tr class="result-sponsored"><td valign="top">1.&nbsp;</td><td><a rel="nofollow" href="//duckduckgo.com/y.js?ad_domain=ads.example&amp;u3=x" class='result-link'>Buy Tor Gear Now</a></td></tr>
<tr class="result-sponsored"><td>&nbsp;</td><td class='result-snippet'>Sponsored snippet</td></tr>
<tr><td valign="top">1.&nbsp;</td><td><a rel="nofollow" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2Feffective_go%3Fa%3D1%26b%3D2&amp;rut=abc" class='result-link'>Effective Go - <b>The Go</b> Programming Language</a></td></tr>
<tr><td>&nbsp;&nbsp;&nbsp;</td><td class='result-snippet'>Tips for writing clear, idiomatic <b>Go</b> code.</td></tr>
<tr><td>&nbsp;&nbsp;&nbsp;</td><td><span class='link-text'>go.dev/doc/effective_go</span></td></tr>
<tr><td valign="top">2.&nbsp;</td><td><a rel="nofollow" href="https://pkg.go.dev/golang.org/x/net/proxy" class='result-link'>proxy package - golang.org/x/net/proxy</a></td></tr>
<tr><td>&nbsp;&nbsp;&nbsp;</td><td class='result-snippet'>Package proxy provides support for a variety of protocols to proxy network data.</td></tr>
<tr><td valign="top">3.&nbsp;</td><td><a rel="nofollow" href="https://pkg.go.dev/golang.org/x/net/proxy" class='result-link'>duplicate of result 2</a></td></tr>
<tr><td>&nbsp;&nbsp;&nbsp;</td><td class='result-snippet'>Same URL again.</td></tr>
<tr><td valign="top">4.&nbsp;</td><td><a rel="nofollow" href="/l/?uddg=http%3A%2F%2Fexample.org%2F" class='result-link'>Relative redirector</a></td></tr>
<tr><td>&nbsp;&nbsp;&nbsp;</td><td class='result-snippet'>Relative redirect snippet.</td></tr>
<tr><td valign="top">5.&nbsp;</td><td><a rel="nofollow" href="//duckduckgo.com/l/?uddg=javascript%3Aalert(1)" class='result-link'>Bad scheme</a></td></tr>
</table>
<form action="/lite/" method="post">
<input type="submit" class='navbutton' value="Next Page &gt;">
<input type="hidden" name="q" value="golang tor"><input type="hidden" name="s" value="30"><input type="hidden" name="nextParams" value="">
<input type="hidden" name="v" value="l"><input type="hidden" name="o" value="json"><input type="hidden" name="dc" value="31">
<input type="hidden" name="api" value="d.js"><input type="hidden" name="vqd" value="4-123456"><input type="hidden" name="kl" value="wt-wt">
</form>
</body></html>`

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }

func TestParseLiteExtractsResults(t *testing.T) {
	base := mustURL(DefaultBaseURL)
	got, next, challenge, err := parseLite(strings.NewReader(liteFixture), base)
	if err != nil || challenge {
		t.Fatalf("err=%v challenge=%v", err, challenge)
	}
	want := []Result{
		{Title: "Effective Go - The Go Programming Language", URL: "https://go.dev/doc/effective_go?a=1&b=2", Snippet: "Tips for writing clear, idiomatic Go code."},
		{Title: "proxy package - golang.org/x/net/proxy", URL: "https://pkg.go.dev/golang.org/x/net/proxy", Snippet: "Package proxy provides support for a variety of protocols to proxy network data."},
		{Title: "duplicate of result 2", URL: "https://pkg.go.dev/golang.org/x/net/proxy", Snippet: "Same URL again."},
		{Title: "Relative redirector", URL: "http://example.org/", Snippet: "Relative redirect snippet."},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("result %d = %+v\nwant     %+v", i, got[i], want[i])
		}
	}
	for _, r := range got {
		if strings.Contains(r.Title, "Buy Tor Gear") || strings.Contains(r.URL, "y.js") {
			t.Errorf("sponsored result leaked: %+v", r)
		}
		if strings.Contains(r.URL, "javascript:") {
			t.Errorf("javascript: URL leaked: %+v", r)
		}
	}

	if next == nil {
		t.Fatal("next-page form not found")
	}
	for k, v := range map[string]string{"q": "golang tor", "s": "30", "dc": "31", "vqd": "4-123456", "api": "d.js", "kl": "wt-wt"} {
		if next.values.Get(k) != v {
			t.Errorf("next form %s = %q, want %q", k, next.values.Get(k), v)
		}
	}
	if got := next.action(base).String(); got != DefaultBaseURL {
		t.Errorf("next action = %s", got)
	}
}

func TestNextFormToAnotherHostIsNotFollowed(t *testing.T) {
	base := mustURL("https://abc.onion/lite/")
	n := &nextPage{formAction: "https://evil.example/steal", values: url.Values{"q": {"x"}}}
	if got := n.action(base); got.Host != "abc.onion" {
		t.Fatalf("form action followed to %s", got)
	}
}

func TestParseLiteDetectsChallengePages(t *testing.T) {
	page := `<html><body><div class="anomaly-modal">Unfortunately, bots use DuckDuckGo too. Please complete the following challenge. Select all squares containing a duck.</div></body></html>`
	hits, _, challenge, err := parseLite(strings.NewReader(page), mustURL(DefaultBaseURL))
	if err != nil || len(hits) != 0 || !challenge {
		t.Fatalf("hits=%d challenge=%v err=%v", len(hits), challenge, err)
	}
	// A genuine empty-results page is not a challenge.
	_, _, challenge, _ = parseLite(strings.NewReader(`<html><body>No results.</body></html>`), mustURL(DefaultBaseURL))
	if challenge {
		t.Fatal("empty results page misread as a challenge")
	}
}

func TestValidateOptions(t *testing.T) {
	for _, o := range []Options{{}, {Region: "us-en", SafeSearch: "strict", TimeRange: "w"}, {Region: " WT-WT ", SafeSearch: "OFF"}} {
		if _, err := ValidateOptions(o); err != nil {
			t.Errorf("%+v rejected: %v", o, err)
		}
	}
	for _, o := range []Options{{Region: "usa"}, {Region: "us_en"}, {SafeSearch: "maybe"}, {TimeRange: "h"}} {
		if _, err := ValidateOptions(o); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
}

// ddgStandIn answers DuckDuckGo Lite POSTs and records them.
type ddgStandIn struct {
	mu    sync.Mutex
	forms []url.Values
	hdrs  []http.Header
	paths []string
}

func (d *ddgStandIn) handler(pages ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		d.mu.Lock()
		d.forms = append(d.forms, form)
		d.hdrs = append(d.hdrs, r.Header.Clone())
		d.paths = append(d.paths, r.Method+" "+r.URL.Path)
		n := len(d.forms)
		d.mu.Unlock()
		if n > len(pages) {
			n = len(pages)
		}
		fmt.Fprint(w, pages[n-1])
	})
}

func newClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	hc := torhttp.NewWithDialer(func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
	}, "test-agent/9")
	return &Client{HTTP: hc, BaseURL: "http://ddg.example.test/lite/"}
}

const page2 = `<table><tr><td>31.</td><td><a class='result-link' href="https://third.example/one">Third site</a></td></tr>
<tr><td></td><td class='result-snippet'>Page two snippet.</td></tr></table>`

func TestSearchSendsExpectedFormAndFollowsPages(t *testing.T) {
	d := &ddgStandIn{}
	c := newClient(t, d.handler(liteFixture, page2))
	results, err := c.Search(context.Background(), "golang tor", 10, Options{Region: "us-en", SafeSearch: "strict", TimeRange: "m"})
	if err != nil {
		t.Fatal(err)
	}
	// 3 unique results from page 1 (the duplicate URL is dropped) + 1 from page 2.
	if len(results) != 4 {
		t.Fatalf("got %d results: %+v", len(results), results)
	}
	for i, r := range results {
		if r.Position != i+1 {
			t.Errorf("result %d has position %d", i, r.Position)
		}
	}
	if results[3].URL != "https://third.example/one" {
		t.Errorf("page 2 result missing: %+v", results[3])
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.forms) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(d.forms))
	}
	first := d.forms[0]
	for k, v := range map[string]string{"q": "golang tor", "kl": "us-en", "kp": "1", "df": "m"} {
		if first.Get(k) != v {
			t.Errorf("first request %s = %q, want %q", k, first.Get(k), v)
		}
	}
	if d.paths[0] != "POST /lite/" {
		t.Errorf("request = %q (query must travel in the POST body, not the URL)", d.paths[0])
	}
	if d.hdrs[0].Get("User-Agent") != "test-agent/9" {
		t.Errorf("User-Agent = %q", d.hdrs[0].Get("User-Agent"))
	}
	second := d.forms[1]
	if second.Get("s") != "30" || second.Get("vqd") != "4-123456" {
		t.Errorf("second request did not carry the next-page fields: %v", second)
	}
}

func TestSearchStopsAtMaxResults(t *testing.T) {
	d := &ddgStandIn{}
	c := newClient(t, d.handler(liteFixture, page2))
	results, err := c.Search(context.Background(), "x", 2, Options{})
	if err != nil || len(results) != 2 {
		t.Fatalf("results=%d err=%v", len(results), err)
	}
	if len(d.forms) != 1 {
		t.Fatalf("fetched %d pages for 2 results", len(d.forms))
	}
}

func TestSearchReportsBlocksWithoutRetrying(t *testing.T) {
	for _, status := range []int{http.StatusNotAcceptable, http.StatusForbidden, http.StatusTooManyRequests, http.StatusAccepted} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var hits int
			c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				w.WriteHeader(status)
			}))
			_, err := c.Search(context.Background(), "golang", 5, Options{})
			var be *BlockedError
			if !errors.As(err, &be) || be.Status != status {
				t.Fatalf("error = %v, want *BlockedError with status %d", err, status)
			}
			if hits != 1 {
				t.Fatalf("client sent %d requests; it must not retry", hits)
			}
		})
	}
}

func TestSearchReportsChallengePage(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body>Unfortunately, bots use DuckDuckGo too. Select all squares containing a duck.</body></html>`)
	}))
	_, err := c.Search(context.Background(), "golang", 5, Options{})
	var be *BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("error = %v, want *BlockedError", err)
	}
}

func TestSearchValidatesInput(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("request sent") }))
	if _, err := c.Search(context.Background(), "   ", 5, Options{}); err == nil {
		t.Error("empty query accepted")
	}
	if _, err := c.Search(context.Background(), strings.Repeat("a", 401), 5, Options{}); err == nil {
		t.Error("overlong query accepted")
	}
	if _, err := c.Search(context.Background(), "ok", 5, Options{Region: "nope"}); err == nil {
		t.Error("bad region accepted")
	}
}

func TestSearchSendsBrowserFormSubmissionHeaders(t *testing.T) {
	d := &ddgStandIn{}
	srv := httptest.NewServer(d.handler(liteFixture))
	defer srv.Close()
	hc := torhttp.NewWithProfile(func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
	}, torhttp.TorBrowserProfile())
	c := &Client{HTTP: hc, BaseURL: "http://ddg.example.test/lite/", MaxPages: 1}
	if _, err := c.Search(context.Background(), "golang", 3, Options{}); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	h := d.hdrs[0]
	for name, want := range map[string]string{
		"User-Agent":     "Mozilla/5.0 (X11; Linux x86_64; rv:153.0) Gecko/20100101 Firefox/153.0",
		"Accept":         "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Content-Type":   "application/x-www-form-urlencoded",
		"Origin":         "http://ddg.example.test",
		"Referer":        "http://ddg.example.test/lite/",
		"Sec-Fetch-Site": "same-origin",
		"Sec-Fetch-Mode": "navigate",
		"Sec-Fetch-Dest": "document",
		"Sec-Fetch-User": "?1",
	} {
		if h.Get(name) != want {
			t.Errorf("%s = %q, want %q", name, h.Get(name), want)
		}
	}
}

// SPDX-License-Identifier: GPL-3.0-or-later

// Package ddg searches DuckDuckGo through its Lite onion service, over Tor.
package ddg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/spignelon/ddg-onion-mcp/internal/torhttp"
)

// DefaultBaseURL is DuckDuckGo's Lite page on its v3 onion service.
const DefaultBaseURL = "https://duckduckgogg42xjoc72x3sjasowoarfbgcmvfimaftt6twagswzczad.onion/lite/"

// Result is one search hit.
type Result struct {
	Position int
	Title    string
	URL      string
	Snippet  string
}

// Options are per-search settings. Empty values mean "default".
type Options struct {
	Region     string // "wt-wt" (any), "us-en", "de-de", ...
	SafeSearch string // "off", "moderate" (default) or "strict"
	TimeRange  string // "", "d", "w", "m" or "y"
}

// BlockedError means DuckDuckGo refused or challenged the request. The client
// does not retry with a different identity.
type BlockedError struct {
	Status int
	Detail string
}

func (e *BlockedError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("DuckDuckGo refused the request (HTTP %d): %s", e.Status, e.Detail)
	}
	return "DuckDuckGo refused the request: " + e.Detail
}

// Client queries DuckDuckGo Lite.
type Client struct {
	HTTP     *torhttp.Client
	BaseURL  string // default DefaultBaseURL
	MaxPages int    // result pages to follow (default 3)
}

var regionRE = regexp.MustCompile(`^[a-z]{2}-[a-z]{2}$`)

// ValidateOptions normalises and checks o.
func ValidateOptions(o Options) (Options, error) {
	o.Region = strings.ToLower(strings.TrimSpace(o.Region))
	if o.Region != "" && !regionRE.MatchString(o.Region) {
		return o, fmt.Errorf("invalid region %q (use a code like us-en, de-de or wt-wt)", o.Region)
	}
	o.SafeSearch = strings.ToLower(strings.TrimSpace(o.SafeSearch))
	switch o.SafeSearch {
	case "", "off", "moderate", "strict":
	default:
		return o, fmt.Errorf("invalid safe_search %q (use off, moderate or strict)", o.SafeSearch)
	}
	o.TimeRange = strings.ToLower(strings.TrimSpace(o.TimeRange))
	switch o.TimeRange {
	case "", "d", "w", "m", "y":
	default:
		return o, fmt.Errorf("invalid time_range %q (use d, w, m or y)", o.TimeRange)
	}
	return o, nil
}

// Search returns up to max results for query.
func (c *Client) Search(ctx context.Context, query string, max int, o Options) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("query must not be empty")
	}
	if utf8.RuneCountInString(query) > 400 {
		return nil, errors.New("query is too long (max 400 characters)")
	}
	if max < 1 {
		max = 10
	}
	o, err := ValidateOptions(o)
	if err != nil {
		return nil, err
	}
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("invalid DuckDuckGo base URL: %w", err)
	}
	pages := c.MaxPages
	if pages <= 0 {
		pages = 3
	}

	form := url.Values{"q": {query}, "kl": {orDefault(o.Region, "wt-wt")}}
	switch o.SafeSearch {
	case "off":
		form.Set("kp", "-2")
	case "strict":
		form.Set("kp", "1")
	default:
		form.Set("kp", "-1")
	}
	if o.TimeRange != "" {
		form.Set("df", o.TimeRange)
	}

	var results []Result
	seen := map[string]bool{}
	target := baseURL
	for page := 0; page < pages; page++ {
		hits, next, err := c.post(ctx, target, form)
		if err != nil {
			if len(results) > 0 {
				break // keep what earlier pages gave us
			}
			return nil, err
		}
		for _, h := range hits {
			if seen[h.URL] {
				continue
			}
			seen[h.URL] = true
			h.Position = len(results) + 1
			results = append(results, h)
			if len(results) >= max {
				return results, nil
			}
		}
		if next == nil {
			break
		}
		target, form = next.action(baseURL), next.values
	}
	return results, nil
}

func (c *Client) post(ctx context.Context, target *url.URL, form url.Values) ([]Result, *nextPage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("contacting DuckDuckGo over Tor: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, nil, &BlockedError{Status: resp.StatusCode, Detail: statusHint(resp.StatusCode)}
	}
	hits, next, challenge, err := parseLite(io.LimitReader(resp.Body, 2<<20), target)
	if err != nil {
		return nil, nil, err
	}
	if len(hits) == 0 && challenge {
		return nil, nil, &BlockedError{Detail: "the page is a bot-detection challenge, not search results"}
	}
	return hits, next, nil
}

func statusHint(code int) string {
	switch code {
	case http.StatusNotAcceptable:
		return "406 Not Acceptable; the service rejected this client"
	case http.StatusForbidden, http.StatusTooManyRequests, http.StatusAccepted:
		return "rate limited or flagged as automated traffic; try again later"
	}
	return http.StatusText(code)
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// ------------------------------------------------------------------ parsing

type nextPage struct {
	formAction string
	values     url.Values
}

func (n *nextPage) action(base *url.URL) *url.URL {
	if n.formAction == "" {
		return base
	}
	u, err := base.Parse(n.formAction)
	if err != nil || u.Host != base.Host {
		return base // never follow a form to another host
	}
	return u
}

var challengeMarkers = []string{"anomaly", "captcha", "unusual traffic", "select all squares", "are you a human", "bots use duckduckgo"}

func parseLite(r io.Reader, base *url.URL) ([]Result, *nextPage, bool, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, nil, false, fmt.Errorf("parsing DuckDuckGo response: %w", err)
	}

	var results []Result
	var cur *Result
	flush := func() {
		if cur != nil && cur.URL != "" && cur.Title != "" {
			results = append(results, *cur)
		}
		cur = nil
	}

	var walk func(n *html.Node, sponsored bool)
	walk = func(n *html.Node, sponsored bool) {
		if n.Type == html.ElementNode {
			switch {
			case n.DataAtom == atom.Tr && hasClass(n, "result-sponsored"):
				sponsored = true
			case n.DataAtom == atom.A && hasClass(n, "result-link"):
				flush()
				if !sponsored {
					if link := resolveResultURL(attr(n, "href"), base); link != "" {
						cur = &Result{Title: collapse(textOf(n)), URL: link}
					}
				}
				return
			case n.DataAtom == atom.Td && hasClass(n, "result-snippet"):
				if cur != nil && cur.Snippet == "" && !sponsored {
					cur.Snippet = collapse(textOf(n))
				}
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, sponsored)
		}
	}
	walk(doc, false)
	flush()

	next := findNext(doc)
	challenge := false
	if len(results) == 0 {
		low := strings.ToLower(textOf(doc))
		for _, m := range challengeMarkers {
			if strings.Contains(low, m) {
				challenge = true
				break
			}
		}
	}
	return results, next, challenge, nil
}

// resolveResultURL returns the destination of a result link, unwrapping
// DuckDuckGo's /l/?uddg= redirector and dropping ad links.
func resolveResultURL(href string, base *url.URL) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	u, err := base.Parse(href)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if host == "duckduckgo.com" || strings.HasSuffix(host, ".duckduckgo.com") || u.Host == base.Host {
		switch {
		case u.Path == "/l/" || u.Path == "/l":
			dest := u.Query().Get("uddg")
			if dest == "" {
				return ""
			}
			d, err := url.Parse(dest)
			if err != nil || (d.Scheme != "http" && d.Scheme != "https") {
				return ""
			}
			return d.String()
		case strings.HasPrefix(u.Path, "/y.js"):
			return "" // advertisement
		default:
			return "" // internal DuckDuckGo link, not a result
		}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.String()
}

func findNext(doc *html.Node) *nextPage {
	var found *nextPage
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found != nil {
			return
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Form {
			vals := url.Values{}
			isNext := false
			var visit func(*html.Node)
			visit = func(m *html.Node) {
				if m.Type == html.ElementNode && m.DataAtom == atom.Input {
					typ := strings.ToLower(attr(m, "type"))
					switch {
					case typ == "submit" && hasClass(m, "navbutton") && strings.Contains(strings.ToLower(attr(m, "value")), "next"):
						isNext = true
					case typ == "hidden" && attr(m, "name") != "":
						vals.Add(attr(m, "name"), attr(m, "value"))
					}
				}
				for c := m.FirstChild; c != nil; c = c.NextSibling {
					visit(c)
				}
			}
			visit(n)
			if isNext && len(vals) > 0 {
				found = &nextPage{formAction: attr(n, "action"), values: vals}
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return found
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasClass(n *html.Node, class string) bool {
	for _, c := range strings.Fields(attr(n, "class")) {
		if c == class {
			return true
		}
	}
	return false
}

func textOf(n *html.Node) string {
	var sb strings.Builder
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.ElementNode && (n.DataAtom == atom.Script || n.DataAtom == atom.Style) {
			return
		}
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
			sb.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(n)
	return sb.String()
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// SPDX-License-Identifier: GPL-3.0-or-later

// Package fetch downloads web pages through Tor and extracts their text.
//
// Unlike many fetchers it never resolves host names locally: validation works
// on the literal URL only, and name resolution is left to Tor. That keeps DNS
// lookups off the local network, at the cost of not being able to detect a
// public name that points at a private address. Tor exit nodes refuse private
// destinations, which covers that case.
package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html/charset"
	"golang.org/x/net/idna"

	"github.com/spignelon/ddg-onion-mcp/internal/extract"
	"github.com/spignelon/ddg-onion-mcp/internal/torhttp"
)

// BlockedError reports a URL that policy refuses to fetch.
type BlockedError struct{ Reason string }

func (e *BlockedError) Error() string { return e.Reason }

// Page is a fetched and extracted document.
type Page struct {
	URL         string // final URL after redirects
	Title       string
	Text        string
	ContentType string
	// SourceTruncated is true when the download hit the size cap.
	SourceTruncated bool
	// Method says how the text was produced ("readability", "full-page", "raw").
	Method string
	// Sparse is true when almost no text was found (often a JavaScript-only page).
	Sparse bool
}

// Fetcher downloads pages through a Tor-only client.
type Fetcher struct {
	Client       *torhttp.Client
	MaxBytes     int64 // download cap (default 2 MiB)
	MaxRedirects int   // default 5
	AllowOnion   bool  // permit .onion destinations
}

var blockedPrefixes = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "64:ff9b::/96", "100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
)

var blockedSuffixes = []string{".localhost", ".local", ".internal", ".home.arpa", ".lan"}

var numericHost = regexp.MustCompile(`(?i)^(0x[0-9a-f]+|[0-9]+)(\.(0x[0-9a-f]+|[0-9]+))*$`)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// ValidateURL checks that raw is an http(s) URL to a public destination and
// returns it normalised. It does no DNS lookups.
func ValidateURL(raw string, allowOnion bool) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return nil, &BlockedError{"not a valid URL"}
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, &BlockedError{fmt.Sprintf("unsupported URL scheme %q (only http and https)", u.Scheme)}
	}
	if u.User != nil {
		return nil, &BlockedError{"URLs with embedded credentials are not allowed"}
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return nil, &BlockedError{"URL has no host"}
	}
	if p := u.Port(); p != "" {
		var n int
		if _, err := fmt.Sscanf(p, "%d", &n); err != nil || n < 1 || n > 65535 {
			return nil, &BlockedError{"invalid port"}
		}
	}

	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" {
			return nil, &BlockedError{"IPv6 zone identifiers are not allowed"}
		}
		ip = ip.Unmap()
		for _, p := range blockedPrefixes {
			if p.Contains(ip) {
				return nil, &BlockedError{fmt.Sprintf("refusing non-public address %s", ip)}
			}
		}
	} else {
		if numericHost.MatchString(host) {
			return nil, &BlockedError{"ambiguous numeric host name (use a normal dotted IPv4 address or a domain)"}
		}
		if host == "localhost" {
			return nil, &BlockedError{"refusing loopback host"}
		}
		for _, s := range blockedSuffixes {
			if strings.HasSuffix(host, s) {
				return nil, &BlockedError{fmt.Sprintf("refusing local-network host name (*%s)", s)}
			}
		}
		if !strings.Contains(host, ".") {
			return nil, &BlockedError{"refusing single-label host name"}
		}
		if strings.HasSuffix(host, ".onion") && !allowOnion {
			return nil, &BlockedError{".onion destinations are disabled"}
		}
		ascii, err := idna.Lookup.ToASCII(host)
		if err != nil {
			return nil, &BlockedError{"invalid host name"}
		}
		host = ascii
	}

	out := *u
	out.Scheme = scheme
	out.Fragment = ""
	if port := u.Port(); port != "" {
		out.Host = hostForURL(host) + ":" + port
	} else {
		out.Host = hostForURL(host)
	}
	return &out, nil
}

func hostForURL(host string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

// Get downloads rawURL through Tor and extracts its text.
func (f *Fetcher) Get(ctx context.Context, rawURL string, opts extract.Options) (*Page, error) {
	maxBytes := f.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 2 << 20
	}
	maxRedirects := f.MaxRedirects
	if maxRedirects <= 0 {
		maxRedirects = 5
	}

	cur, err := ValidateURL(rawURL, f.AllowOnion)
	if err != nil {
		return nil, err
	}

	var prev *url.URL
	for hop := 0; ; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cur.String(), nil)
		if err != nil {
			return nil, err
		}
		// Identity headers (User-Agent, Accept, ...) come from the client's
		// profile. The first request is like an address typed by the user;
		// later ones are redirects.
		site := "none"
		if hop > 0 {
			site = "cross-site"
			if prev != nil && prev.Scheme == cur.Scheme && prev.Host == cur.Host {
				site = "same-origin"
			}
		}
		f.Client.Navigate(req, torhttp.Nav{Site: site})
		resp, err := f.Client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetching %s over Tor: %w", cur.Host, err)
		}

		switch resp.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			loc := resp.Header.Get("Location")
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if loc == "" {
				return nil, errors.New("redirect without a Location header")
			}
			if hop >= maxRedirects {
				return nil, fmt.Errorf("too many redirects (more than %d)", maxRedirects)
			}
			next, err := cur.Parse(loc)
			if err != nil {
				return nil, &BlockedError{"redirect to an invalid URL"}
			}
			prev = cur
			cur, err = ValidateURL(next.String(), f.AllowOnion)
			if err != nil {
				return nil, fmt.Errorf("redirect target rejected: %w", err)
			}
			continue
		}

		page, err := f.readBody(resp, cur, maxBytes, opts)
		resp.Body.Close()
		return page, err
	}
}

func (f *Fetcher) readBody(resp *http.Response, final *url.URL, maxBytes int64, opts extract.Options) (*Page, error) {
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, final.Host)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading response from %s: %w", final.Host, err)
	}
	truncated := int64(len(body)) > maxBytes
	if truncated {
		body = body[:maxBytes]
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = http.DetectContentType(body)
	}
	media, _, err := mime.ParseMediaType(ct)
	if err != nil {
		media = strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	}
	if !textual(media) {
		return nil, fmt.Errorf("unsupported content type %q (only text-like pages can be read)", media)
	}

	// Decode to UTF-8 using the declared (or sniffed) charset.
	decoded := body
	if r, err := charset.NewReader(bytes.NewReader(body), ct); err == nil {
		if b, err := io.ReadAll(r); err == nil {
			decoded = b
		}
	}

	page := &Page{URL: final.String(), ContentType: media, SourceTruncated: truncated}
	switch media {
	case "text/html", "application/xhtml+xml":
		opts.BaseURL = final
		res, err := extract.HTML(decoded, opts)
		if err != nil {
			return nil, err
		}
		page.Title, page.Text, page.Method, page.Sparse = res.Title, res.Text, res.Method, res.Sparse
	default:
		page.Text = strings.TrimSpace(string(decoded))
		page.Method = "raw"
	}
	return page, nil
}

func textual(media string) bool {
	if strings.HasPrefix(media, "text/") {
		return true
	}
	switch media {
	case "application/xhtml+xml", "application/xml", "application/json", "application/ld+json", "application/rss+xml", "application/atom+xml":
		return true
	}
	return strings.HasSuffix(media, "+json") || strings.HasSuffix(media, "+xml")
}

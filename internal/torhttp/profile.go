// SPDX-License-Identifier: GPL-3.0-or-later

package torhttp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/textproto"
	"os"
	"strings"
)

// Header is one request header sent on every request.
type Header struct{ Name, Value string }

// Profile is the set of identity headers a Client sends.
//
// A profile only covers HTTP request headers. It does not change the TLS
// handshake, HTTP/2 settings or header order, all of which still look like a
// Go client rather than a Firefox-based browser.
type Profile struct {
	Name    string
	Headers []Header
	// Browser makes Navigate add the Fetch Metadata headers (Sec-Fetch-*),
	// Origin and Referer that a browser sends on page navigations.
	Browser bool
}

// TorBrowserProfile mirrors the request headers of a Tor Browser page
// navigation. The values were captured from a real Tor Browser (Firefox 153
// based, Linux) at https://myip.wtf/headers and WILL go stale: Tor Browser
// changes its version about every four weeks. Refresh them with
// --identity-file (see OverrideFromFile).
func TorBrowserProfile() Profile {
	return Profile{
		Name:    "tor-browser",
		Browser: true,
		Headers: []Header{
			{"User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:153.0) Gecko/20100101 Firefox/153.0"},
			{"Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
			{"Accept-Language", "en-US,en;q=0.9"},
			{"Accept-Encoding", "gzip, deflate, br, zstd"},
			{"Upgrade-Insecure-Requests", "1"},
			{"Sec-GPC", "1"},
			{"TE", "trailers"},
			{"Priority", "u=0, i"},
		},
	}
}

// HonestProfile identifies the program by name and sends no browser headers.
func HonestProfile(userAgent string) Profile {
	if userAgent == "" {
		userAgent = "ddg-onion-mcp"
	}
	return Profile{
		Name: "honest",
		Headers: []Header{
			{"User-Agent", userAgent},
			{"Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.1"},
		},
	}
}

// forbidden headers can never be set from a profile: they would break the
// protocol, or send credentials.
var forbidden = map[string]bool{
	"host": true, "content-length": true, "content-type": true, "connection": true,
	"transfer-encoding": true, "upgrade": true, "keep-alive": true, "proxy-authorization": true,
	"proxy-connection": true, "cookie": true, "authorization": true, "origin": true, "referer": true,
}

// With returns a copy of p with the given headers set (case-insensitive name
// match). An empty value removes the header.
func (p Profile) With(overrides map[string]string) (Profile, error) {
	out := Profile{Name: p.Name, Browser: p.Browser, Headers: append([]Header(nil), p.Headers...)}
	for name, value := range overrides {
		name = strings.TrimSpace(name)
		if name == "" || strings.ContainsAny(name, " \t\r\n:") || hasCtl(name) {
			return Profile{}, fmt.Errorf("invalid header name %q", name)
		}
		if forbidden[strings.ToLower(name)] {
			return Profile{}, fmt.Errorf("header %q cannot be set from a profile", name)
		}
		if hasCtl(value) {
			return Profile{}, fmt.Errorf("invalid value for header %q (control characters)", name)
		}
		replaced := false
		kept := out.Headers[:0:0]
		for _, h := range out.Headers {
			if strings.EqualFold(h.Name, name) {
				if value != "" && !replaced {
					kept = append(kept, Header{h.Name, value})
					replaced = true
				}
				continue
			}
			kept = append(kept, h)
		}
		if !replaced && value != "" {
			kept = append(kept, Header{name, value})
		}
		out.Headers = kept
	}
	return out, nil
}

// OverrideFromFile applies a JSON object of header overrides, e.g.
//
//	{"User-Agent": "Mozilla/5.0 (X11; Linux x86_64; rv:154.0) Gecko/20100101 Firefox/154.0"}
//
// so the profile can be refreshed from https://myip.wtf/headers without a
// rebuild. A value of "" removes a header.
func (p Profile) OverrideFromFile(path string) (Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, fmt.Errorf("reading identity file: %w", err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return Profile{}, fmt.Errorf("identity file must be a JSON object of header names to string values: %w", err)
	}
	return p.With(m)
}

func hasCtl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// apply sets the profile's headers on req, replacing any the caller set.
func (p Profile) apply(h http.Header) {
	for _, hd := range p.Headers {
		delete(h, textproto.CanonicalMIMEHeaderKey(hd.Name))
		delete(h, hd.Name)
		// Assigned directly so the name keeps its exact spelling (for
		// example "Sec-GPC") on HTTP/1.1; HTTP/2 lower-cases it anyway.
		h[hd.Name] = []string{hd.Value}
	}
}

// Nav describes a page navigation for Navigate.
type Nav struct {
	// Site is the Sec-Fetch-Site value: "none" (address typed by the user),
	// "same-origin" or "cross-site".
	Site string
	// Origin and Referer are sent as given when non-empty (form submissions).
	Origin  string
	Referer string
}

// Navigate marks req as a top-level page navigation the way a browser would,
// when the profile is browser-like. With the honest profile it does nothing.
func (c *Client) Navigate(req *http.Request, n Nav) {
	if !c.profile.Browser {
		return
	}
	req.Header["Sec-Fetch-Dest"] = []string{"document"}
	req.Header["Sec-Fetch-Mode"] = []string{"navigate"}
	req.Header["Sec-Fetch-Site"] = []string{n.Site}
	req.Header["Sec-Fetch-User"] = []string{"?1"}
	if n.Origin != "" {
		req.Header.Set("Origin", n.Origin)
	}
	if n.Referer != "" {
		req.Header.Set("Referer", n.Referer)
	}
}

// ProfileName reports which identity profile the client uses.
func (c *Client) ProfileName() string { return c.profile.Name }

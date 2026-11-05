// SPDX-License-Identifier: GPL-3.0-or-later

// Package extract turns an HTML page into readable Markdown or plain text.
//
// It uses the same recipe as the official MCP "fetch" server: Mozilla's
// Readability algorithm (here the Go port from Readeck) to find the main
// article, then an HTML-to-Markdown converter. When Readability finds no
// article (forum threads, listings, link pages), the whole page is converted
// instead, minus navigation, footers and other page chrome.
//
// Neither library is allowed to touch the network: this package only ever
// works on bytes it is given. Never call readability.FromURL here; it opens
// its own direct connection and would bypass Tor.
package extract

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Mode selects the output format.
type Mode string

const (
	// ModeMarkdown (default): main article as Markdown (headings, lists,
	// links and code kept).
	ModeMarkdown Mode = "markdown"
	// ModeText: main article as plain text.
	ModeText Mode = "text"
	// ModeFull: the whole page as Markdown, skipping Readability. Use it for
	// pages where the article extractor drops content.
	ModeFull Mode = "full"
)

// minArticleChars is the least text Readability must find before its result
// is trusted over the full-page fallback.
const minArticleChars = 200

// ParseMode validates a user-supplied mode; "" means ModeMarkdown.
func ParseMode(s string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case "", ModeMarkdown:
		return ModeMarkdown, nil
	case ModeText:
		return ModeText, nil
	case ModeFull:
		return ModeFull, nil
	}
	return "", fmt.Errorf("unknown parse_mode %q (use markdown, text or full)", s)
}

// Options configures HTML.
type Options struct {
	Mode Mode
	// BaseURL is the page's final URL; it resolves relative links.
	BaseURL *url.URL
	// MaxChars caps the output in characters (0 = 1,000,000).
	MaxChars int
}

// Result is the extracted page.
type Result struct {
	Title string
	Text  string
	// Method is "readability" or "full-page".
	Method string
	// Sparse is true when very little text came out, which usually means the
	// page builds its content with JavaScript (which this server cannot run).
	Sparse bool
}

// HTML extracts the page's content from UTF-8 HTML bytes.
func HTML(data []byte, o Options) (Result, error) {
	if o.Mode == "" {
		o.Mode = ModeMarkdown
	}
	if o.MaxChars <= 0 {
		o.MaxChars = 1_000_000
	}
	base := o.BaseURL
	if base == nil {
		base = &url.URL{}
	}

	if o.Mode != ModeFull {
		if res, ok := viaReadability(data, base, o.Mode); ok {
			return finish(res, o.MaxChars), nil
		}
	}

	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return Result{}, fmt.Errorf("parsing HTML: %w", err)
	}
	title := docTitle(doc)
	md, err := newConverter().ConvertNode(doc, converter.WithDomain(domainOf(base)))
	if err != nil {
		return Result{}, fmt.Errorf("converting page to Markdown: %w", err)
	}
	return finish(Result{Title: title, Text: string(md), Method: "full-page"}, o.MaxChars), nil
}

func viaReadability(data []byte, base *url.URL, mode Mode) (Result, bool) {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return Result{}, false
	}
	art, err := readability.FromDocument(doc, base)
	if err != nil || art.Node == nil {
		return Result{}, false
	}
	var text string
	switch mode {
	case ModeText:
		var buf bytes.Buffer
		if err := art.RenderText(&buf); err != nil {
			return Result{}, false
		}
		text = buf.String()
	default:
		md, err := newConverter().ConvertNode(art.Node, converter.WithDomain(domainOf(base)))
		if err != nil {
			return Result{}, false
		}
		text = string(md)
	}
	if utf8.RuneCountInString(strings.TrimSpace(text)) < minArticleChars {
		return Result{}, false
	}
	return Result{Title: strings.TrimSpace(art.Title()), Text: text, Method: "readability"}, true
}

func newConverter() *converter.Converter {
	conv := converter.NewConverter(converter.WithPlugins(
		base.NewBasePlugin(),
		commonmark.NewCommonmarkPlugin(commonmark.WithHeadingStyle(commonmark.HeadingStyleATX)),
	))
	// Page chrome and non-text elements.
	for _, tag := range []string{"nav", "footer", "aside", "form", "noscript", "iframe", "svg", "canvas", "button", "select", "dialog", "template"} {
		conv.Register.TagType(tag, converter.TagTypeRemove, converter.PriorityStandard)
	}
	return conv
}

func domainOf(u *url.URL) string {
	if u == nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func finish(r Result, max int) Result {
	r.Text = strings.TrimSpace(r.Text)
	if utf8.RuneCountInString(r.Text) > max {
		r.Text = string([]rune(r.Text)[:max])
	}
	r.Sparse = utf8.RuneCountInString(r.Text) < 100
	return r
}

func docTitle(doc *html.Node) string {
	var title string
	var f func(*html.Node)
	f = func(n *html.Node) {
		if title != "" {
			return
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Title && n.FirstChild != nil {
			title = strings.Join(strings.Fields(n.FirstChild.Data), " ")
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(doc)
	return title
}

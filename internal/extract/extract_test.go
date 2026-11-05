// SPDX-License-Identifier: GPL-3.0-or-later

package extract

import (
	"net/url"
	"strings"
	"testing"
)

const article = `<!doctype html><html><head><title>Go Concurrency Patterns - Example Blog</title>
<script>var tracking = "SCRIPT-SHOULD-NOT-APPEAR";</script><style>.x{color:red}</style></head>
<body>
<nav><a href="/home">NAV-SHOULD-NOT-APPEAR</a></nav>
<header><div class="site">Example Blog</div></header>
<main><article>
<h1>Go Concurrency Patterns</h1>
<p>Concurrency in Go is built on goroutines and channels. A goroutine is a lightweight thread managed by the Go runtime, and channels let goroutines communicate safely without sharing memory directly.</p>
<h2>Worker pools</h2>
<p>A worker pool starts a fixed number of goroutines that read jobs from a channel. See the <a href="/docs/pools">pool documentation</a> and the <a href="https://example.org/spec">spec</a> for details about <code>sync.WaitGroup</code>.</p>
<ul><li>Fan out the work</li><li>Fan in the results</li></ul>
<pre>for w := 0; w &lt; 3; w++ {
    go worker(jobs, results)
}</pre>
<p>Pipelines compose stages that each receive values from upstream and send values downstream, which keeps every stage small and testable.</p>
</article></main>
<aside>ASIDE-SHOULD-NOT-APPEAR</aside>
<footer>FOOTER-SHOULD-NOT-APPEAR &copy; Example</footer>
</body></html>`

func pageURL() *url.URL { u, _ := url.Parse("https://blog.example.com/posts/go"); return u }

func TestMarkdownModeUsesReadability(t *testing.T) {
	r, err := HTML([]byte(article), Options{Mode: ModeMarkdown, BaseURL: pageURL()})
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != "readability" {
		t.Fatalf("method = %q, want readability", r.Method)
	}
	if !strings.Contains(r.Title, "Go Concurrency Patterns") {
		t.Fatalf("title = %q", r.Title)
	}
	// Readability moves an <h1> that repeats the page title into Title.
	for _, want := range []string{"## Worker pools", "goroutines and channels", "- Fan out the work", "go worker(jobs, results)"} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("markdown missing %q\n---\n%s", want, r.Text)
		}
	}
	// Relative links are resolved against the page URL.
	if !strings.Contains(r.Text, "(https://blog.example.com/docs/pools)") {
		t.Errorf("relative link not resolved:\n%s", r.Text)
	}
	if !strings.Contains(r.Text, "(https://example.org/spec)") {
		t.Errorf("absolute link lost:\n%s", r.Text)
	}
	for _, bad := range []string{"SCRIPT-SHOULD-NOT-APPEAR", "NAV-SHOULD-NOT-APPEAR", "ASIDE-SHOULD-NOT-APPEAR", "FOOTER-SHOULD-NOT-APPEAR"} {
		if strings.Contains(r.Text, bad) {
			t.Errorf("page chrome leaked: %s", bad)
		}
	}
	if r.Sparse {
		t.Error("article flagged as sparse")
	}
}

func TestTextModeHasNoMarkdownSyntax(t *testing.T) {
	r, err := HTML([]byte(article), Options{Mode: ModeText, BaseURL: pageURL()})
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != "readability" {
		t.Fatalf("method = %q", r.Method)
	}
	if !strings.Contains(r.Text, "Concurrency in Go is built on goroutines") {
		t.Fatalf("body text missing:\n%s", r.Text)
	}
	if strings.Contains(r.Text, "](") || strings.Contains(r.Text, "## ") {
		t.Fatalf("markdown syntax in text mode:\n%s", r.Text)
	}
}

func TestFullModeKeepsWholePageButDropsChrome(t *testing.T) {
	r, err := HTML([]byte(article), Options{Mode: ModeFull, BaseURL: pageURL()})
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != "full-page" {
		t.Fatalf("method = %q, want full-page", r.Method)
	}
	if !strings.Contains(r.Text, "Example Blog") { // the <header> text is kept in full mode
		t.Errorf("header content missing in full mode:\n%s", r.Text)
	}
	for _, bad := range []string{"SCRIPT-SHOULD-NOT-APPEAR", "NAV-SHOULD-NOT-APPEAR", "ASIDE-SHOULD-NOT-APPEAR", "FOOTER-SHOULD-NOT-APPEAR", "color:red"} {
		if strings.Contains(r.Text, bad) {
			t.Errorf("chrome leaked in full mode: %s", bad)
		}
	}
}

func TestFallsBackToFullPageWhenNoArticle(t *testing.T) {
	page := `<html><head><title>Links</title></head><body><ul><li><a href="https://a.example/">Alpha</a></li><li><a href="https://b.example/">Beta</a></li></ul></body></html>`
	r, err := HTML([]byte(page), Options{Mode: ModeMarkdown, BaseURL: pageURL()})
	if err != nil {
		t.Fatal(err)
	}
	if r.Method != "full-page" {
		t.Fatalf("method = %q, want full-page fallback", r.Method)
	}
	if !strings.Contains(r.Text, "[Alpha](https://a.example/)") || !strings.Contains(r.Text, "[Beta](https://b.example/)") {
		t.Fatalf("links lost in fallback:\n%s", r.Text)
	}
	if r.Title != "Links" {
		t.Fatalf("title = %q", r.Title)
	}
}

func TestJavaScriptShellIsFlaggedSparse(t *testing.T) {
	page := `<html><head><title>App</title><script src="/bundle.js"></script></head><body><div id="root"></div><noscript>You need to enable JavaScript to run this app.</noscript></body></html>`
	r, err := HTML([]byte(page), Options{BaseURL: pageURL()})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Sparse {
		t.Fatalf("JS-only shell not flagged sparse; text = %q", r.Text)
	}
}

func TestMaxCharsCapsOutput(t *testing.T) {
	r, err := HTML([]byte(article), Options{Mode: ModeFull, BaseURL: pageURL(), MaxChars: 120})
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(r.Text)); n > 120 {
		t.Fatalf("output is %d characters, cap is 120", n)
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": ModeMarkdown, "markdown": ModeMarkdown, " TEXT ": ModeText, "full": ModeFull} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseMode("main"); err == nil {
		t.Error("unknown mode accepted")
	}
}

func TestDangerousLinkSchemesAreNotRendered(t *testing.T) {
	page := strings.Replace(article, `<a href="/docs/pools">pool documentation</a>`, `<a href="javascript:alert(1)">evil link</a>`, 1)
	r, err := HTML([]byte(page), Options{Mode: ModeMarkdown, BaseURL: pageURL()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Text, "javascript:") {
		t.Fatalf("javascript: URL survived:\n%s", r.Text)
	}
}

// SPDX-License-Identifier: GPL-3.0-or-later

package extract

import (
	"net/url"
	"strings"
	"testing"
)

// A journal-style page: in-page anchors for authors and citations, a
// self-link, a relative link that needs the page's directory, inline images
// and literal "<" / ">" characters in the text.
const journal = `<!doctype html><html><head><title>Air quality and health</title></head><body>
<main><article>
<h1>Air quality and health</h1>
<ul class="authors"><li><a href="#">Ada Author,</a></li><li><a href="">Bob Writer,</a></li><li><a href="#aff1">Cy Third</a></li></ul>
<p>Fine particles penetrate deep into the lungs and bloodstream, exacerbating respiratory disease [<a href="#ref001">1</a>,<a href="#ref002">2</a>]. The association was strong (p < 0.001) and the effect size was large (OR > 3).</p>
<p>See the <a href="figs/fig1.html">first figure</a>, the <a href="/about">journal page</a>, this <a href="https://journals.example.org/vol/1/article?id=5#sec2">section of this article</a> and an <a href="https://doi.org/10.1/abc">external study</a>. Children and young people reported itchy eyes, rashes and low mood on high pollution days.</p>
<p><img src="data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==" alt="pixel"><img src="figs/chart.png"><img src="figs/map.png" alt="Map of monitoring stations"></p>
<p>Participants were recruited online during two windows, one with high pollution and one with good air quality, and answered the same questions both times.</p>
</article></main></body></html>`

func journalURL() *url.URL {
	u, _ := url.Parse("https://journals.example.org/vol/1/article?id=5")
	return u
}

func TestLinksAreCleanedAndResolved(t *testing.T) {
	for _, mode := range []Mode{ModeMarkdown, ModeFull} {
		t.Run(string(mode), func(t *testing.T) {
			r, err := HTML([]byte(journal), Options{Mode: mode, BaseURL: journalURL()})
			if err != nil {
				t.Fatal(err)
			}
			txt := r.Text
			// In-page anchors, empty links and self-links carry no information:
			// their text stays, the link goes.
			for _, bad := range []string{
				"](https://journals.example.org)", "](https://journals.example.org#", "](#",
				"](https://journals.example.org/vol/1/article?id=5", "]()",
			} {
				if strings.Contains(txt, bad) {
					t.Errorf("useless link %q survived:\n%s", bad, txt)
				}
			}
			keep := []string{"section of this article"}
			if mode == ModeFull {
				// Readability drops the link-only author list itself; full mode keeps it.
				keep = append(keep, "Ada Author", "Bob Writer", "Cy Third")
			}
			for _, want := range keep {
				if !strings.Contains(txt, want) {
					t.Errorf("link text %q lost:\n%s", want, txt)
				}
			}
			// Relative links resolve against the page's full URL, not just its host.
			for _, want := range []string{
				"(https://journals.example.org/vol/1/figs/fig1.html)",
				"(https://journals.example.org/about)",
				"(https://doi.org/10.1/abc)",
				"(https://journals.example.org/vol/1/figs/map.png)",
			} {
				if !strings.Contains(txt, want) {
					t.Errorf("missing resolved link %s:\n%s", want, txt)
				}
			}
			// Inline data URIs and images without alt text are noise.
			if strings.Contains(txt, "data:image") || strings.Contains(txt, "chart.png") {
				t.Errorf("data URI or alt-less image kept:\n%s", txt)
			}
			// Literal "<" and ">" from the page come out as themselves.
			if strings.Contains(txt, "&lt;") || strings.Contains(txt, "&gt;") {
				t.Errorf("HTML entities in output:\n%s", txt)
			}
			if !strings.Contains(txt, "p < 0.001") || !strings.Contains(txt, "OR > 3") {
				t.Errorf("comparison text lost:\n%s", txt)
			}
		})
	}
}

func TestTextModeHasNoEntities(t *testing.T) {
	r, err := HTML([]byte(journal), Options{Mode: ModeText, BaseURL: journalURL()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Text, "&lt;") || !strings.Contains(r.Text, "p < 0.001") {
		t.Fatalf("text mode:\n%s", r.Text)
	}
}

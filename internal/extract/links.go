// SPDX-License-Identifier: GPL-3.0-or-later

package extract

import (
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// cleanLinks prepares a DOM for Markdown conversion so links and images carry
// information instead of noise:
//
//   - links that go nowhere useful (empty, "#", in-page "#ref12" anchors,
//     javascript:, links back to the page itself) keep their text and lose the
//     link, so a citation marker comes out as "[1]" instead of
//     "[1](https://site#ref12)";
//   - every other link and image URL is resolved against the page's full URL
//     (the converter's own resolution only knows scheme and host, which breaks
//     relative links on pages below the site root);
//   - images with inline data: URIs or without alt text are dropped.
func cleanLinks(root *html.Node, page *url.URL) {
	if root == nil {
		return
	}
	self := stripFragment(page)
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		// Collect first: unwrapping and removing change the sibling list.
		var kids []*html.Node
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			kids = append(kids, c)
		}
		for _, c := range kids {
			if c.Type == html.ElementNode {
				switch c.DataAtom {
				case atom.A:
					if keep := cleanAnchor(c, page, self); !keep {
						walk(c)
						unwrap(c)
						continue
					}
				case atom.Img:
					if !cleanImage(c, page) {
						n.RemoveChild(c)
						continue
					}
				}
			}
			walk(c)
		}
	}
	walk(root)
}

// cleanAnchor rewrites a link's href to an absolute URL and reports whether
// the link is worth keeping.
func cleanAnchor(a *html.Node, page, self *url.URL) bool {
	href := strings.TrimSpace(attrVal(a, "href"))
	if href == "" || strings.HasPrefix(href, "#") {
		return false
	}
	u, err := page.Parse(href)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if self != nil && stripFragment(u).String() == self.String() {
			return false // a link back to this very page
		}
	case "mailto", "tel":
	default:
		return false // javascript:, data:, vbscript:, ...
	}
	setAttr(a, "href", u.String())
	return true
}

// cleanImage rewrites an image's src to an absolute URL and reports whether
// the image is worth keeping.
func cleanImage(img *html.Node, page *url.URL) bool {
	src := strings.TrimSpace(attrVal(img, "src"))
	if src == "" || strings.HasPrefix(strings.ToLower(src), "data:") {
		return false
	}
	if strings.TrimSpace(attrVal(img, "alt")) == "" {
		return false
	}
	u, err := page.Parse(src)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	setAttr(img, "src", u.String())
	return true
}

// unwrap replaces n with its children.
func unwrap(n *html.Node) {
	parent := n.Parent
	if parent == nil {
		return
	}
	for n.FirstChild != nil {
		c := n.FirstChild
		n.RemoveChild(c)
		parent.InsertBefore(c, n)
	}
	parent.RemoveChild(n)
}

func stripFragment(u *url.URL) *url.URL {
	if u == nil || u.Host == "" {
		return nil
	}
	c := *u
	c.Fragment, c.RawFragment = "", ""
	return &c
}

func attrVal(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func setAttr(n *html.Node, key, val string) {
	for i := range n.Attr {
		if n.Attr[i].Key == key {
			n.Attr[i].Val = val
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: key, Val: val})
}

// unescapeMarkdown turns the converter's "&lt;" and "&gt;" back into the
// literal characters the page showed. The converter always escapes them, which
// leaves text such as "p &lt; 0.001" that a model has to decode.
var entityRestorer = strings.NewReplacer("&lt;", "<", "&gt;", ">")

func unescapeMarkdown(s string) string { return entityRestorer.Replace(s) }

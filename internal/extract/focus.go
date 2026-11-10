// SPDX-License-Identifier: GPL-3.0-or-later

package extract

import (
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Passage is a stretch of a page returned by Focus.
type Passage struct {
	Offset  int    // character (rune) offset in the page text; usable as start_index
	Section string // heading path, e.g. "Results > Health outcomes"
	Text    string
	Score   float64
}

// FocusResult is the outcome of Focus.
type FocusResult struct {
	Passages []Passage // best matches, in document order, within the budget
	Total    int       // passages the page was split into
	Matched  int       // passages with any query term
}

const (
	passageTarget = 700  // merge small blocks up to about this many characters
	passageMax    = 1100 // split blocks larger than this
	maxPassages   = 12
)

// Focus returns the passages of text that best match query, using BM25
// ranking over paragraph-sized passages. It is plain keyword matching: no
// model, no embeddings, nothing leaves the process. Words with a different
// form or a synonym on the page will not match.
//
// budget caps the total characters returned. Passages come back in document
// order so they read naturally and their offsets can be used to page.
func Focus(text, query string, budget int) FocusResult {
	passages := splitPassages(text)
	res := FocusResult{Total: len(passages)}
	qTerms := queryTerms(query)
	if len(passages) == 0 || len(qTerms) == 0 {
		return res
	}

	// Per-passage term frequencies and document frequencies.
	tfs := make([]map[string]int, len(passages))
	secTerms := make([]map[string]bool, len(passages))
	df := map[string]int{}
	var totalLen int
	for i, p := range passages {
		toks := tokenize(p.Text)
		totalLen += len(toks)
		tf := map[string]int{}
		for _, t := range toks {
			tf[t]++
		}
		tfs[i] = tf
		for t := range tf {
			df[t]++
		}
		secTerms[i] = map[string]bool{}
		for _, t := range tokenize(p.Section) {
			secTerms[i][t] = true
		}
	}
	n := float64(len(passages))
	avg := math.Max(float64(totalLen)/n, 1)
	const k1, b = 1.2, 0.75

	for i := range passages {
		dl := 0
		for _, c := range tfs[i] {
			dl += c
		}
		var score float64
		for _, q := range qTerms {
			idf := math.Log(1 + (n-float64(df[q])+0.5)/(float64(df[q])+0.5))
			if tf := float64(tfs[i][q]); tf > 0 {
				score += idf * tf * (k1 + 1) / (tf + k1*(1-b+b*float64(dl)/avg))
			}
			if secTerms[i][q] {
				score += 0.5 * idf // the heading names what the user asked about
			}
		}
		passages[i].Score = score
		if score > 0 {
			res.Matched++
		}
	}

	order := make([]int, 0, len(passages))
	for i, p := range passages {
		if p.Score > 0 {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, c int) bool { return passages[order[a]].Score > passages[order[c]].Score })

	used := 0
	var picked []Passage
	for _, i := range order {
		if len(picked) >= maxPassages {
			break
		}
		p := passages[i]
		size := utf8.RuneCountInString(p.Text)
		if used+size > budget {
			if len(picked) > 0 {
				continue // try smaller passages that still fit
			}
			p.Text = string([]rune(p.Text)[:max(budget, 1)]) // budget smaller than one passage
			size = utf8.RuneCountInString(p.Text)
		}
		picked = append(picked, p)
		used += size
	}
	sort.Slice(picked, func(a, c int) bool { return picked[a].Offset < picked[c].Offset })
	res.Passages = picked
	return res
}

// splitPassages cuts text into passages of about passageTarget characters:
// paragraphs are merged until they reach that size, never across a heading,
// and oversized blocks are split at line or word boundaries.
func splitPassages(text string) []Passage {
	runes := []rune(text)
	type block struct {
		start, end int // rune range
		heading    bool
	}
	var blocks []block
	// Paragraph blocks separated by blank lines.
	i := 0
	for i < len(runes) {
		for i < len(runes) && (runes[i] == '\n' || runes[i] == '\r') {
			i++
		}
		start := i
		for i < len(runes) {
			if runes[i] == '\n' && i+1 < len(runes) && (runes[i+1] == '\n' || (runes[i+1] == '\r' && i+2 < len(runes) && runes[i+2] == '\n')) {
				break
			}
			i++
		}
		if i > start {
			blocks = append(blocks, block{start: start, end: i})
		}
	}
	// Mark headings and split oversized blocks.
	var parts []block
	for _, bl := range blocks {
		first := string(runes[bl.start:min(bl.end, bl.start+8)])
		isHeading := strings.HasPrefix(first, "#")
		if isHeading {
			// A heading line may be followed directly by text in the same block.
			eol := bl.start
			for eol < bl.end && runes[eol] != '\n' {
				eol++
			}
			if _, _, ok := parseHeading(string(runes[bl.start:eol])); ok {
				parts = append(parts, block{start: bl.start, end: eol, heading: true})
				bl.start = eol
				for bl.start < bl.end && (runes[bl.start] == '\n' || runes[bl.start] == '\r') {
					bl.start++
				}
				if bl.start >= bl.end {
					continue
				}
			}
		}
		for bl.end-bl.start > passageMax {
			cut := splitPoint(runes, bl.start, bl.start+passageTarget+passageTarget/3)
			parts = append(parts, block{start: bl.start, end: cut})
			bl.start = cut
			for bl.start < bl.end && unicode.IsSpace(runes[bl.start]) {
				bl.start++
			}
		}
		if bl.end > bl.start {
			parts = append(parts, block{start: bl.start, end: bl.end})
		}
	}

	// Merge small neighbours; headings start a new passage and set the section.
	var out []Passage
	var stack []Section
	var cur *Passage
	var curStart, curEnd int
	section := func() string {
		names := make([]string, len(stack))
		for i, s := range stack {
			names[i] = s.Title
		}
		return strings.Join(names, " > ")
	}
	flush := func() {
		if cur != nil {
			cur.Text = strings.TrimSpace(string(runes[curStart:curEnd]))
			if cur.Text != "" {
				out = append(out, *cur)
			}
			cur = nil
		}
	}
	for _, p := range parts {
		if p.heading {
			flush()
			level, title, _ := parseHeading(string(runes[p.start:p.end]))
			for len(stack) > 0 && stack[len(stack)-1].Level >= level {
				stack = stack[:len(stack)-1]
			}
			stack = append(stack, Section{Level: level, Title: title})
			cur = &Passage{Offset: p.start, Section: section()}
			curStart, curEnd = p.start, p.end
			continue
		}
		if cur != nil && curEnd-curStart+(p.end-curEnd) > passageTarget && curEnd-curStart > passageTarget/4 {
			sec := cur.Section
			flush()
			cur = &Passage{Offset: p.start, Section: sec}
			curStart, curEnd = p.start, p.end
			continue
		}
		if cur == nil {
			cur = &Passage{Offset: p.start, Section: section()}
			curStart = p.start
		}
		curEnd = p.end
	}
	flush()
	return out
}

// splitPoint picks where to cut runes[from:limit]: the last line break, else
// sentence end, else space, in the second half of the window.
func splitPoint(runes []rune, from, limit int) int {
	limit = min(limit, len(runes))
	lo := from + (limit-from)/2
	for _, set := range []func(r rune, next rune) bool{
		func(r, _ rune) bool { return r == '\n' },
		func(r, next rune) bool {
			return (r == '.' || r == '!' || r == '?' || r == '。') && unicode.IsSpace(next)
		},
		func(r, _ rune) bool { return unicode.IsSpace(r) },
	} {
		for i := limit - 1; i >= lo; i-- {
			next := rune(' ')
			if i+1 < len(runes) {
				next = runes[i+1]
			}
			if set(runes[i], next) {
				return i + 1
			}
		}
	}
	return limit
}

// ------------------------------------------------------------- tokenising

var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an the of to in on at by for from with and or but is are was were be been it its this that these those as
		what which who whom how does do did about into than then there their`) {
		stopwords[w] = true
	}
}

func queryTerms(q string) []string {
	all := tokenize(q)
	var kept []string
	seen := map[string]bool{}
	for _, t := range all {
		if !stopwords[t] && !seen[t] {
			kept = append(kept, t)
			seen[t] = true
		}
	}
	if len(kept) == 0 { // a question made only of stopwords: use them
		for _, t := range all {
			if !seen[t] {
				kept = append(kept, t)
				seen[t] = true
			}
		}
	}
	return kept
}

// tokenize lowercases s and splits it into terms. Words and numbers (3.44,
// 69.3%) are single terms; scripts written without spaces (Chinese, Japanese,
// Korean, Thai) become character pairs so substrings still match.
func tokenize(s string) []string {
	var out []string
	runes := []rune(strings.ToLower(s))
	for i := 0; i < len(runes); {
		r := runes[i]
		switch {
		case isUnspaced(r):
			j := i
			for j < len(runes) && isUnspaced(runes[j]) {
				j++
			}
			seg := runes[i:j]
			if len(seg) == 1 {
				out = append(out, string(seg))
			}
			for k := 0; k+1 < len(seg); k++ {
				out = append(out, string(seg[k:k+2]))
			}
			i = j
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			j := i
			for j < len(runes) {
				c := runes[j]
				if (unicode.IsLetter(c) || unicode.IsMark(c)) && !isUnspaced(c) || unicode.IsDigit(c) {
					j++
					continue
				}
				// a '.', ',' or '-' inside a number or word: 3.44, 1,000, covid-19
				if (c == '.' || c == ',' || c == '-' || c == '\'') && j+1 < len(runes) && j > i &&
					(unicode.IsLetter(runes[j+1]) || unicode.IsDigit(runes[j+1])) && !isUnspaced(runes[j+1]) &&
					(c != '.' && c != ',' || unicode.IsDigit(runes[j-1]) && unicode.IsDigit(runes[j+1])) {
					j++
					continue
				}
				break
			}
			out = append(out, fold(string(runes[i:j])))
			i = j
		default:
			i++
		}
	}
	return out
}

func isUnspaced(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r) || unicode.Is(unicode.Thai, r)
}

// fold reduces simple English plurals so "eyes" matches "eye".
func fold(w string) string {
	if len(w) <= 3 || strings.ContainsAny(w, "0123456789") {
		return w
	}
	switch {
	case strings.HasSuffix(w, "ies") && len(w) > 4:
		return w[:len(w)-3] + "y"
	case strings.HasSuffix(w, "sses"), strings.HasSuffix(w, "shes"), strings.HasSuffix(w, "ches"), strings.HasSuffix(w, "xes"):
		return w[:len(w)-2]
	case strings.HasSuffix(w, "ss"), strings.HasSuffix(w, "us"), strings.HasSuffix(w, "is"):
		return w
	case strings.HasSuffix(w, "s"):
		return w[:len(w)-1]
	}
	return w
}

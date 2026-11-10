// SPDX-License-Identifier: GPL-3.0-or-later

package extract

import (
	"strings"
	"unicode/utf8"
)

// Section is one heading of an extracted page.
type Section struct {
	Offset int // character (rune) offset of the heading line in the text
	Level  int // 1 for "#", 2 for "##", ...
	Title  string
}

const (
	maxOutlineEntries = 40
	maxOutlineChars   = 2000
	maxOutlineTitle   = 100
)

// Outline lists the Markdown headings (levels 1 to 6) of text, in document
// order, with character offsets that can be passed back as start_index.
// Headings inside fenced code blocks are ignored. When a page has more
// headings than fit, the deepest levels are dropped first, so the outline
// always spans the whole page.
func Outline(text string) []Section {
	var all []Section
	offset := 0 // rune offset of the current line
	var fence string
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case fence != "":
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
			}
		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			fence = trimmed[:3]
		default:
			if level, title, ok := parseHeading(line); ok {
				all = append(all, Section{Offset: offset, Level: level, Title: title})
			}
		}
		offset += utf8.RuneCountInString(line)
	}
	return capOutline(all)
}

func parseHeading(line string) (level int, title string, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level < 1 || level > 6 || level >= len(line) || line[level] != ' ' {
		return 0, "", false
	}
	title = strings.Join(strings.Fields(strings.TrimRight(strings.TrimSpace(line[level:]), "# ")), " ")
	if title == "" {
		return 0, "", false
	}
	if utf8.RuneCountInString(title) > maxOutlineTitle {
		title = string([]rune(title)[:maxOutlineTitle]) + "…"
	}
	return level, title, true
}

// capOutline drops the deepest headings until the outline fits the limits.
func capOutline(all []Section) []Section {
	fits := func(s []Section) bool {
		chars := 0
		for _, x := range s {
			chars += utf8.RuneCountInString(x.Title) + 12
		}
		return len(s) <= maxOutlineEntries && chars <= maxOutlineChars
	}
	for depth := 6; !fits(all) && depth >= 1; depth-- {
		kept := all[:0:0]
		for _, s := range all {
			if s.Level < depth {
				kept = append(kept, s)
			}
		}
		if len(kept) == 0 { // even the top level alone is too long: truncate
			kept = all
			for !fits(kept) && len(kept) > 0 {
				kept = kept[:len(kept)-1]
			}
			return kept
		}
		all = kept
	}
	return all
}

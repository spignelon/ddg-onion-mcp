// SPDX-License-Identifier: GPL-3.0-or-later

package extract

import (
	"fmt"
	"strings"
	"testing"
)

// paper builds a long document: filler paragraphs, decoys that share one
// common word with the question, and one passage with the answer.
func paper() string {
	var b strings.Builder
	b.WriteString("# Real-time impacts of air pollution on young people\n\n")
	b.WriteString("## Abstract\n\nWe surveyed participants in two cities during a high-pollution window and a clean-air window.\n\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "## Background %d\n\nSection %d reviews prior literature on emissions, traffic, household fuels and seasonal weather patterns across the region. It describes monitoring networks and data quality in detail.\n\n", i, i)
	}
	b.WriteString("## Methods\n\n### Sampling\n\nThe ratio of eligible to enrolled participants was monitored daily and reviewed weekly by the study team.\n\n")
	b.WriteString("## Results\n\n### Health outcomes\n\nHigh pollution was associated with gastrointestinal and respiratory symptoms, particularly diarrhea or vomiting (OR = 4.55, p < 0.001), respiratory difficulties (OR = 3.50, p < 0.001), and itchy eyes (OR = 3.44, p < 0.001). Moderate increases were seen for skin rash.\n\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "## Discussion %d\n\nPoint %d considers policy options, cost, enforcement and public communication in other cities, including school closures and transport restrictions.\n\n", i, i)
	}
	return b.String()
}

func TestFocusFindsTheAnswerPassageAnywhereInALongPage(t *testing.T) {
	text := paper()
	for _, q := range []string{"itchy eyes odds ratio", "what is the odds ratio for itchy eyes?", "OR = 3.44", "3.44"} {
		res := Focus(text, q, 3000)
		if len(res.Passages) == 0 {
			t.Errorf("%q: no passages (total %d)", q, res.Total)
			continue
		}
		found := false
		for _, p := range res.Passages {
			if strings.Contains(p.Text, "itchy eyes (OR = 3.44") {
				found = true
				if !strings.Contains(p.Section, "Results") || !strings.Contains(p.Section, "Health outcomes") {
					t.Errorf("%q: section = %q, want Results > Health outcomes", q, p.Section)
				}
			}
		}
		if !found {
			t.Errorf("%q: answer passage not returned: %+v", q, res.Passages)
		}
	}
}

func TestFocusRanksPassagesMatchingMoreOfTheQuestionHigher(t *testing.T) {
	// Passages come back in document order, so check Score, not position.
	res := Focus(paper(), "itchy eyes odds ratio", 4000)
	var best Passage
	for _, p := range res.Passages {
		if p.Score > best.Score {
			best = p
		}
	}
	if !strings.Contains(best.Text, "itchy eyes") {
		t.Fatalf("top-scored passage is not the answer (decoy shares only \"ratio\"): %+v", res.Passages)
	}
	// With room for only one passage, the best one is the one that is kept.
	one := Focus(paper(), "itchy eyes odds ratio", 400)
	if len(one.Passages) != 1 || !strings.Contains(one.Passages[0].Text, "itchy eyes") {
		t.Fatalf("with a tight budget the best passage must win: %+v", one.Passages)
	}
}

func TestFocusOffsetsAreRuneOffsetsInDocumentOrder(t *testing.T) {
	text := "# Étude sur l'air\n\n" + paper() + "\n\n## 空气\n\n空气污染对儿童的影响。 itchy eyes again in children (OR = 3.44)."
	res := Focus(text, "itchy eyes", 4000)
	if len(res.Passages) < 2 {
		t.Fatalf("want at least 2 passages, got %d", len(res.Passages))
	}
	runes := []rune(text)
	prev := -1
	for _, p := range res.Passages {
		if p.Offset <= prev {
			t.Errorf("passages not in document order: %d after %d", p.Offset, prev)
		}
		prev = p.Offset
		got := string(runes[p.Offset : p.Offset+len([]rune(p.Text))])
		if got != p.Text {
			t.Errorf("offset %d does not point at the passage text", p.Offset)
		}
	}
}

func TestFocusRespectsBudgetAndDoesNotMergeAcrossHeadings(t *testing.T) {
	res := Focus(paper(), "section literature emissions policy", 1500)
	total := 0
	for _, p := range res.Passages {
		total += len([]rune(p.Text))
		if n := strings.Count(p.Text, "\n#"); n > 0 || (strings.HasPrefix(p.Text, "#") && strings.Count(p.Text, "\n## ") > 0) {
			t.Errorf("passage spans several headings:\n%s", p.Text)
		}
	}
	if total > 1500 {
		t.Errorf("returned %d characters, budget 1500", total)
	}
	if res.Matched < len(res.Passages) || res.Total < res.Matched {
		t.Errorf("counts inconsistent: %+v", res)
	}
}

func TestFocusNoMatch(t *testing.T) {
	res := Focus(paper(), "photosynthesis chloroplast", 2000)
	if len(res.Passages) != 0 || res.Total == 0 || res.Matched != 0 {
		t.Fatalf("got %+v", res)
	}
	if res := Focus(paper(), "   ", 2000); len(res.Passages) != 0 {
		t.Fatal("blank query returned passages")
	}
}

func TestFocusWorksForLanguagesWithoutSpaces(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&b, "## 第%d章\n\n这一章讨论交通、能源和城市规划的一般问题，没有涉及健康。\n\n", i)
	}
	b.WriteString("## 结果\n\n空气污染会影响儿童的肺部发育，并增加哮喘风险。\n\n")
	res := Focus(b.String(), "空气污染 儿童", 1000)
	if len(res.Passages) == 0 || !strings.Contains(res.Passages[0].Text, "空气污染会影响儿童") {
		t.Fatalf("Chinese query missed its passage: %+v", res.Passages)
	}
}

func TestFocusSplitsHugeBlocks(t *testing.T) {
	// A table or log dump with no blank lines must not become one giant passage.
	var b strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "| row %d | value %d | note |\n", i, i*7)
	}
	b.WriteString("| needle | 424242 | found |\n")
	res := Focus(b.String(), "needle 424242", 3000)
	if len(res.Passages) == 0 {
		t.Fatal("no passage for the needle row")
	}
	if n := len([]rune(res.Passages[0].Text)); n > 1500 {
		t.Fatalf("passage is %d characters; huge blocks must be split", n)
	}
}

func TestTokenizeKeepsNumbersAndFoldsPlurals(t *testing.T) {
	got := fmt.Sprint(tokenize("Itchy eyes (OR = 3.44, p < 0.001) and 69.3% rashes"))
	for _, want := range []string{"3.44", "0.001", "69.3", "itchy", "eye", "rash"} {
		if !strings.Contains(got, want) {
			t.Errorf("tokens %s missing %q", got, want)
		}
	}
}

func TestFocusKeepsWordsWithCombiningMarksTogether(t *testing.T) {
	// Hindi (Devanagari) and Arabic words contain combining vowel signs; they
	// must stay single terms, not be cut at every mark.
	for word, text := range map[string]string{
		"ताजमहल":   "ताजमहल आगरा में है",
		"القاهرَة": "القاهرَة عاصمة مصر",
	} {
		toks := tokenize(text)
		if len(toks) == 0 || toks[0] != word {
			t.Errorf("tokenize(%q)[0] = %q, want the whole word %q (tokens: %q)", text, first(toks), word, toks)
		}
	}
	var b strings.Builder
	for i := 0; i < 25; i++ {
		fmt.Fprintf(&b, "## अध्याय %d\n\nयह अध्याय सामान्य जानकारी देता है।\n\n", i)
	}
	b.WriteString("## इतिहास\n\nताजमहल का निर्माण शाहजहाँ ने करवाया।\n\n")
	res := Focus(b.String(), "ताजमहल शाहजहाँ", 800)
	if len(res.Passages) == 0 || !strings.Contains(res.Passages[0].Text, "शाहजहाँ") {
		t.Fatalf("Hindi query missed its passage: %+v", res.Passages)
	}
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

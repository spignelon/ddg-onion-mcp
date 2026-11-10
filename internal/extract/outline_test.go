// SPDX-License-Identifier: GPL-3.0-or-later

package extract

import (
	"fmt"
	"strings"
	"testing"
)

func TestOutlineOffsetsAreRuneOffsets(t *testing.T) {
	// Non-ASCII text before a heading: offsets must count characters, not bytes,
	// because start_index is in characters.
	text := "Intro éè中文 text.\n\n## Methods\n\nbody\n\n### Survey design\n\nmore\n\n## Results\n\nfound it"
	got := Outline(text)
	if len(got) != 3 {
		t.Fatalf("got %d sections: %+v", len(got), got)
	}
	runes := []rune(text)
	for _, s := range got {
		if !strings.HasPrefix(string(runes[s.Offset:]), strings.Repeat("#", s.Level)+" "+s.Title) {
			t.Errorf("offset %d for %q does not point at its heading: %q", s.Offset, s.Title, string(runes[s.Offset:s.Offset+15]))
		}
	}
	if got[0].Title != "Methods" || got[0].Level != 2 || got[1].Title != "Survey design" || got[1].Level != 3 || got[2].Title != "Results" {
		t.Errorf("unexpected sections: %+v", got)
	}
}

func TestOutlineIgnoresCodeFencesAndNonHeadings(t *testing.T) {
	text := "# Real\n\n```sh\n# a shell comment, not a heading\n## also not\n```\n\n~~~\n# tilde fence\n~~~\n\n#hashtag no space\n\n####### seven hashes\n\n## Second\n"
	got := Outline(text)
	titles := []string{}
	for _, s := range got {
		titles = append(titles, s.Title)
	}
	if fmt.Sprint(titles) != "[Real Second]" {
		t.Fatalf("titles = %v", titles)
	}
}

func TestOutlineIsCappedKeepingTopLevels(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&b, "## Chapter %d\n\ntext\n\n", i)
		for j := 0; j < 5; j++ {
			fmt.Fprintf(&b, "#### Detail %d.%d\n\ntext\n\n", i, j)
		}
	}
	got := Outline(b.String())
	if len(got) > maxOutlineEntries {
		t.Fatalf("%d entries, cap is %d", len(got), maxOutlineEntries)
	}
	chapters := 0
	for _, s := range got {
		if s.Level == 2 {
			chapters++
		}
	}
	if chapters != 30 {
		t.Errorf("only %d of 30 top-level chapters kept: deeper levels must be dropped first", chapters)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Offset <= got[i-1].Offset {
			t.Fatal("outline not in document order")
		}
	}
}

func TestOutlineEmptyForPlainText(t *testing.T) {
	if got := Outline("just some plain text\nwith no headings"); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestOutlineTruncatesLongTitles(t *testing.T) {
	got := Outline("## " + strings.Repeat("word ", 100))
	if len(got) != 1 || len([]rune(got[0].Title)) > maxOutlineTitle+1 {
		t.Fatalf("got %+v", got)
	}
}

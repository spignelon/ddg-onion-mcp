// SPDX-License-Identifier: GPL-3.0-or-later

package mcpserver

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/spignelon/ddg-onion-mcp/internal/ddg"
	"github.com/spignelon/ddg-onion-mcp/internal/extract"
	"github.com/spignelon/ddg-onion-mcp/internal/fetch"
	"github.com/spignelon/ddg-onion-mcp/internal/limits"
)

type stubSearch struct {
	results []ddg.Result
	err     error
	gotOpts ddg.Options
	gotMax  int
}

func (s *stubSearch) Search(_ context.Context, _ string, max int, o ddg.Options) ([]ddg.Result, error) {
	s.gotOpts, s.gotMax = o, max
	return s.results, s.err
}

type stubFetch struct {
	page  *fetch.Page
	err   error
	calls atomic.Int32
	mode  extract.Mode
}

func (s *stubFetch) Get(_ context.Context, _ string, o extract.Options) (*fetch.Page, error) {
	s.calls.Add(1)
	s.mode = o.Mode
	return s.page, s.err
}

func connect(t *testing.T, cfg Config) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	srv := New(cfg)
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("protocol error calling %s: %v", name, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String(), res.IsError
}

func TestToolsAreAdvertised(t *testing.T) {
	cs := connect(t, Config{Search: &stubSearch{}, Fetch: &stubFetch{}})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		got[tool.Name] = tool
	}
	for _, name := range []string{"search", "fetch_content"} {
		tool := got[name]
		if tool == nil {
			t.Fatalf("tool %q missing; have %v", name, got)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || tool.Annotations.OpenWorldHint == nil || !*tool.Annotations.OpenWorldHint {
			t.Errorf("%s annotations = %+v", name, tool.Annotations)
		}
		if !strings.Contains(strings.ToLower(tool.Description), "untrusted") {
			t.Errorf("%s description lacks the untrusted-content warning", name)
		}
	}
	if len(res.Tools) != 2 {
		t.Errorf("%d tools advertised, want 2", len(res.Tools))
	}
}

func TestSearchFormatsResultsAndPassesOptions(t *testing.T) {
	ss := &stubSearch{results: []ddg.Result{
		{Position: 1, Title: "First", URL: "https://a.example/", Snippet: "alpha"},
		{Position: 2, Title: "Second", URL: "https://b.example/"},
	}}
	cs := connect(t, Config{Search: ss, Fetch: &stubFetch{}, DefaultRegion: "de-de", DefaultSafeSearch: "strict"})
	out, isErr := call(t, cs, "search", map[string]any{"query": "hello", "max_results": 7, "time_range": "w"})
	if isErr {
		t.Fatalf("tool error: %s", out)
	}
	for _, want := range []string{"Found 2 search results:", "1. First", "URL: https://a.example/", "Summary: alpha", "2. Second", "URL: https://b.example/"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if ss.gotMax != 7 || ss.gotOpts.Region != "de-de" || ss.gotOpts.SafeSearch != "strict" || ss.gotOpts.TimeRange != "w" {
		t.Errorf("search got max=%d opts=%+v", ss.gotMax, ss.gotOpts)
	}
}

func TestSearchErrors(t *testing.T) {
	cs := connect(t, Config{Search: &stubSearch{err: &ddg.BlockedError{Status: 406, Detail: "rejected"}}, Fetch: &stubFetch{}})
	out, isErr := call(t, cs, "search", map[string]any{"query": "x"})
	if !isErr || !strings.Contains(out, "406") || !strings.Contains(out, "do not retry in a loop") {
		t.Errorf("blocked search: isErr=%v out=%q", isErr, out)
	}

	cs = connect(t, Config{Search: &stubSearch{}, Fetch: &stubFetch{}})
	for name, args := range map[string]map[string]any{
		"empty query":     {"query": "  "},
		"too many":        {"query": "x", "max_results": 99},
		"negative":        {"query": "x", "max_results": -1},
		"bad region":      {"query": "x", "region": "mars"},
		"bad safe search": {"query": "x", "safe_search": "sorta"},
		"bad time range":  {"query": "x", "time_range": "decade"},
	} {
		if out, isErr := call(t, cs, "search", args); !isErr {
			t.Errorf("%s accepted: %s", name, out)
		}
	}
	if res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "search", Arguments: map[string]any{}}); err == nil && !res.IsError {
		t.Error("missing required query accepted")
	}
}

func TestEmptyResults(t *testing.T) {
	cs := connect(t, Config{Search: &stubSearch{}, Fetch: &stubFetch{}})
	out, isErr := call(t, cs, "search", map[string]any{"query": "nothing"})
	if isErr || !strings.Contains(out, "No results") {
		t.Errorf("isErr=%v out=%q", isErr, out)
	}
}

func longPage() *fetch.Page {
	return &fetch.Page{URL: "https://site.example/a", Title: "A Page", Text: strings.Repeat("0123456789", 100), Method: "readability"}
}

func TestFetchPaginatesAndCaches(t *testing.T) {
	sf := &stubFetch{page: longPage()}
	cs := connect(t, Config{Search: &stubSearch{}, Fetch: sf, Cache: limits.NewCache[*fetch.Page](time.Minute, 8)})

	out, isErr := call(t, cs, "fetch_content", map[string]any{"url": "https://site.example/a", "max_length": 300})
	if isErr {
		t.Fatalf("tool error: %s", out)
	}
	for _, want := range []string{"Title: A Page", "URL: https://site.example/a", "Showing characters 0-300 of 1000 total", "Use start_index=300 to see more", "source=readability", "cache=miss"} {
		if !strings.Contains(out, want) {
			t.Errorf("page 1 missing %q:\n%s", want, out)
		}
	}

	out, isErr = call(t, cs, "fetch_content", map[string]any{"url": "https://site.example/a", "max_length": 300, "start_index": 900})
	if isErr || !strings.Contains(out, "Showing characters 900-1000 of 1000 total") || strings.Contains(out, "to see more") || !strings.Contains(out, "cache=hit") {
		t.Errorf("last page: isErr=%v\n%s", isErr, out)
	}
	if strings.Contains(out, "Title:") {
		t.Errorf("title repeated on a continuation page:\n%s", out)
	}
	if sf.calls.Load() != 1 {
		t.Errorf("page downloaded %d times, want 1 (second read should hit the cache)", sf.calls.Load())
	}

	out, isErr = call(t, cs, "fetch_content", map[string]any{"url": "https://site.example/a", "start_index": 5000})
	if !strings.Contains(out, "past the end") {
		t.Errorf("out-of-range start_index: isErr=%v %q", isErr, out)
	}
}

func TestFetchCacheKeyIncludesMode(t *testing.T) {
	sf := &stubFetch{page: longPage()}
	cs := connect(t, Config{Search: &stubSearch{}, Fetch: sf, Cache: limits.NewCache[*fetch.Page](time.Minute, 8)})
	call(t, cs, "fetch_content", map[string]any{"url": "https://site.example/a"})
	call(t, cs, "fetch_content", map[string]any{"url": "https://site.example/a", "parse_mode": "full"})
	if sf.calls.Load() != 2 || sf.mode != extract.ModeFull {
		t.Fatalf("calls=%d mode=%q; different parse modes must not share a cache entry", sf.calls.Load(), sf.mode)
	}
}

func TestFetchValidationNeverReachesTheFetcher(t *testing.T) {
	sf := &stubFetch{page: longPage()}
	cs := connect(t, Config{Search: &stubSearch{}, Fetch: sf})
	for name, args := range map[string]map[string]any{
		"loopback":       {"url": "http://127.0.0.1/admin"},
		"metadata":       {"url": "http://169.254.169.254/latest/meta-data"},
		"file scheme":    {"url": "file:///etc/passwd"},
		"bad mode":       {"url": "https://site.example/", "parse_mode": "pdf"},
		"small length":   {"url": "https://site.example/", "max_length": 5},
		"huge length":    {"url": "https://site.example/", "max_length": 10000000},
		"negative start": {"url": "https://site.example/", "start_index": -1},
	} {
		if out, isErr := call(t, cs, "fetch_content", args); !isErr {
			t.Errorf("%s accepted: %s", name, out)
		}
	}
	if sf.calls.Load() != 0 {
		t.Fatalf("fetcher called %d times for invalid input", sf.calls.Load())
	}
}

func TestFetchErrorsAndNotes(t *testing.T) {
	cs := connect(t, Config{Search: &stubSearch{}, Fetch: &stubFetch{err: errors.New("fetching x.example over Tor: cannot reach the Tor SOCKS proxy at 127.0.0.1:9050")}})
	out, isErr := call(t, cs, "fetch_content", map[string]any{"url": "https://x.example/"})
	if !isErr || !strings.Contains(out, "never connects without it") {
		t.Errorf("tor-down error: isErr=%v %q", isErr, out)
	}

	sparse := &fetch.Page{URL: "https://spa.example/", Text: "Loading…", Method: "full-page", Sparse: true, SourceTruncated: true}
	cs = connect(t, Config{Search: &stubSearch{}, Fetch: &stubFetch{page: sparse}})
	out, isErr = call(t, cs, "fetch_content", map[string]any{"url": "https://spa.example/"})
	if isErr || !strings.Contains(out, "JavaScript") || !strings.Contains(out, "cut off") {
		t.Errorf("notes missing: isErr=%v\n%s", isErr, out)
	}
}

func TestRateLimiterIsConsultedOnCacheMissOnly(t *testing.T) {
	sf := &stubFetch{page: longPage()}
	// One fetch per minute: the second, uncached request must wait and be cancelled.
	cs := connect(t, Config{
		Search: &stubSearch{}, Fetch: sf,
		FetchLimiter: limits.NewLimiter(1),
		Cache:        limits.NewCache[*fetch.Page](time.Minute, 8),
	})
	if out, isErr := call(t, cs, "fetch_content", map[string]any{"url": "https://site.example/a"}); isErr {
		t.Fatalf("first fetch failed: %s", out)
	}
	// Same URL again: served from cache without touching the limiter.
	if out, isErr := call(t, cs, "fetch_content", map[string]any{"url": "https://site.example/a", "start_index": 10}); isErr {
		t.Fatalf("cached read was rate limited: %s", out)
	}
	if sf.calls.Load() != 1 {
		t.Fatalf("fetch calls = %d", sf.calls.Load())
	}
}

// structuredPage is a long page with headings and one passage that answers a
// question buried far from the start.
func structuredPage() *fetch.Page {
	var b strings.Builder
	b.WriteString("## Abstract\n\nWe surveyed participants during high and low pollution.\n\n")
	for i := 0; i < 30; i++ {
		b.WriteString("## Background " + string(rune('A'+i%26)) + "\n\nGeneral literature review about emissions, traffic and weather, with no health outcomes.\n\n")
	}
	b.WriteString("## Results\n\nHigh pollution was linked to itchy eyes (OR = 3.44, p < 0.001) and low mood.\n\n")
	for i := 0; i < 30; i++ {
		b.WriteString("## Discussion " + string(rune('A'+i%26)) + "\n\nPolicy options, cost and enforcement in other cities.\n\n")
	}
	return &fetch.Page{URL: "https://journal.example/paper", Title: "A Paper", Text: b.String(), Method: "readability"}
}

func TestFetchFocusReturnsRelevantPassagesAndOutline(t *testing.T) {
	sf := &stubFetch{page: structuredPage()}
	cs := connect(t, Config{Search: &stubSearch{}, Fetch: sf, Cache: limits.NewCache[*fetch.Page](time.Minute, 8)})

	out, isErr := call(t, cs, "fetch_content", map[string]any{"url": "https://journal.example/paper", "focus": "itchy eyes odds ratio"})
	if isErr {
		t.Fatalf("tool error: %s", out)
	}
	for _, want := range []string{
		"Title: A Paper", "URL: https://journal.example/paper",
		`passages for "itchy eyes odds ratio"`, "[at ", "section: Results", "itchy eyes (OR = 3.44",
		"Sections (use start_index to jump)", "Abstract", "Results",
		"characters total", "of ", " passages matched", "cache=miss",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("focused output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Showing characters") {
		t.Errorf("focused output should not look like a plain page:\n%s", out)
	}

	// A second call with different words reuses the cached page.
	out, _ = call(t, cs, "fetch_content", map[string]any{"url": "https://journal.example/paper", "focus": "policy cost enforcement"})
	if !strings.Contains(out, "cache=hit") || sf.calls.Load() != 1 {
		t.Errorf("focused read should hit the cache (fetcher calls = %d):\n%s", sf.calls.Load(), out)
	}
}

func TestFetchFocusNoMatchSaysSoAndKeepsTheOutline(t *testing.T) {
	cs := connect(t, Config{Search: &stubSearch{}, Fetch: &stubFetch{page: structuredPage()}})
	out, isErr := call(t, cs, "fetch_content", map[string]any{"url": "https://journal.example/paper", "focus": "photosynthesis chloroplast"})
	if isErr || !strings.Contains(out, "No passage") || !strings.Contains(out, "Sections (use start_index to jump)") {
		t.Fatalf("isErr=%v\n%s", isErr, out)
	}
}

func TestFetchFocusIsValidated(t *testing.T) {
	sf := &stubFetch{page: structuredPage()}
	cs := connect(t, Config{Search: &stubSearch{}, Fetch: sf})
	if out, isErr := call(t, cs, "fetch_content", map[string]any{"url": "https://journal.example/paper", "focus": strings.Repeat("x ", 200)}); !isErr || !strings.Contains(out, "focus") {
		t.Errorf("overlong focus accepted: isErr=%v %q", isErr, out)
	}
	if sf.calls.Load() != 0 {
		t.Errorf("fetcher called %d times for invalid input", sf.calls.Load())
	}
	// start_index is not an error alongside focus; it is simply not used.
	if out, isErr := call(t, cs, "fetch_content", map[string]any{"url": "https://journal.example/paper", "focus": "itchy eyes", "start_index": 500}); isErr || !strings.Contains(out, "itchy eyes (OR = 3.44") {
		t.Errorf("focus with start_index: isErr=%v\n%s", isErr, out)
	}
}

func TestFetchFirstPageOfLongStructuredPageGetsOutlineAndFocusHint(t *testing.T) {
	cs := connect(t, Config{Search: &stubSearch{}, Fetch: &stubFetch{page: structuredPage()}})
	out, _ := call(t, cs, "fetch_content", map[string]any{"url": "https://journal.example/paper", "max_length": 1000})
	for _, want := range []string{"Sections (use start_index to jump)", "Results", `focus="`, "Showing characters 0-1000"} {
		if !strings.Contains(out, want) {
			t.Errorf("first page missing %q:\n%s", want, out[max(0, len(out)-900):])
		}
	}
	// Continuation pages and short pages stay lean.
	out, _ = call(t, cs, "fetch_content", map[string]any{"url": "https://journal.example/paper", "max_length": 1000, "start_index": 1000})
	if strings.Contains(out, "Sections (use start_index") {
		t.Errorf("outline repeated on a continuation page")
	}
	short := connect(t, Config{Search: &stubSearch{}, Fetch: &stubFetch{page: &fetch.Page{URL: "https://s.example/", Text: "## Hi\n\nshort page", Method: "readability"}}})
	out, _ = call(t, short, "fetch_content", map[string]any{"url": "https://s.example/"})
	if strings.Contains(out, "Sections (use start_index") || strings.Contains(out, `focus="`) {
		t.Errorf("short page should have no outline or hint:\n%s", out)
	}
}

func TestFetchToolDescriptionMentionsFocus(t *testing.T) {
	cs := connect(t, Config{Search: &stubSearch{}, Fetch: &stubFetch{}})
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		if tl.Name == "fetch_content" && !strings.Contains(tl.Description, "focus") {
			t.Errorf("fetch_content description does not mention focus: %s", tl.Description)
		}
	}
}

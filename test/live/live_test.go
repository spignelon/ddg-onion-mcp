// SPDX-License-Identifier: GPL-3.0-or-later

//go:build live

// Package live holds tests that need a running Tor SOCKS proxy (default
// 127.0.0.1:9050, override with DDG_TOR_PROXY) and real network access.
// Run with: go test -tags live ./test/live -v
package live

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func proxy() string {
	if v := os.Getenv("DDG_TOR_PROXY"); v != "" {
		return v
	}
	return "127.0.0.1:9050"
}

func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ddg-onion-mcp")
	out, err := exec.Command("go", "build", "-o", bin, "../../cmd/ddg-onion-mcp").CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// session starts the real binary over stdio.
func session(t *testing.T, extraArgs ...string) *mcp.ClientSession {
	t.Helper()
	bin := buildBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	args := append([]string{"--tor-proxy", proxy()}, extraArgs...)
	// LIVE_WRAP lets an audit tool wrap the binary, e.g.
	// LIVE_WRAP="strace -f -e trace=network -o /tmp/trace.txt".
	if wrap := strings.Fields(os.Getenv("LIVE_WRAP")); len(wrap) > 0 {
		args = append(append(wrap[1:], bin), args...)
		bin = wrap[0]
	}
	cmd := exec.Command(bin, args...)
	cmd.Stderr = os.Stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "live-test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("starting the server (is Tor running on %s?): %v", proxy(), err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String(), res.IsError
}

// The binary's own startup check (Tor Project's API through the proxy) must
// pass; the session helper fails otherwise.
func TestStartupTorCheckPasses(t *testing.T) {
	cs := session(t)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 2 {
		t.Fatalf("tools = %v, err = %v", tools, err)
	}
}

func TestFetchContentOverTor(t *testing.T) {
	cs := session(t)
	out, isErr := call(t, cs, "fetch_content", map[string]any{"url": "https://example.com/"})
	if isErr {
		t.Fatalf("fetch failed: %s", out)
	}
	t.Logf("fetched:\n%s", out)
	if !strings.Contains(out, "Example Domain") {
		t.Fatalf("unexpected content:\n%s", out)
	}
}

func TestFetchBlocksPrivateTargets(t *testing.T) {
	cs := session(t)
	for _, u := range []string{"http://127.0.0.1:9050/", "http://169.254.169.254/latest/meta-data/", "http://localhost/"} {
		if out, isErr := call(t, cs, "fetch_content", map[string]any{"url": u}); !isErr {
			t.Errorf("%s was fetched: %s", u, out)
		}
	}
}

func TestSearchViaDuckDuckGoOnion(t *testing.T) {
	cs := session(t)
	out, isErr := call(t, cs, "search", map[string]any{"query": "tor project", "max_results": 5})
	t.Logf("isError=%v\n%s", isErr, out)
	if isErr {
		// DuckDuckGo may refuse automated clients. That is reported, not worked around.
		t.Skipf("search was refused or failed: %s", out)
	}
	if !strings.Contains(out, "search results") || !strings.Contains(out, "URL: http") {
		t.Fatalf("unexpected search output:\n%s", out)
	}
}

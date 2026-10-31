package chatgpt

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSameURL(t *testing.T) {
	for _, tc := range []struct {
		stored, expected string
		match            bool
	}{
		{"before [https://example.com/path](https://example.com/path) after", "before https://example.com/path after", true},
		{"[https://example.com/a](https://example.com/b)", "https://example.com/a", false},
		{"before [https://example.com/a](https://example.com/a) changed", "before https://example.com/a after", false},
		{`literal \* and [https://example.com](https://example.com)`, `literal * and https://example.com`, false},
	} {
		got := projectSourceURLLinks(tc.stored, tc.expected)
		if (normalizeRecoveryPrompt(got) == normalizeRecoveryPrompt(tc.expected)) != tc.match {
			t.Fatal("source URL projection lost full prompt binding", tc)
		}
	}
	t.Run("URL defusing preserves full prompt identity", func(t *testing.T) {
		for _, tc := range []struct {
			want, actual string
			equal        bool
		}{
			{"see <https://example.com> and [link](http://example.com)", "see < https://example.com> and [link]( http://example.com)", true},
			{"see <https://example.com>", "see <https://example.com>", true},
			{"see ( https://example.com)", "see (https://example.com)", true},
			{`{"$schema":"https://json-schema.org/draft/2020-12/schema"}`, `{"$schema":" https://json-schema.org/draft/2020-12/schema"}`, true},
			{"see [https://example.com]", "see [ https://example.com]", false},
			{"see <https://example.com> tail", "see < https://example.com>", false},
			{"see <https://example.com>", "draft see < https://example.com>", false},
			{"a b", "ab", false},
			{"a b", "a  b", false},
			{"a\nb", "a b", false},
			{"a\tb", "a b", false},
			{"x = 1", "x=1", false},
		} {
			want := autolinkSpaceRe.ReplaceAllString(tc.want, "$1$2")
			actual := autolinkSpaceRe.ReplaceAllString(tc.actual, "$1$2")
			if (want == actual) != tc.equal {
				t.Fatalf("identity comparison: %q / %q", tc.want, tc.actual)
			}
		}
	})
	t.Run("Lone carriage returns match renderer newlines", func(t *testing.T) {
		norm := normalizeRecoveryPrompt
		if norm("progress\n\r\x1b[Kdone") != "progress\n\n\x1b[Kdone" {
			t.Fatalf("lone CR not normalized: %q", norm("progress\n\r\x1b[Kdone"))
		}
		if norm("a\r\nb") != norm("a\nb") || norm("a\rb") != norm("a\nb") {
			t.Fatal("CR/CRLF/LF forms diverge")
		}
		if norm("a\rb") == "ab" || norm("a\n\rb") == norm("a\nb") || norm(`a\*b`) == norm(`a*b`) {
			t.Fatal("meaningful newline or literal backslash lost")
		}
	})
	same := [][2]string{
		{"https://chatgpt.com/c/abc", "https://chatgpt.com/c/abc"},
		{"https://chatgpt.com/c/abc/", "https://chatgpt.com/c/abc"},
		{"https://chatgpt.com/c/abc#frag", "https://chatgpt.com/c/abc"},
	}
	for _, s := range same {
		if !sameURL(s[0], s[1]) {
			t.Fatalf("expected same: %q %q", s[0], s[1])
		}
	}
	diff := [][2]string{
		{"https://chatgpt.com/c/abc", "https://chatgpt.com/c/def"},
		{"https://chatgpt.com/", "https://chatgpt.com/c/abc"},
		{"", "https://chatgpt.com/c/abc"},
	}
	for _, s := range diff {
		if sameURL(s[0], s[1]) {
			t.Fatalf("expected different: %q %q", s[0], s[1])
		}
	}
}

func TestRequestContextCancellationDoesNotCancelTab(t *testing.T) {
	t.Run("already canceled turn has explicit pre-submission evidence", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		c := New(0, t.TempDir(), "")
		_, err := c.AskThreadTurn(ctx, TurnOptions{Prompt: "next", URL: "https://chatgpt.com/c/existing"})
		if !errors.Is(err, ErrNotSubmitted) || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled before browser access lost stage/cause: %v", err)
		}
	})
	key := struct{}{}
	tab := context.WithValue(context.Background(), key, "CDP executor")
	request, cancel := context.WithCancel(context.Background())
	child, done := requestContext(request, tab)
	defer done()
	if child.Value(key) != "CDP executor" {
		t.Fatal("lost tab values")
	}
	cancel()
	select {
	case <-child.Done():
	case <-time.After(time.Second):
		t.Fatal("request cancellation did not propagate")
	}
	if tab.Err() != nil {
		t.Fatal("canceled persistent browser context")
	}
}

func TestMCPDoesNotAdoptDedicatedAdapterTab(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json/list" {
			fmt.Fprint(w, `[{"id":"adapter","type":"page","url":"https://chatgpt.com/c/adapter"},{"id":"mcp","type":"page","url":"https://chatgpt.com/c/mcp"}]`)
			return
		}
		if r.Method != "PUT" || r.URL.Path != "/json/new" {
			t.Error("unexpected request", r.Method, r.URL)
			w.WriteHeader(404)
			return
		}
		fmt.Fprint(w, `{"id":"new-adapter"}`)
	}))
	defer server.Close()
	port, _ := strconv.Atoi(server.URL[strings.LastIndex(server.URL, ":")+1:])
	profile := t.TempDir()
	if err := os.WriteFile(filepath.Join(profile, "openai-tab-id"), []byte("adapter"), 0600); err != nil {
		t.Fatal(err)
	}
	c := New(port, profile, "")
	id, err := c.findOrCreateTab()
	if err != nil || string(id) != "mcp" {
		t.Fatal("MCP adopted adapter tab", id, err)
	}
	c.DedicatedTab = true
	id, err = c.findOrCreateTab()
	if err != nil || string(id) != "new-adapter" {
		t.Fatal("adapter reused MCP tab", id, err)
	}
	b, err := os.ReadFile(filepath.Join(profile, "openai-tab-id"))
	if err != nil || string(b) != "new-adapter" {
		t.Fatal("private target not registered", err)
	}
}

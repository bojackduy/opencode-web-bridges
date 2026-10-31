package chatgpt

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestProjectURLCacheAndValidation(t *testing.T) {
	c := New(9222, t.TempDir(), "")
	path := filepath.Join(c.ProfileDir, "openai-project-url")
	valid := "https://chatgpt.com/g/g-p-abc123/project"
	for _, url := range []string{valid, "https://evil.test/g/g-p-abc/project", HomePage, valid + "?other=1"} {
		if err := os.WriteFile(path, []byte(url), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := c.ensureProject(context.Background(), nil)
		if url == valid {
			if err != nil || got != valid {
				t.Fatal(got, err)
			}
		} else if err == nil {
			t.Fatalf("invalid Project URL accepted: %s", url)
		}
	}
}

func TestThreadURLValidation(t *testing.T) {
	for _, url := range []string{"https://chatgpt.com/c/abc-123", "https://chatgpt.com/g/g-p-abc123/c/thread-123"} {
		if !ThreadPattern.MatchString(url) {
			t.Fatalf("valid thread rejected: %s", url)
		}
	}
	c := New(9222, t.TempDir(), "")
	for _, url := range []string{HomePage, "https://chatgpt.com/c/abc/../other", "https://evil.test/c/abc", "https://chatgpt.com/c/abc?redirect=evil", "https://chatgpt.com/g/g-p-abc/project"} {
		if _, _, err := c.AskThread(context.Background(), "must not submit", url); err == nil {
			t.Fatalf("invalid thread accepted: %s", url)
		}
	}
}

package meta

import (
	"errors"
	"testing"
)

func TestAnswerAdoptable(t *testing.T) {
	cases := []struct {
		name         string
		beforeCount  int
		beforeBytes  int
		count        int
		text         string
		hasCopy      bool
		wantAdopt    bool
	}{
		{"no copy button blocks", 1, 10, 5, "new answer text", false, false},
		{"blank never adopted", 1, 10, 5, "   ", true, false},
		{"stale same-size rejected", 2, 22, 2, "exactly-twenty-chars!!", true, false},
		{"new node adopted", 1, 10, 2, "fresh answer", true, true},
		{"grown text adopted", 2, 5, 2, "much longer answer now", true, true},
		{"shrunk text rejected", 2, 50, 2, "short", true, false},
		{"no nodes no text rejected", 0, 0, 0, "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := answerAdoptable(tc.beforeCount, tc.beforeBytes, tc.count, tc.text, tc.hasCopy)
			if got != tc.wantAdopt {
				t.Fatalf("answerAdoptable() = %v, want %v", got, tc.wantAdopt)
			}
		})
	}
}

func TestValidThreadURL(t *testing.T) {
	if !ValidThreadURL("https://www.meta.ai/c/abc123") {
		t.Fatal("want valid thread URL accepted")
	}
	for _, bad := range []string{"", "https://gemini.google.com/app/xyz", "http://www.meta.ai/c/x"} {
		if ValidThreadURL(bad) {
			t.Fatalf("want %q rejected", bad)
		}
	}
}

func TestIsPreSubmit(t *testing.T) {
	if !IsPreSubmit(ErrNoComposer) {
		t.Fatal("want ErrNoComposer pre-submit")
	}
	if !IsPreSubmit(ErrNotLoggedIn) {
		t.Fatal("want ErrNotLoggedIn pre-submit")
	}
	if IsPreSubmit(ErrNotSubmitted) {
		t.Fatal("want ErrNotSubmitted NOT pre-submit (may have fired)")
	}
	if IsPreSubmit(errors.New("meta: no answer within timeout")) {
		t.Fatal("want timeout NOT pre-submit")
	}
}

func TestIsNavigateError(t *testing.T) {
	if !IsNavigateError(errors.New("navigate: bad gateway")) {
		t.Fatal("want navigate error detected")
	}
	if !IsNavigateError(errors.New("page did not settle after navigation")) {
		t.Fatal("want settle error detected")
	}
	if IsNavigateError(errors.New("meta: no composer")) {
		t.Fatal("want composer error NOT navigate")
	}
	if IsNavigateError(nil) {
		t.Fatal("want nil NOT navigate")
	}
}

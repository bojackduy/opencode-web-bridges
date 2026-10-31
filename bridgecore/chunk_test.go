package bridgecore

import (
	"strings"
	"testing"
)

func TestSplitTextNewlineFallback(t *testing.T) {
	text := strings.Repeat("a", 5) + "\n" + strings.Repeat("b", 5)
	got := SplitText(text, 7)
	if len(got) != 2 || got[0] != "aaaaa" {
		t.Fatalf("unexpected split: %+v", got)
	}
}

func TestChunkTextsGroups(t *testing.T) {
	texts := []string{"hello", "world"}
	groups, ends := ChunkTexts(texts, 100)
	if len(groups) != 1 || ends[0] != 2 {
		t.Fatalf("unexpected: groups=%d ends=%v", len(groups), ends)
	}
}

func TestPasteCap(t *testing.T) {
	if PasteCap(10, 5, true) != 10 {
		t.Fatal("replay")
	}
	if PasteCap(10, 5, false) != 15 {
		t.Fatal("final")
	}
}

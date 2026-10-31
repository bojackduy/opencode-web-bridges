package bridgecore

import "strings"

// SplitText splits text into chunks no larger than limit, preferring the last
// newline before the cut; falls back to a hard cut when no newline exists.
func SplitText(text string, limit int) []string {
	if limit <= 0 || len(text) <= limit {
		return []string{text}
	}
	var out []string
	for len(text) > limit {
		cut := strings.LastIndex(text[:limit], "\n")
		if cut <= 0 {
			cut = limit
		}
		out = append(out, text[:cut])
		text = text[cut:]
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

// ChunkTexts groups texts into chunks whose joined size stays within limit.
// Returns the groups and the index of the last input text absorbed by each.
func ChunkTexts(texts []string, limit int) (groups [][]string, ends []int) {
	var cur []string
	curLen := 0
	flush := func(end int) {
		if len(cur) > 0 {
			groups = append(groups, cur)
			ends = append(ends, end)
			cur, curLen = nil, 0
		}
	}
	for i, text := range texts {
		for _, seg := range SplitText(text, limit) {
			if len(cur) > 0 && curLen+len(seg)+1 > limit {
				flush(i)
			}
			cur = append(cur, seg)
			curLen += len(seg) + 1
		}
	}
	flush(len(texts))
	return groups, ends
}

// PasteCap is the effective ceiling for one turn: replay/import turns obey
// the raw budget, while the answer-seeking turn additionally carries the
// fixed teaching head, which must not count against the budget.
func PasteCap(limit, headBytes int, replay bool) int {
	if !replay {
		return limit + headBytes
	}
	return limit
}
